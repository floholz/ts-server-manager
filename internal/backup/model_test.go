package backup

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func validFile() *File {
	return &File{
		SchemaVersion: SchemaVersion,
		Server: Server{
			Properties:               map[string]string{"virtualserver_name": "x"},
			DefaultServerGroup:       "sg:8",
			DefaultChannelGroup:      "cg:8",
			DefaultChannelAdminGroup: "cg:5",
		},
		ServerGroups:  []Group{{Ref: "sg:8", Name: "Guest", Perms: []Perm{}}},
		ChannelGroups: []Group{{Ref: "cg:5", Name: "Channel Admin", Perms: []Perm{}}, {Ref: "cg:8", Name: "Guest", Perms: []Perm{}}},
		Channels: []*Channel{{
			Ref: "c:1", Name: "Lobby",
			Properties: map[string]string{"channel_flag_default": "1"},
			Perms:      []Perm{},
			Children: []*Channel{{
				Ref: "c:2", Name: "Sub", Properties: map[string]string{}, Perms: []Perm{}, Children: []*Channel{},
			}},
		}},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	f := validFile()
	var buf bytes.Buffer
	if err := Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Errorf("round trip mismatch:\n got  %+v\n want %+v", got, f)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*File)
		wantErr string
	}{
		{"valid", func(*File) {}, ""},
		{"schema version", func(f *File) { f.SchemaVersion = 99 }, "schema_version 99"},
		{"duplicate ref", func(f *File) { f.Channels[0].Children[0].Ref = "c:1" }, `duplicate ref "c:1"`},
		{"empty ref", func(f *File) { f.ServerGroups[0].Ref = "" }, "empty ref"},
		{"default server group missing", func(f *File) { f.Server.DefaultServerGroup = "sg:1" }, "default_server_group"},
		{"default channel group points at server group", func(f *File) { f.Server.DefaultChannelGroup = "sg:8" }, "default_channel_group"},
		{"no default channel", func(f *File) { f.Channels[0].Properties["channel_flag_default"] = "0" }, "found 0"},
		{"two default channels", func(f *File) { f.Channels[0].Children[0].Properties["channel_flag_default"] = "1" }, "found 2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := validFile()
			tc.mutate(f)
			err := f.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestRead_RejectsUnknownFields(t *testing.T) {
	var buf bytes.Buffer
	_ = Write(&buf, validFile())
	s := strings.Replace(buf.String(), `"schema_version": 1,`, `"schema_version": 1, "surprise": true,`, 1)
	if _, err := Read(strings.NewReader(s)); err == nil || !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}
