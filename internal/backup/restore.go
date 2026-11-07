package backup

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/floholz/ts-server-manager/internal/ts"
)

// ErrNotConfirmed is returned when the target is not a fresh server and
// the overwrite was not confirmed.
var ErrNotConfirmed = errors.New("restore: target is not a fresh server and the overwrite was not confirmed")

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// DryRun reads the target but sends no modifying command. The report
	// lists exactly what a real run would do.
	DryRun bool

	// Passwords maps a channel's ref ("c:5") or path ("Lobby/Gaming/Locked")
	// to its plaintext password.
	Passwords map[string]string

	// ConfirmOverwrite is called (in real runs only) when the target is not
	// a fresh server. Returning false aborts with ErrNotConfirmed before
	// anything is changed. A nil func aborts.
	ConfirmOverwrite func(TargetSummary) bool

	// Progress, if set, receives each action as it is performed.
	Progress func(msg string)

	// RandomPassword generates passwords for protected channels whose
	// password was not supplied. Defaults to 18 random bytes, base64url.
	RandomPassword func() (string, error)
}

// TargetSummary describes the target before anything is changed.
type TargetSummary struct {
	ServerName    string
	TSVersion     string
	ClientsOnline int
	// Fresh is true when the target matches a newly created virtual server.
	Fresh bool
	// What an overwrite removes. All existing channels are replaced (the
	// current default one last). Groups that also exist in the backup are
	// updated in place (keeping their members) and are not listed here.
	ChannelsDeleted      int
	ServerGroupsDeleted  []string
	ChannelGroupsDeleted []string
}

// Report is the outcome of a restore (or the plan, for a dry run).
type Report struct {
	DryRun  bool
	Target  TargetSummary
	Actions []string

	SkippedPerms []Skipped
	SkippedProps []Skipped
	Passwords    []PasswordOutcome
	// UnusedPasswords are supplied password keys that matched no channel.
	UnusedPasswords []string
}

// Skipped is a permission or property that could not be applied.
type Skipped struct {
	Target string // e.g. `server group "Moderator"`
	Name   string // permsid or property name
	Reason string
}

// PasswordOutcome says what happened to a password-protected channel.
type PasswordOutcome struct {
	Channel string // path
	Outcome string
	// Password is set when a random password was generated, so the admin
	// can hand it out or change it.
	Password string `json:",omitempty"`
}

// Restore rebuilds f's setup on the server behind q. q must be a single
// session (ts.Conn, normally behind a ts.Pacer) with the target virtual
// server selected and serveradmin-level rights.
func Restore(q ts.Execer, f *File, opts RestoreOptions) (*Report, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	r := &restorer{
		q:      q,
		f:      f,
		opts:   opts,
		report: &Report{DryRun: opts.DryRun},
		sgIDs:  map[string]string{},
		cgIDs:  map[string]string{},
		chIDs:  map[string]string{},
	}
	if r.opts.Progress == nil {
		r.opts.Progress = func(string) {}
	}
	if r.opts.RandomPassword == nil {
		r.opts.RandomPassword = randomPassword
	}
	if err := r.run(); err != nil {
		return r.report, err
	}
	return r.report, nil
}

type restorer struct {
	q      ts.Execer
	f      *File
	opts   RestoreOptions
	report *Report

	t *targetState
	// sameServer is true when the target is the server the backup was
	// taken from; only then can password hashes be compared.
	sameServer bool

	// backup ref -> target ID (or a dry-run placeholder)
	sgIDs, cgIDs, chIDs map[string]string
}

type targetState struct {
	serverInfo  ts.Row
	sgs, cgs    []ts.Row // regular groups
	channels    []ts.Row
	knownPerms  map[string]bool
	bootstrapID string // current default channel; kept until the end

	sgPlan, cgPlan groupPlan
}

