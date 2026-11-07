package backup

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/floholz/ts-server-manager/internal/ts"
)

var update = flag.Bool("update", false, "rewrite golden files")

// fixtureUID is the unique identifier of the fixture's server. The
// simulated restore target uses it too unless a test needs a different
// server (password hashes are server-specific).
const fixtureUID = "W8SOO/IWcqv911YwiWI6f3Rx0SiESSZ1RvEaatKp48s="

// fakeServer answers commands from a table keyed by the exact command line.
// Unscripted commands fail, so tests also pin which commands are sent.
type fakeServer struct {
	responses map[string][]ts.Row
	sent      []string
}

func (s *fakeServer) Exec(cmd *ts.Command) ([]ts.Row, error) {
	line := cmd.String()
	s.sent = append(s.sent, line)
	rows, ok := s.responses[line]
	if !ok {
		return nil, fmt.Errorf("fake: unscripted command %q", line)
	}
	return rows, nil
}

func perm(sid, val string) ts.Row {
	return ts.Row{"permsid": sid, "permvalue": val, "permnegated": "0", "permskip": "0"}
}

// newFixture is a small TS6 server shaped like the beta13.1 responses in the
// spec's Appendix A:
//
//	Lobby (default)          cid 1
//	├─ Gaming                cid 4
//	│  └─ Locked             cid 5 (password)
//	└─ AFK                   cid 3   <- listed before Gaming by CID, but ordered after it
//	Music                    cid 2
func newFixture() *fakeServer {
	return &fakeServer{responses: map[string][]ts.Row{
		"version": {{"version": "6.0.0-beta13.1", "build": "1790080330", "platform": "Linux"}},
		"serverinfo": {{
			"virtualserver_name":                        "Example TS",
			"virtualserver_unique_identifier":           fixtureUID,
			"virtualserver_welcomemessage":              "Welcome!",
			"virtualserver_port":                        "9987",
			"virtualserver_uptime":                      "4242",
			"virtualserver_clientsonline":               "3",
			"virtualserver_id":                          "1",
			"virtualserver_password":                    "hash",
			"virtualserver_total_ping":                  "12.5",
			"connection_bytes_sent_total":               "999",
			"virtualserver_default_server_group":        "8",
			"virtualserver_default_channel_group":       "8",
			"virtualserver_default_channel_admin_group": "5",
		}},

		"servergrouplist": {
			{"sgid": "1", "name": "Guest Server Query", "type": "2"},
			{"sgid": "3", "name": "Server Admin", "type": "0"},
			{"sgid": "9", "name": "Moderator", "type": "1"},
			{"sgid": "6", "name": "Server Admin", "type": "1"},
			{"sgid": "8", "name": "Guest", "type": "1"},
		},
		"servergrouppermlist sgid=6 -permsid": {
			{"sgid": "6", "permsid": "i_group_sort_id", "permvalue": "10", "permnegated": "0", "permskip": "0"},
			{"sgid": "6", "permsid": "b_virtualserver_modify_name", "permvalue": "1", "permnegated": "0", "permskip": "0"},
		},
		"servergrouppermlist sgid=8 -permsid": {
			{"sgid": "8", "permsid": "b_virtualserver_channel_list", "permvalue": "1", "permnegated": "0", "permskip": "0"},
		},
		"servergrouppermlist sgid=9 -permsid": {
			{"sgid": "9", "permsid": "i_client_talk_power", "permvalue": "75", "permnegated": "1", "permskip": "1"},
			{"sgid": "9", "permsid": "i_client_kick_from_channel_power", "permvalue": "-1", "permnegated": "0", "permskip": "0"},
		},

		"channelgrouplist": {
			{"cgid": "1", "name": "Channel Admin", "type": "0"},
			{"cgid": "5", "name": "Channel Admin", "type": "1"},
			{"cgid": "8", "name": "Guest", "type": "1"},
		},
		"channelgrouppermlist cgid=5 -permsid": {perm("b_channel_modify_name", "1")},
		"channelgrouppermlist cgid=8 -permsid": nil, // empty result set

		"channellist": {
			{"cid": "1", "pid": "0", "channel_order": "0", "channel_name": "Lobby"},
			{"cid": "2", "pid": "0", "channel_order": "1", "channel_name": "Music"},
			{"cid": "3", "pid": "1", "channel_order": "4", "channel_name": "AFK"},
			{"cid": "4", "pid": "1", "channel_order": "0", "channel_name": "Gaming"},
			{"cid": "5", "pid": "4", "channel_order": "0", "channel_name": "Locked"},
		},
		"channelinfo cid=1": {channelInfo("Lobby", "1", "", map[string]string{"channel_topic": "Say hi"})},
		"channelinfo cid=2": {channelInfo("Music", "0", "", map[string]string{"channel_codec_quality": "10"})},
		"channelinfo cid=3": {channelInfo("AFK", "0", "", map[string]string{"channel_needed_talk_power": "100"})},
		"channelinfo cid=4": {channelInfo("Gaming", "0", "", nil)},
		"channelinfo cid=5": {channelInfo("Locked", "0", "enDElJk5HEDQo7PmB469eMoCfSs=", nil)},
		"channelpermlist cid=1 -permsid": {
			{"cid": "1", "permsid": "i_channel_needed_permission_modify_power", "permvalue": "50", "permnegated": "0", "permskip": "0"},
		},
		"channelpermlist cid=2 -permsid": nil,
		"channelpermlist cid=3 -permsid": nil,
		"channelpermlist cid=4 -permsid": {
			{"cid": "4", "permsid": "i_icon_id", "permvalue": "300", "permnegated": "0", "permskip": "0"},
			{"cid": "4", "permsid": "i_channel_needed_join_power", "permvalue": "25", "permnegated": "0", "permskip": "0"},
		},
		"channelpermlist cid=5 -permsid": nil,
	}}
}

