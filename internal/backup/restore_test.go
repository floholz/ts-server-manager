package backup

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- normalised comparison ----------------------------------------------------

// norm is the part of a backup a restore is expected to reproduce, with
// server-assigned IDs and refs removed.
type norm struct {
	Server   map[string]string
	Defaults [3]string // group names
	SG, CG   []normGroup
	Channels []normChannel
}

type normGroup struct {
	Name  string
	Perms []Perm
}

type normChannel struct {
	Name        string
	Props       map[string]string
	HasPassword bool
	Hash        string
	Perms       []Perm
	Children    []normChannel
}

func normalize(f *File) norm {
	n := norm{Server: map[string]string{}}
	for _, k := range restorableServerProps {
		if v, ok := f.Server.Properties[k]; ok {
			n.Server[k] = v
		}
	}
	name := func(groups []Group, ref string) string {
		for _, g := range groups {
			if g.Ref == ref {
				return g.Name
			}
		}
		return "?" + ref
	}
	n.Defaults = [3]string{
		name(f.ServerGroups, f.Server.DefaultServerGroup),
		name(f.ChannelGroups, f.Server.DefaultChannelGroup),
		name(f.ChannelGroups, f.Server.DefaultChannelAdminGroup),
	}
	groups := func(gs []Group) []normGroup {
		out := []normGroup{}
		for _, g := range gs {
			out = append(out, normGroup{Name: g.Name, Perms: g.Perms})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	n.SG, n.CG = groups(f.ServerGroups), groups(f.ChannelGroups)
	var chans func([]*Channel) []normChannel
	chans = func(cs []*Channel) []normChannel {
		out := []normChannel{}
		for _, c := range cs {
			props := map[string]string{}
			for _, k := range restorableChannelProps {
				if v, ok := c.Properties[k]; ok {
					props[k] = v
				}
			}
			if c.IsDefault() {
				props["channel_flag_default"] = "1"
			}
			out = append(out, normChannel{Name: c.Name, Props: props, HasPassword: c.HasPassword,
				Hash: c.PasswordHash, Perms: c.Perms, Children: chans(c.Children)})
		}
		return out
	}
	n.Channels = chans(f.Channels)
	return n
}

func assertSameSetup(t *testing.T, want, got *File) {
	t.Helper()
	w, g := normalize(want), normalize(got)
	if !reflect.DeepEqual(w, g) {
		t.Errorf("restored server differs from backup\n want %+v\n got  %+v", w, g)
	}
}

// --- helpers ----------------------------------------------------------------

func fixtureBackup(t *testing.T) *File {
	t.Helper()
	f, _ := exportFixture(t)
	return f
}

func restoreInto(t *testing.T, sim *simServer, f *File, opts RestoreOptions) *Report {
	t.Helper()
	if opts.RandomPassword == nil {
		opts.RandomPassword = func() (string, error) { return "RANDOM", nil }
	}
	if opts.Passwords == nil {
		// Supply the fixture's real password unless a test opts out with
		// an empty map, so round-trip comparisons include the hash.
		opts.Passwords = map[string]string{"c:5": "s3cret"}
	}
	rep, err := Restore(sim, f, opts)
	if err != nil {
		t.Fatalf("Restore: %v\nactions:\n  %s\nwrites:\n  %s", err,
			strings.Join(rep.Actions, "\n  "), strings.Join(sim.writes, "\n  "))
	}
	return rep
}

func exportSim(t *testing.T, sim *simServer) *File {
	t.Helper()
	f, err := Export(sim, ExportOptions{})
	if err != nil {
		t.Fatalf("export of restored server: %v", err)
	}
	return f
}

func pathsOf(f *File) []string {
	var out []string
	for ch, p := range channelPaths(f.Channels) {
		_ = ch
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// --- tests ------------------------------------------------------------------

func TestRestore_FreshServerRoundTrip(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	confirmCalled := false
	rep := restoreInto(t, sim, backup, RestoreOptions{
		Passwords:        map[string]string{"Lobby/Gaming/Locked": "s3cret"},
		ConfirmOverwrite: func(TargetSummary) bool { confirmCalled = true; return true },
	})

	if !rep.Target.Fresh || confirmCalled {
		t.Errorf("fresh target: Fresh=%v confirmCalled=%v", rep.Target.Fresh, confirmCalled)
	}
	assertSameSetup(t, backup, exportSim(t, sim))

	if len(rep.Passwords) != 1 || rep.Passwords[0].Outcome != "password restored and verified" {
		t.Errorf("password outcome: %+v", rep.Passwords)
	}
	if len(rep.SkippedPerms) != 0 || len(rep.SkippedProps) != 0 {
		t.Errorf("unexpected skips: perms=%+v props=%+v", rep.SkippedPerms, rep.SkippedProps)
	}
}

func TestRestore_ChannelOrderAndDefault(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	restoreInto(t, sim, backup, RestoreOptions{})
	got := exportSim(t, sim)

	var order []string
	_ = Walk(got.Channels, func(ch, _ *Channel) error {
		mark := ""
		if ch.IsDefault() {
			mark = "*"
		}
		order = append(order, ch.Name+mark)
		return nil
	})
	if s := strings.Join(order, ","); s != "Lobby*,Gaming,Locked,AFK,Music" {
		t.Errorf("channel order: %s", s)
	}
	for _, p := range pathsOf(got) {
		if strings.Contains(p, placeholderChannelName) {
			t.Errorf("placeholder channel left behind: %s", p)
		}
	}
}

func TestRestore_NonFreshRequiresConfirmation(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	restoreInto(t, sim, backup, RestoreOptions{}) // now not fresh
	sim.writes = nil

	var summary TargetSummary
	_, err := Restore(sim, backup, RestoreOptions{
		ConfirmOverwrite: func(s TargetSummary) bool { summary = s; return false },
	})
	if !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("expected ErrNotConfirmed, got %v", err)
	}
	if len(sim.writes) != 0 {
		t.Errorf("nothing may change before confirmation, got writes: %q", sim.writes)
	}
	if summary.Fresh || summary.ChannelsDeleted != 5 {
		t.Errorf("summary: %+v", summary)
	}

	if _, err := Restore(sim, backup, RestoreOptions{}); !errors.Is(err, ErrNotConfirmed) {
		t.Fatalf("nil ConfirmOverwrite must abort, got %v", err)
	}
}

func TestRestore_OverwriteUpdatesMatchingGroupsInPlace(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	// A "used" server: an extra role, a role that also exists in the
	// backup (members must survive, so its ID must too), extra channels.
	sim.sgs = append(sim.sgs,
		&simGroup{id: 20, name: "Old Role", typ: "1", perms: map[string]Perm{}},
		&simGroup{id: 21, name: "Moderator", typ: "1", perms: map[string]Perm{
			"b_client_ignore_antiflood": {PermSID: "b_client_ignore_antiflood", Value: 1}}},
	)
	sim.cgs = append(sim.cgs, &simGroup{id: 30, name: "Old CG", typ: "1", perms: map[string]Perm{}})
	for _, c := range []*simChannel{
		{cid: 50, pid: 0, name: "Old Root", props: map[string]string{}, perms: map[string]Perm{}},
		{cid: 51, pid: 50, name: "Old Child", props: map[string]string{}, perms: map[string]Perm{}},
		{cid: 52, pid: 1, name: "Under Default", props: map[string]string{}, perms: map[string]Perm{}},
	} {
		sim.chans = append(sim.chans, c)
	}
	sim.chans[1].order = 1
	sim.nextCID = 60

	var summary TargetSummary
	rep := restoreInto(t, sim, backup, RestoreOptions{
		ConfirmOverwrite: func(s TargetSummary) bool { summary = s; return true },
	})
	if summary.Fresh || summary.ChannelsDeleted != 4 {
		t.Errorf("summary: %+v", summary)
	}
	if strings.Join(summary.ServerGroupsDeleted, ",") != "Normal,Old Role" ||
		strings.Join(summary.ChannelGroupsDeleted, ",") != "Operator,Voice,Old CG" {
		t.Errorf("deleted groups: sg=%v cg=%v", summary.ServerGroupsDeleted, summary.ChannelGroupsDeleted)
	}

	assertSameSetup(t, backup, exportSim(t, sim))
	if g := findGroup(sim.sgs, "21"); g == nil || g.name != "Moderator" {
		t.Errorf("existing Moderator group must be updated in place (sgid 21), groups: %+v", sim.sgs)
	}
	if !rep.Target.Fresh && len(rep.Actions) == 0 {
		t.Error("expected actions")
	}
}

func TestRestore_IsRepeatable(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	opts := RestoreOptions{ConfirmOverwrite: func(TargetSummary) bool { return true },
		Passwords: map[string]string{"c:5": "s3cret"}}
	restoreInto(t, sim, backup, opts)
	restoreInto(t, sim, backup, opts)
	assertSameSetup(t, backup, exportSim(t, sim))
}

func TestRestore_DryRunChangesNothing(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	rep := restoreInto(t, sim, backup, RestoreOptions{DryRun: true})
	if len(sim.writes) != 0 {
		t.Fatalf("dry run sent modifying commands: %q", sim.writes)
	}
	plan := strings.Join(rep.Actions, "\n")
	for _, want := range []string{
		`create server group "Moderator"`,
		`create channel "Lobby/Gaming/Locked"`,
		`make "Lobby" the default channel`,
		`delete the original default channel (cid 1)`,
		`server settings: update virtualserver_name, virtualserver_welcomemessage`,
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("plan lacks %q\nplan:\n%s", want, plan)
		}
	}
}

func TestRestore_DryRunOnNonFreshDoesNotAskButReports(t *testing.T) {
	backup := fixtureBackup(t)
	sim := newFreshSim()
	restoreInto(t, sim, backup, RestoreOptions{})
	sim.writes = nil
	rep, err := Restore(sim, backup, RestoreOptions{DryRun: true,
		ConfirmOverwrite: func(TargetSummary) bool { t.Error("dry run must not ask"); return false }})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Target.Fresh || len(sim.writes) != 0 {
		t.Errorf("Fresh=%v writes=%q", rep.Target.Fresh, sim.writes)
	}
	if !strings.Contains(strings.Join(rep.Actions, "\n"), "and its subchannels") {
		t.Errorf("plan should show channel deletions:\n%s", strings.Join(rep.Actions, "\n"))
	}
}

func TestRestore_UnknownPermissionSkippedRestApplied(t *testing.T) {
	backup := fixtureBackup(t)
	backup.ServerGroups[2].Perms = append(backup.ServerGroups[2].Perms, Perm{PermSID: "i_from_the_future", Value: 1})
	sim := newFreshSim()
	rep := restoreInto(t, sim, backup, RestoreOptions{})
	if len(rep.SkippedPerms) != 1 || rep.SkippedPerms[0].Name != "i_from_the_future" {
		t.Fatalf("skipped: %+v", rep.SkippedPerms)
	}
	backup.ServerGroups[2].Perms = backup.ServerGroups[2].Perms[:len(backup.ServerGroups[2].Perms)-1]
	assertSameSetup(t, backup, exportSim(t, sim))
}

func TestRestore_RejectedBatchItemFallsBackPerItem(t *testing.T) {
	backup := fixtureBackup(t)
	gaming := backup.Channels[0].Children[0]
	// Valid permission name, but not accepted on channels: the batch fails
	// part-way with "invalid parameter".
	gaming.Perms = append([]Perm{{PermSID: "b_channel_join_ignore_password", Value: 1}}, gaming.Perms...)
	sim := newFreshSim()
	rep := restoreInto(t, sim, backup, RestoreOptions{})

	if len(rep.SkippedPerms) != 1 || rep.SkippedPerms[0].Name != "b_channel_join_ignore_password" ||
		!strings.Contains(rep.SkippedPerms[0].Reason, "invalid parameter") {
		t.Fatalf("skipped: %+v", rep.SkippedPerms)
	}
	gaming.Perms = gaming.Perms[1:]
	assertSameSetup(t, backup, exportSim(t, sim))
}

func TestRestore_Passwords(t *testing.T) {
	t.Run("missing password gets a random one", func(t *testing.T) {
		sim := newFreshSim()
		rep := restoreInto(t, sim, fixtureBackup(t), RestoreOptions{Passwords: map[string]string{}})
		if len(rep.Passwords) != 1 || rep.Passwords[0].Password != "RANDOM" ||
			!strings.Contains(rep.Passwords[0].Outcome, "random") {
			t.Fatalf("outcome: %+v", rep.Passwords)
		}
		if got := exportSim(t, sim).Channels[0].Children[0].Children[0]; !got.HasPassword || got.PasswordHash != "H:"+fixtureUID+":RANDOM" {
			t.Errorf("locked channel must stay locked: %+v", got)
		}
	})
	t.Run("wrong password is kept but flagged", func(t *testing.T) {
		rep := restoreInto(t, newFreshSim(), fixtureBackup(t),
			RestoreOptions{Passwords: map[string]string{"Lobby/Gaming/Locked": "s3cert"}})
		if !strings.Contains(rep.Passwords[0].Outcome, "does NOT match") {
			t.Fatalf("outcome: %+v", rep.Passwords)
		}
	})
	t.Run("other server: correct password is not reported as wrong", func(t *testing.T) {
		sim := newFreshSim()
		sim.props["virtualserver_unique_identifier"] = "another-server"
		rep := restoreInto(t, sim, fixtureBackup(t), RestoreOptions{Passwords: map[string]string{"c:5": "s3cret"}})
		if !strings.Contains(rep.Passwords[0].Outcome, "not verifiable") {
			t.Fatalf("outcome: %+v", rep.Passwords)
		}
	})
	t.Run("unused keys are reported", func(t *testing.T) {
		rep := restoreInto(t, newFreshSim(), fixtureBackup(t),
			RestoreOptions{Passwords: map[string]string{"Lobby/Gaming/Lockd": "x", "c:5": "s3cret"}})
		if strings.Join(rep.UnusedPasswords, ",") != "Lobby/Gaming/Lockd" {
			t.Fatalf("unused: %v", rep.UnusedPasswords)
		}
		if rep.Passwords[0].Outcome != "password restored and verified" {
			t.Errorf("ref key should work: %+v", rep.Passwords)
		}
	})
}

func TestRestore_RootChannelNamedLikeTargetDefault(t *testing.T) {
	backup := fixtureBackup(t)
	backup.Channels[0].Name = "Default Channel" // same name as the target's bootstrap channel
	sim := newFreshSim()
	restoreInto(t, sim, backup, RestoreOptions{})
	assertSameSetup(t, backup, exportSim(t, sim))
}

func TestRestore_RejectedServerPropertyIsSkipped(t *testing.T) {
	backup := fixtureBackup(t)
	restorableServerProps = append(restorableServerProps, "virtualserver_bogus")
	defer func() { restorableServerProps = restorableServerProps[:len(restorableServerProps)-1] }()
	backup.Server.Properties["virtualserver_bogus"] = "1"

	sim := newFreshSim()
	rep := restoreInto(t, sim, backup, RestoreOptions{})
	if len(rep.SkippedProps) != 1 || rep.SkippedProps[0].Name != "virtualserver_bogus" {
		t.Fatalf("skipped props: %+v", rep.SkippedProps)
	}
	if sim.props["virtualserver_name"] != "Example TS" {
		t.Errorf("other properties must still be applied: %v", sim.props["virtualserver_name"])
	}
}

func TestRestore_StopsOnFailure(t *testing.T) {
	sim := newFreshSim()
	sim.failNext = map[string]bool{"channelcreate": true}
	_, err := Restore(sim, fixtureBackup(t), RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), `create channel "Lobby"`) {
		t.Fatalf("expected failure at first channel, got %v", err)
	}
}

func TestPlanGroups(t *testing.T) {
	backup := []Group{{Ref: "sg:8", Name: "Guest"}, {Ref: "sg:9", Name: "Moderator"}, {Ref: "sg:10", Name: "VIP"}}
	target := []map[string]string{
		{"sgid": "8", "name": "Guest"}, {"sgid": "12", "name": "Moderator"}, {"sgid": "13", "name": "Old"},
	}
	rows := make([]map[string]string, len(target))
	copy(rows, target)
	p := planGroups(backup, toRows(rows), "sgid", map[string]string{"sg:8": "8"})
	if p.matches["sg:8"] != "8" || p.matches["sg:9"] != "12" || p.matches["sg:10"] != "" {
		t.Errorf("matches: %v", p.matches)
	}
	if strings.Join(p.deletes, ",") != "13" {
		t.Errorf("deletes: %v", p.deletes)
	}
}