func (r *restorer) run() error {
	if err := r.inspect(); err != nil {
		return err
	}
	if !r.report.Target.Fresh && !r.opts.DryRun {
		if r.opts.ConfirmOverwrite == nil || !r.opts.ConfirmOverwrite(r.report.Target) {
			return ErrNotConfirmed
		}
	}

	steps := []func() error{
		r.wipe,
		r.restoreServerProps,
		func() error { return r.restoreGroups(serverGroupKind, r.f.ServerGroups, r.t.sgPlan, r.sgIDs) },
		func() error { return r.restoreGroups(channelGroupKind, r.f.ChannelGroups, r.t.cgPlan, r.cgIDs) },
		r.restoreDefaultGroups,
		r.restoreChannels,
		r.finishDefaultChannel,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	r.checkUnusedPasswords()
	return nil
}

// --- reading the target -------------------------------------------------

func (r *restorer) inspect() error {
	t := &targetState{}
	var err error
	ver, err := first(r.q, ts.NewCommand("version"))
	if err != nil {
		return err
	}
	if t.serverInfo, err = first(r.q, ts.NewCommand("serverinfo")); err != nil {
		return err
	}
	if t.sgs, err = r.regularGroups("servergrouplist"); err != nil {
		return err
	}
	if t.cgs, err = r.regularGroups("channelgrouplist"); err != nil {
		return err
	}
	if t.channels, err = r.q.Exec(ts.NewCommand("channellist").Option("-flags")); err != nil {
		return fmt.Errorf("channellist: %w", err)
	}
	perms, err := r.q.Exec(ts.NewCommand("permissionlist"))
	if err != nil {
		return fmt.Errorf("permissionlist: %w", err)
	}
	t.knownPerms = make(map[string]bool, len(perms))
	for _, p := range perms {
		if name := p["permname"]; name != "" {
			t.knownPerms[name] = true
		}
	}
	for _, c := range t.channels {
		if c["channel_flag_default"] == "1" {
			t.bootstrapID = c["cid"]
		}
	}
	if t.bootstrapID == "" {
		return errors.New("target has no default channel")
	}

	si := t.serverInfo
	t.sgPlan = planGroups(r.f.ServerGroups, t.sgs, "sgid",
		map[string]string{r.f.Server.DefaultServerGroup: si["virtualserver_default_server_group"]})
	t.cgPlan = planGroups(r.f.ChannelGroups, t.cgs, "cgid",
		map[string]string{
			r.f.Server.DefaultChannelGroup:      si["virtualserver_default_channel_group"],
			r.f.Server.DefaultChannelAdminGroup: si["virtualserver_default_channel_admin_group"],
		})
	// Never try to delete the target's current default groups: TS6 refuses.
	t.sgPlan.deletes = without(t.sgPlan.deletes, si["virtualserver_default_server_group"])
	t.cgPlan.deletes = without(t.cgPlan.deletes,
		si["virtualserver_default_channel_group"], si["virtualserver_default_channel_admin_group"])

	r.t = t
	r.sameServer = r.f.Source.ServerUID != "" && r.f.Source.ServerUID == si["virtualserver_unique_identifier"]
	r.report.Target = TargetSummary{
		ServerName:           si["virtualserver_name"],
		TSVersion:            ver["version"],
		ClientsOnline:        si.IntOr("virtualserver_clientsonline", 0) - si.IntOr("virtualserver_queryclientsonline", 0),
		Fresh:                isFresh(t),
		ChannelsDeleted:      len(t.channels),
		ServerGroupsDeleted:  groupNames(t.sgs, "sgid", t.sgPlan.deletes),
		ChannelGroupsDeleted: groupNames(t.cgs, "cgid", t.cgPlan.deletes),
	}
	return nil
}

func (r *restorer) regularGroups(listCmd string) ([]ts.Row, error) {
	rows, err := r.q.Exec(ts.NewCommand(listCmd))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", listCmd, err)
	}
	var out []ts.Row
	for _, g := range rows {
		if g["type"] == groupTypeRegular {
			out = append(out, g)
		}
	}
	return out, nil
}

// isFresh reports whether the target looks like a newly created virtual
// server: a single channel and only stock regular groups.
func isFresh(t *targetState) bool {
	if len(t.channels) != 1 {
		return false
	}
	for _, g := range t.sgs {
		if !stockServerGroups[g["name"]] {
			return false
		}
	}
	for _, g := range t.cgs {
		if !stockChannelGroups[g["name"]] {
			return false
		}
	}
	return len(t.sgs) <= len(stockServerGroups) && len(t.cgs) <= len(stockChannelGroups)
}

// groupPlan maps backup group refs onto existing target groups.
type groupPlan struct {
	matches map[string]string // backup ref -> existing target ID
	deletes []string          // target IDs not used by the backup
}

