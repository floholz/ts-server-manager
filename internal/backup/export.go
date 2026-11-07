package backup

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/floholz/ts-server-manager/internal/ts"
)

// TS6 group types. Only regular groups are exported.
const (
	groupTypeTemplate = "0"
	groupTypeRegular  = "1"
	groupTypeQuery    = "2"
)

// ExportOptions configures Export.
type ExportOptions struct {
	ToolVersion string
	// Now defaults to time.Now.
	Now func() time.Time
	// Progress, if set, receives short human-readable progress messages.
	Progress func(msg string)
}

// Export reads the selected virtual server through q and builds a backup.
// q should be paced (ts.Pacer) unless the client is on the query allowlist.
func Export(q ts.Execer, opts ExportOptions) (*File, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	progress := opts.Progress
	if progress == nil {
		progress = func(string) {}
	}

	f := &File{
		SchemaVersion: SchemaVersion,
		CreatedAt:     now().UTC().Truncate(time.Second),
		ToolVersion:   opts.ToolVersion,
	}

	progress("reading server info")
	ver, err := first(q, ts.NewCommand("version"))
	if err != nil {
		return nil, err
	}
	si, err := first(q, ts.NewCommand("serverinfo"))
	if err != nil {
		return nil, err
	}
	f.Source = Source{
		TSVersion:  ver["version"],
		Platform:   ver["platform"],
		ServerName: si["virtualserver_name"],
	}
	f.Server.Properties = filterProps(si, volatileServerProp)

	progress("reading server groups")
	sgs, err := exportGroups(q, "servergrouplist", "servergrouppermlist", "sgid", "sg:", progress)
	if err != nil {
		return nil, err
	}
	f.ServerGroups = sgs

	progress("reading channel groups")
	cgs, err := exportGroups(q, "channelgrouplist", "channelgrouppermlist", "cgid", "cg:", progress)
	if err != nil {
		return nil, err
	}
	f.ChannelGroups = cgs

	f.Server.DefaultServerGroup = "sg:" + si["virtualserver_default_server_group"]
	f.Server.DefaultChannelGroup = "cg:" + si["virtualserver_default_channel_group"]
	f.Server.DefaultChannelAdminGroup = "cg:" + si["virtualserver_default_channel_admin_group"]

	progress("reading channels")
	chs, err := exportChannels(q, progress)
	if err != nil {
		return nil, err
	}
	f.Channels = chs

	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("exported backup is inconsistent: %w", err)
	}
	return f, nil
}

func exportGroups(q ts.Execer, listCmd, permCmd, idKey, refPrefix string, progress func(string)) ([]Group, error) {
	rows, err := q.Exec(ts.NewCommand(listCmd))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", listCmd, err)
	}
	var regular []ts.Row
	for _, r := range rows {
		if r["type"] == groupTypeRegular {
			regular = append(regular, r)
		}
	}
	sort.SliceStable(regular, func(i, j int) bool {
		return regular[i].IntOr(idKey, 0) < regular[j].IntOr(idKey, 0)
	})

	groups := make([]Group, 0, len(regular))
	for i, r := range regular {
		id := r[idKey]
		progress(fmt.Sprintf("  %s %d/%d: %s", strings.TrimSuffix(refPrefix, ":"), i+1, len(regular), r["name"]))
		perms, err := readPerms(q, ts.NewCommand(permCmd).Arg(idKey, id).Option("-permsid"))
		if err != nil {
			return nil, err
		}
		groups = append(groups, Group{Ref: refPrefix + id, Name: r["name"], Perms: perms})
	}
	return groups, nil
}

func exportChannels(q ts.Execer, progress func(string)) ([]*Channel, error) {
	rows, err := q.Exec(ts.NewCommand("channellist"))
	if err != nil {
		return nil, fmt.Errorf("channellist: %w", err)
	}
	ordered, err := orderChannels(rows)
	if err != nil {
		return nil, err
	}

	byCID := map[string]*Channel{}
	var roots []*Channel
	for i, r := range ordered {
		cid := r["cid"]
		progress(fmt.Sprintf("  channel %d/%d: %s", i+1, len(ordered), r["channel_name"]))

		info, err := first(q, ts.NewCommand("channelinfo").Arg("cid", cid))
		if err != nil {
			return nil, err
		}
		perms, err := readPerms(q, ts.NewCommand("channelpermlist").Arg("cid", cid).Option("-permsid"))
		if err != nil {
			return nil, err
		}

		ch := &Channel{
			Ref:         "c:" + cid,
			Name:        info["channel_name"],
			Properties:  filterProps(info, nonChannelProp),
			HasPassword: info["channel_flag_password"] == "1",
			Perms:       perms,
			Children:    []*Channel{},
		}
		if ch.HasPassword {
			ch.PasswordHash = info["channel_password"]
		}
		byCID[cid] = ch

		// orderChannels yields parents before children.
		if pid := r["pid"]; pid == "0" {
			roots = append(roots, ch)
		} else {
			parent := byCID[pid]
			parent.Children = append(parent.Children, ch)
		}
	}
	if roots == nil {
		roots = []*Channel{}
	}
	return roots, nil
}

