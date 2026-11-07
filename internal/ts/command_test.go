package ts

import (
	"reflect"
	"testing"
)

func TestCommand_String(t *testing.T) {
	tests := []struct {
		name string
		cmd  *Command
		want string
	}{
		{"bare", NewCommand("version"), "version"},
		{"args", NewCommand("use").Arg("sid", 1), "use sid=1"},
		{"bool", NewCommand("servergroupdel").Arg("sgid", 6).Arg("force", true), "servergroupdel sgid=6 force=1"},
		{"escaping", NewCommand("channelcreate").Arg("channel_name", "AFK | away/idle"),
			`channelcreate channel_name=AFK\s\p\saway\/idle`},
		{"options", NewCommand("channellist").Option("-topic").Option("flags"), "channellist -topic -flags"},
		{"batched groups",
			NewCommand("servergroupaddperm").Arg("sgid", 6).
				Arg("permsid", "b_a").Arg("permvalue", 1).
				Next().Arg("permsid", "i_b").Arg("permvalue", 75),
			"servergroupaddperm sgid=6 permsid=b_a permvalue=1|permsid=i_b permvalue=75"},
		{"next on empty group is a no-op",
			NewCommand("x").Next().Arg("a", 1).Next().Next().Arg("b", 2).Next(),
			"x a=1|b=2"},
		{"args and options", NewCommand("servergrouppermlist").Arg("sgid", 6).Option("-permsid"),
			"servergrouppermlist sgid=6 -permsid"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cmd.String(); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	for _, s := range []string{
		"plain", "with space", `back\slash`, "pipe|char", "a/b",
		"multi\nline\ttab", `\s literal`, "",
	} {
		if got := Unescape(Escape(s)); got != s {
			t.Errorf("round trip %q -> %q -> %q", s, Escape(s), got)
		}
	}
}

func TestParseRows(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		want  []Row
	}{
		{"none", nil, nil},
		{"empty line", []string{""}, nil},
		{"single", []string{`version=6.0.0 build=1 platform=Linux`},
			[]Row{{"version": "6.0.0", "build": "1", "platform": "Linux"}}},
		{"multiple records and escapes",
			[]string{`cid=1 channel_name=Default\sChannel|cid=2 channel_name=a\/b\pc`},
			[]Row{
				{"cid": "1", "channel_name": "Default Channel"},
				{"cid": "2", "channel_name": "a/b|c"},
			}},
		{"key without value and value containing '='",
			[]string{`channel_topic channel_description=x=y`},
			[]Row{{"channel_topic": "", "channel_description": "x=y"}}},
		{"permlist shape: id only on first record",
			[]string{`sgid=6 permsid=b_a permvalue=1 permnegated=0 permskip=0|permsid=i_b permvalue=75 permnegated=0 permskip=1`},
			[]Row{
				{"sgid": "6", "permsid": "b_a", "permvalue": "1", "permnegated": "0", "permskip": "0"},
				{"permsid": "i_b", "permvalue": "75", "permnegated": "0", "permskip": "1"},
			}},
		{"multiple data lines", []string{"a=1", "b=2\r"}, []Row{{"a": "1"}, {"b": "2"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseRows(tc.lines); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestRowInt(t *testing.T) {
	r := Row{"n": "42", "bad": "x"}
	if v, ok := r.Int("n"); !ok || v != 42 {
		t.Errorf("Int(n) = %d, %v", v, ok)
	}
	if _, ok := r.Int("bad"); ok {
		t.Error("Int(bad) should fail")
	}
	if v := r.IntOr("missing", 7); v != 7 {
		t.Errorf("IntOr(missing) = %d", v)
	}
}