// planGroups matches backup groups to target groups: default pointers
// first, then by exact name (first unused). Unmatched target groups are
// deleted; unmatched backup groups are created. Updating matched groups in
// place keeps their members.
func planGroups(backup []Group, target []ts.Row, idKey string, defaults map[string]string) groupPlan {
	p := groupPlan{matches: map[string]string{}}
	used := map[string]bool{}
	exists := map[string]bool{}
	for _, g := range target {
		exists[g[idKey]] = true
	}
	refs := make([]string, 0, len(defaults))
	for ref := range defaults {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		id := defaults[ref]
		if _, done := p.matches[ref]; done || used[id] || !exists[id] {
			continue
		}
		p.matches[ref] = id
		used[id] = true
	}
	for _, bg := range backup {
		if _, done := p.matches[bg.Ref]; done {
			continue
		}
		for _, tg := range target {
			if id := tg[idKey]; !used[id] && tg["name"] == bg.Name {
				p.matches[bg.Ref] = id
				used[id] = true
				break
			}
		}
	}
	for _, tg := range target {
		if id := tg[idKey]; !used[id] {
			p.deletes = append(p.deletes, id)
		}
	}
	return p
}

// --- writes ---------------------------------------------------------------

func isPlaceholder(id string) bool { return strings.HasPrefix(id, "new(") }

// write performs (or, in a dry run, records) a modifying command.
func (r *restorer) write(desc string, cmd *ts.Command) ([]ts.Row, error) {
	r.report.Actions = append(r.report.Actions, desc)
	r.opts.Progress(desc)
	if r.opts.DryRun {
		return nil, nil
	}
	rows, err := r.q.Exec(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", desc, err)
	}
	return rows, nil
}

// create is write for commands that return a new ID under idKey.
func (r *restorer) create(desc string, cmd *ts.Command, idKey, ref string) (string, error) {
	rows, err := r.write(desc, cmd)
	if err != nil {
		return "", err
	}
	if r.opts.DryRun {
		return "new(" + ref + ")", nil
	}
	if len(rows) == 0 || rows[0][idKey] == "" {
		return "", fmt.Errorf("%s: no %s in response", desc, idKey)
	}
	return rows[0][idKey], nil
}

// --- steps ------------------------------------------------------------------

func (r *restorer) wipe() error {
	for _, id := range r.t.sgPlan.deletes {
		desc := fmt.Sprintf("delete server group %q (sgid %s): not in backup", nameOf(r.t.sgs, "sgid", id), id)
		if _, err := r.write(desc, ts.NewCommand("servergroupdel").Arg("sgid", id).Arg("force", true)); err != nil {
			return err
		}
	}
	for _, id := range r.t.cgPlan.deletes {
		desc := fmt.Sprintf("delete channel group %q (cgid %s): not in backup", nameOf(r.t.cgs, "cgid", id), id)
		if _, err := r.write(desc, ts.NewCommand("channelgroupdel").Arg("cgid", id).Arg("force", true)); err != nil {
			return err
		}
	}
	// Delete every channel except the current default. Deleting a channel
	// with force=1 removes its subtree, so only top-level channels and the
	// default channel's direct children need a command.
	boot := r.t.bootstrapID
	for _, c := range r.t.channels {
		cid, pid := c["cid"], c["pid"]
		if cid == boot || (pid != "0" && pid != boot) {
			continue
		}
		desc := fmt.Sprintf("delete channel %q (cid %s) and its subchannels", c["channel_name"], cid)
		if _, err := r.write(desc, ts.NewCommand("channeldelete").Arg("cid", cid).Arg("force", true)); err != nil {
			return err
		}
	}
	return nil
}

func (r *restorer) restoreServerProps() error {
	current := r.t.serverInfo
	cmd := ts.NewCommand("serveredit")
	var keys []string
	for _, k := range restorableServerProps {
		v, ok := r.f.Server.Properties[k]
		if !ok || current[k] == v {
			continue
		}
		cmd.Arg(k, v)
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil
	}
	desc := fmt.Sprintf("server settings: update %s", strings.Join(keys, ", "))
	_, err := r.write(desc, cmd)
	if err == nil || !ts.IsQueryError(err) {
		return err
	}
	// One property was rejected: apply them one by one to find it.
	for _, k := range keys {
		_, err := r.q.Exec(ts.NewCommand("serveredit").Arg(k, r.f.Server.Properties[k]))
		if err != nil {
			if !ts.IsQueryError(err) {
				return fmt.Errorf("serveredit %s: %w", k, err)
			}
			r.report.SkippedProps = append(r.report.SkippedProps, Skipped{Target: "server", Name: k, Reason: queryMsg(err)})
		}
	}
	return nil
}

type groupKind struct {
	label                      string // "server group"
	idKey, addCmd, renameCmd   string
	permList, permAdd, permDel string
	groupPerms                 bool
}