// orderChannels returns channellist rows depth first, parents before
// children, siblings in display order. A channel's channel_order is the
// CID of the sibling directly above it (0 = first).
func orderChannels(rows []ts.Row) ([]ts.Row, error) {
	byParent := map[string][]ts.Row{}
	known := map[string]bool{}
	for _, r := range rows {
		byParent[r["pid"]] = append(byParent[r["pid"]], r)
		known[r["cid"]] = true
	}
	for pid := range byParent {
		if pid != "0" && !known[pid] {
			return nil, fmt.Errorf("channellist: parent channel %s missing", pid)
		}
	}

	var out []ts.Row
	var visit func(pid string)
	visit = func(pid string) {
		for _, r := range sortSiblings(byParent[pid]) {
			out = append(out, r)
			visit(r["cid"])
		}
	}
	visit("0")
	if len(out) != len(rows) {
		return nil, fmt.Errorf("channellist: channel tree is not connected (%d of %d channels reachable)", len(out), len(rows))
	}
	return out, nil
}

// sortSiblings follows the channel_order chain. Channels the chain doesn't
// reach (shouldn't happen) are appended in CID order rather than dropped.
func sortSiblings(sibs []ts.Row) []ts.Row {
	after := map[string]ts.Row{} // channel_order -> channel placed after it
	for _, r := range sibs {
		after[r["channel_order"]] = r
	}
	var out []ts.Row
	seen := map[string]bool{}
	for prev := "0"; ; {
		r, ok := after[prev]
		if !ok || seen[r["cid"]] {
			break
		}
		out = append(out, r)
		seen[r["cid"]] = true
		prev = r["cid"]
	}
	if len(out) < len(sibs) {
		var rest []ts.Row
		for _, r := range sibs {
			if !seen[r["cid"]] {
				rest = append(rest, r)
			}
		}
		sort.Slice(rest, func(i, j int) bool { return rest[i].IntOr("cid", 0) < rest[j].IntOr("cid", 0) })
		out = append(out, rest...)
	}
	return out
}

func readPerms(q ts.Execer, cmd *ts.Command) ([]Perm, error) {
	rows, err := q.Exec(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd.Name(), err)
	}
	perms := make([]Perm, 0, len(rows))
	for _, r := range rows {
		sid := r["permsid"]
		if sid == "" {
			return nil, fmt.Errorf("%s: row without permsid: %v", cmd.Name(), r)
		}
		v, ok := r.Int("permvalue")
		if !ok {
			return nil, fmt.Errorf("%s: %s has non-numeric permvalue %q", cmd.Name(), sid, r["permvalue"])
		}
		perms = append(perms, Perm{
			PermSID: sid,
			Value:   v,
			Negated: r["permnegated"] == "1",
			Skip:    r["permskip"] == "1",
		})
	}
	sort.Slice(perms, func(i, j int) bool { return perms[i].PermSID < perms[j].PermSID })
	return perms, nil
}

func first(q ts.Execer, cmd *ts.Command) (ts.Row, error) {
	rows, err := q.Exec(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", cmd.Name(), err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%s: empty response", cmd.Name())
	}
	return rows[0], nil
}

func filterProps(r ts.Row, drop func(string) bool) map[string]string {
	out := make(map[string]string, len(r))
	for k, v := range r {
		if !drop(k) {
			out[k] = v
		}
	}
	return out
}

// nonChannelProp reports channelinfo keys that are not stored in
// Channel.Properties: represented elsewhere (name, tree position, password,
// icon via i_icon_id), or read-only/volatile on TS6 (Appendix A).
func nonChannelProp(k string) bool {
	switch k {
	case "cid", "pid", "channel_order", "channel_name",
		"channel_password", "channel_flag_password",
		"channel_icon_id",
		"channel_filepath", "seconds_empty", "channel_unique_identifier",
		"channel_security_salt", "channel_forced_silence",
		"total_clients", "total_clients_family":
		return true
	}
	return false
}

// volatileServerProp reports serverinfo keys that are dropped: counters,
// runtime state, identity of this particular instance, the password hash,
// and default group IDs (stored as refs instead).
func volatileServerProp(k string) bool {
	if strings.HasPrefix(k, "connection_") ||
		strings.HasPrefix(k, "virtualserver_total_") ||
		strings.HasPrefix(k, "virtualserver_month_bytes_") ||
		strings.HasPrefix(k, "virtualserver_default_") && strings.HasSuffix(k, "_group") {
		return true
	}
	switch k {
	case "virtualserver_id", "virtualserver_unique_identifier", "virtualserver_uuid",
		"virtualserver_status", "virtualserver_uptime", "virtualserver_created",
		"virtualserver_version", "virtualserver_version_sign", "virtualserver_platform",
		"virtualserver_clientsonline", "virtualserver_queryclientsonline", "virtualserver_channelsonline",
		"virtualserver_client_connections", "virtualserver_query_client_connections",
		"virtualserver_password", "virtualserver_flag_password",
		"virtualserver_filebase", "virtualserver_machine_id":
		return true
	}
	return false
}