// channelInfo builds a channelinfo row including the read-only/volatile
// properties TS6 returns, which the exporter must drop.
func channelInfo(name, isDefault, pwHash string, extra map[string]string) ts.Row {
	r := ts.Row{
		"channel_name":              name,
		"channel_topic":             "",
		"channel_description":       "",
		"channel_codec":             "4",
		"channel_codec_quality":     "6",
		"channel_maxclients":        "-1",
		"channel_flag_permanent":    "1",
		"channel_flag_default":      isDefault,
		"channel_flag_password":     "0",
		"channel_password":          "",
		"channel_needed_talk_power": "0",
		"channel_storage_quota":     "4294967295",
		// read-only / volatile on TS6:
		"pid":                       "0",
		"channel_order":             "0",
		"channel_icon_id":           "0",
		"channel_filepath":          "files/virtualserver_1/channel_x",
		"seconds_empty":             "-1",
		"channel_unique_identifier": "3b351454-0056-4037-a0c9-55a498fd7a1f",
		"channel_security_salt":     "",
		"channel_forced_silence":    "0",
	}
	if pwHash != "" {
		r["channel_flag_password"] = "1"
		r["channel_password"] = pwHash
	}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

func exportFixture(t *testing.T) (*File, *fakeServer) {
	t.Helper()
	srv := newFixture()
	f, err := Export(srv, ExportOptions{
		ToolVersion: "v0.0.0-test",
		Now:         func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Export: %v\nsent: %q", err, srv.sent)
	}
	return f, srv
}

func TestExport_Golden(t *testing.T) {
	f, _ := exportFixture(t)
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "export.golden.json")
	if *update {
		if err := os.WriteFile(golden, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run `go test ./internal/backup -update` to create): %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("export differs from %s (run with -update if intended)\n--- got ---\n%s", golden, buf.String())
	}
}

func TestExport_OnlyRegularGroupsSortedByID(t *testing.T) {
	f, _ := exportFixture(t)
	var refs []string
	for _, g := range f.ServerGroups {
		refs = append(refs, g.Ref+"="+g.Name)
	}
	if got, want := strings.Join(refs, ","), "sg:6=Server Admin,sg:8=Guest,sg:9=Moderator"; got != want {
		t.Errorf("server groups: got %s, want %s", got, want)
	}
	if len(f.ChannelGroups) != 2 {
		t.Errorf("channel groups: got %d, want 2 regular", len(f.ChannelGroups))
	}
	if f.ChannelGroups[1].Perms == nil || len(f.ChannelGroups[1].Perms) != 0 {
		t.Errorf("empty perm list should be [] not null: %#v", f.ChannelGroups[1].Perms)
	}
}

func TestExport_PermsSortedAndFlagsKept(t *testing.T) {
	f, _ := exportFixture(t)
	mod := f.ServerGroups[2]
	want := []Perm{
		{PermSID: "i_client_kick_from_channel_power", Value: -1},
		{PermSID: "i_client_talk_power", Value: 75, Negated: true, Skip: true},
	}
	if fmt.Sprint(mod.Perms) != fmt.Sprint(want) {
		t.Errorf("got %+v\nwant %+v", mod.Perms, want)
	}
}

func TestExport_ChannelTreeAndOrder(t *testing.T) {
	f, _ := exportFixture(t)
	var lines []string
	_ = Walk(f.Channels, func(ch, parent *Channel) error {
		p := "-"
		if parent != nil {
			p = parent.Name
		}
		lines = append(lines, p+">"+ch.Name)
		return nil
	})
	got := strings.Join(lines, " ")
	want := "->Lobby Lobby>Gaming Gaming>Locked Lobby>AFK ->Music"
	if got != want {
		t.Errorf("tree:\n got  %s\n want %s", got, want)
	}
}

func TestExport_PropertiesFiltered(t *testing.T) {
	f, _ := exportFixture(t)
	for _, k := range []string{"virtualserver_uptime", "virtualserver_clientsonline", "virtualserver_id",
		"virtualserver_password", "virtualserver_total_ping", "connection_bytes_sent_total",
		"virtualserver_default_server_group"} {
		if _, ok := f.Server.Properties[k]; ok {
			t.Errorf("server property %s should be dropped", k)
		}
	}
	for _, k := range []string{"virtualserver_name", "virtualserver_welcomemessage", "virtualserver_port"} {
		if _, ok := f.Server.Properties[k]; !ok {
			t.Errorf("server property %s should be kept", k)
		}
	}
	if f.Server.DefaultServerGroup != "sg:8" || f.Server.DefaultChannelAdminGroup != "cg:5" {
		t.Errorf("default pointers: %+v", f.Server)
	}

	lobby := f.Channels[0]
	for _, k := range []string{"channel_name", "pid", "channel_order", "channel_filepath", "seconds_empty",
		"channel_unique_identifier", "channel_security_salt", "channel_icon_id", "channel_password",
		"channel_flag_password", "channel_forced_silence"} {
		if _, ok := lobby.Properties[k]; ok {
			t.Errorf("channel property %s should be dropped", k)
		}
	}
	if lobby.Properties["channel_topic"] != "Say hi" || !lobby.IsDefault() {
		t.Errorf("lobby properties: %v", lobby.Properties)
	}
}

func TestExport_PasswordHashOnlyForProtectedChannels(t *testing.T) {
	f, _ := exportFixture(t)
	locked := f.Channels[0].Children[0].Children[0]
	if !locked.HasPassword || locked.PasswordHash != "enDElJk5HEDQo7PmB469eMoCfSs=" {
		t.Errorf("locked: has=%v hash=%q", locked.HasPassword, locked.PasswordHash)
	}
	if f.Channels[1].HasPassword || f.Channels[1].PasswordHash != "" {
		t.Errorf("music should have no password: %+v", f.Channels[1])
	}
}

func TestExport_PropagatesQueryErrors(t *testing.T) {
	srv := newFixture()
	delete(srv.responses, "channelinfo cid=4")
	_, err := Export(srv, ExportOptions{})
	if err == nil || !strings.Contains(err.Error(), "channelinfo") {
		t.Fatalf("expected channelinfo error, got %v", err)
	}
}

func TestOrderChannels_MissingParent(t *testing.T) {
	_, err := orderChannels([]ts.Row{{"cid": "2", "pid": "7", "channel_order": "0"}})
	if err == nil {
		t.Fatal("expected error for missing parent")
	}
}

func TestSortSiblings_BrokenChainKeepsAllChannels(t *testing.T) {
	got := sortSiblings([]ts.Row{
		{"cid": "5", "channel_order": "0"},
		{"cid": "9", "channel_order": "42"}, // points at a non-sibling
		{"cid": "7", "channel_order": "5"},
	})
	var cids []string
	for _, r := range got {
		cids = append(cids, r["cid"])
	}
	if strings.Join(cids, ",") != "5,7,9" {
		t.Errorf("got %v", cids)
	}
}