var (
	serverGroupKind = groupKind{
		label: "server group", idKey: "sgid", addCmd: "servergroupadd", renameCmd: "servergrouprename",
		permList: "servergrouppermlist", permAdd: "servergroupaddperm", permDel: "servergroupdelperm", groupPerms: true,
	}
	channelGroupKind = groupKind{
		label: "channel group", idKey: "cgid", addCmd: "channelgroupadd", renameCmd: "channelgrouprename",
		permList: "channelgrouppermlist", permAdd: "channelgroupaddperm", permDel: "channelgroupdelperm", groupPerms: true,
	}
	channelKind = groupKind{
		label: "channel", idKey: "cid",
		permList: "channelpermlist", permAdd: "channeladdperm", permDel: "channeldelperm",
	}
)

func (r *restorer) restoreGroups(k groupKind, groups []Group, plan groupPlan, ids map[string]string) error {
	existing := r.t.sgs
	if k.idKey == "cgid" {
		existing = r.t.cgs
	}
	for _, g := range groups {
		id, matched := plan.matches[g.Ref]
		if matched {
			if cur := nameOf(existing, k.idKey, id); cur != g.Name {
				desc := fmt.Sprintf("rename %s %q (%s %s) to %q", k.label, cur, k.idKey, id, g.Name)
				if _, err := r.write(desc, ts.NewCommand(k.renameCmd).Arg(k.idKey, id).Arg("name", g.Name)); err != nil {
					return err
				}
			}
		} else {
			var err error
			desc := fmt.Sprintf("create %s %q", k.label, g.Name)
			if id, err = r.create(desc, ts.NewCommand(k.addCmd).Arg("name", g.Name), k.idKey, g.Ref); err != nil {
				return err
			}
		}
		ids[g.Ref] = id
		if err := r.replacePerms(k, id, fmt.Sprintf("%s %q", k.label, g.Name), g.Perms); err != nil {
			return err
		}
	}
	return nil
}

func (r *restorer) restoreDefaultGroups() error {
	si := r.t.serverInfo
	cmd := ts.NewCommand("serveredit")
	var changes []string
	for _, d := range []struct{ key, id string }{
		{"virtualserver_default_server_group", r.sgIDs[r.f.Server.DefaultServerGroup]},
		{"virtualserver_default_channel_group", r.cgIDs[r.f.Server.DefaultChannelGroup]},
		{"virtualserver_default_channel_admin_group", r.cgIDs[r.f.Server.DefaultChannelAdminGroup]},
	} {
		if d.id != si[d.key] {
			cmd.Arg(d.key, d.id)
			changes = append(changes, d.key)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	_, err := r.write("server settings: point "+strings.Join(changes, ", ")+" at the restored groups", cmd)
	return err
}

// placeholderChannelName is the temporary name of the target's original
// default channel while the restored tree is built, so a restored channel
// with the same name (e.g. "Default Channel") doesn't collide with it.
const placeholderChannelName = "ts-server-manager: restore in progress"

func (r *restorer) restoreChannels() error {
	boot := r.t.bootstrapID
	desc := fmt.Sprintf("rename current default channel (cid %s) to a placeholder until the restored default exists", boot)
	if _, err := r.write(desc, ts.NewCommand("channeledit").Arg("cid", boot).Arg("channel_name", placeholderChannelName)); err != nil {
		return err
	}

	paths := channelPaths(r.f.Channels)
	prevSibling := map[*Channel]string{} // parent (nil = root) -> new ID of the last created child
	return Walk(r.f.Channels, func(ch, parent *Channel) error {
		path := paths[ch]
		cmd := ts.NewCommand("channelcreate").Arg("channel_name", ch.Name)
		if parent != nil {
			cmd.Arg("cpid", r.chIDs[parent.Ref])
		}
		order := prevSibling[parent]
		if order == "" {
			order = "0"
		}
		cmd.Arg("channel_order", order)
		for _, k := range restorableChannelProps {
			if v, ok := ch.Properties[k]; ok {
				cmd.Arg(k, v)
			}
		}
		if ch.Properties["channel_flag_permanent"] != "1" && ch.Properties["channel_flag_semi_permanent"] != "1" {
			cmd.Arg("channel_flag_permanent", 1) // temporary channels would vanish immediately
		}

		var pwNote *PasswordOutcome
		if ch.HasPassword {
			pw, supplied := r.lookupPassword(ch, path)
			note := PasswordOutcome{Channel: path}
			if supplied {
				note.Outcome = "password set as supplied"
			} else {
				var err error
				if pw, err = r.opts.RandomPassword(); err != nil {
					return fmt.Errorf("generate password for %s: %w", path, err)
				}
				note.Outcome = "no password supplied: set to a random password"
				note.Password = pw
			}
			cmd.Arg("channel_password", pw)
			pwNote = &note
		}

		id, err := r.create(fmt.Sprintf("create channel %q", path), cmd, "cid", ch.Ref)
		if err != nil {
			return err
		}
		r.chIDs[ch.Ref] = id
		prevSibling[parent] = id

		if pwNote != nil {
			if pwNote.Password == "" && !r.opts.DryRun {
				r.verifyPassword(ch, id, pwNote)
			}
			r.report.Passwords = append(r.report.Passwords, *pwNote)
		}
		return r.replacePerms(channelKind, id, fmt.Sprintf("channel %q", path), ch.Perms)
	})
}

func (r *restorer) finishDefaultChannel() error {
	var def *Channel
	_ = Walk(r.f.Channels, func(ch, _ *Channel) error {
		if ch.IsDefault() {
			def = ch
		}
		return nil
	})
	id := r.chIDs[def.Ref]
	if _, err := r.write(fmt.Sprintf("make %q the default channel", def.Name),
		ts.NewCommand("channeledit").Arg("cid", id).Arg("channel_flag_default", 1)); err != nil {
		return err
	}
	boot := r.t.bootstrapID
	_, err := r.write(fmt.Sprintf("delete the original default channel (cid %s)", boot),
		ts.NewCommand("channeldelete").Arg("cid", boot).Arg("force", true))
	return err
}

// --- passwords ----------------------------------------------------------------

func (r *restorer) lookupPassword(ch *Channel, path string) (string, bool) {
	if pw, ok := r.opts.Passwords[ch.Ref]; ok {
		return pw, true
	}
	pw, ok := r.opts.Passwords[path]
	return pw, ok
}

// verifyPassword compares the target's hash of the supplied password with
// the backup's. TS6 hashes deterministically per server but differently
// across servers, so this only works when restoring onto the server the
// backup came from; a mismatch there normally means a typo. The supplied
// password is kept either way; the report says so.
func (r *restorer) verifyPassword(ch *Channel, id string, note *PasswordOutcome) {
	if ch.PasswordHash == "" {
		note.Outcome = "password set as supplied (backup has no hash to verify against)"
		return
	}
	if !r.sameServer {
		note.Outcome = "password set as supplied (not verifiable: password hashes differ between servers)"
		return
	}
	info, err := first(r.q, ts.NewCommand("channelinfo").Arg("cid", id))
	switch {
	case err != nil:
		note.Outcome = "password set as supplied (could not verify: " + err.Error() + ")"
	case info["channel_password"] == ch.PasswordHash:
		note.Outcome = "password restored and verified"
	default:
		note.Outcome = "password set as supplied, but it does NOT match the backup: check it for typos"
	}
}

func (r *restorer) checkUnusedPasswords() {
	known := map[string]bool{}
	for ch, path := range channelPaths(r.f.Channels) {
		known[ch.Ref] = true
		known[path] = true
	}
	for k := range r.opts.Passwords {
		if !known[k] {
			r.report.UnusedPasswords = append(r.report.UnusedPasswords, k)
		}
	}
	sort.Strings(r.report.UnusedPasswords)
}

func randomPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- helpers ----------------------------------------------------------------

// channelPaths returns "Parent/Child" paths for every channel.
func channelPaths(channels []*Channel) map[*Channel]string {
	paths := map[*Channel]string{}
	_ = Walk(channels, func(ch, parent *Channel) error {
		if parent == nil {
			paths[ch] = ch.Name
		} else {
			paths[ch] = paths[parent] + "/" + ch.Name
		}
		return nil
	})
	return paths
}

func nameOf(rows []ts.Row, idKey, id string) string {
	for _, r := range rows {
		if r[idKey] == id {
			return r["name"]
		}
	}
	return ""
}

func groupNames(rows []ts.Row, idKey string, ids []string) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		names = append(names, nameOf(rows, idKey, id))
	}
	return names
}

func without(ids []string, drop ...string) []string {
	out := ids[:0:0]
	for _, id := range ids {
		keep := true
		for _, d := range drop {
			if id == d {
				keep = false
			}
		}
		if keep {
			out = append(out, id)
		}
	}
	return out
}

func queryMsg(err error) string {
	var qe *ts.QueryError
	if errors.As(err, &qe) {
		if qe.ExtraMsg != "" {
			return fmt.Sprintf("%s: %s (id %d)", qe.Msg, qe.ExtraMsg, qe.ID)
		}
		return fmt.Sprintf("%s (id %d)", qe.Msg, qe.ID)
	}
	return err.Error()
}
