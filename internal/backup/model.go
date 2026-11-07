// Package backup exports a TeamSpeak virtual server's channel layout,
// server groups, channel groups and their permissions to a portable JSON
// file, and (later) restores it.
//
// The file never relies on server-assigned IDs: objects reference each
// other through file-local refs, and permissions are stored by name
// (permsid), so a backup can be restored onto a different instance or a
// newer TS6 build. See docs/superpowers/specs/2026-11-07-backup-and-restore-design.md.
package backup

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SchemaVersion is the current backup file format version. Restore refuses
// files with a different version.
const SchemaVersion = 1

// File is the root of a backup file.
type File struct {
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	ToolVersion   string    `json:"tool_version"`
	Source        Source    `json:"source"`

	Server        Server     `json:"server"`
	ServerGroups  []Group    `json:"server_groups"`
	ChannelGroups []Group    `json:"channel_groups"`
	Channels      []*Channel `json:"channels"`
}

// Source describes the server the backup was taken from. Informational only.
type Source struct {
	TSVersion  string `json:"ts_version"`
	Platform   string `json:"platform"`
	ServerName string `json:"server_name"`
	// ServerUID is the source's virtualserver_unique_identifier. Channel
	// password hashes are server-specific, so restore can only verify a
	// supplied password when the target is the same server.
	ServerUID string `json:"server_uid,omitempty"`
}

// Server holds virtual-server-wide settings.
type Server struct {
	// Properties are the server's virtualserver_* properties as returned by
	// serverinfo, minus volatile ones (counters, uptime, IDs). Values are
	// kept as the raw ServerQuery strings. Restore writes only a curated
	// subset.
	Properties map[string]string `json:"properties"`

	// Default group pointers, as refs into ServerGroups / ChannelGroups.
	DefaultServerGroup       string `json:"default_server_group"`
	DefaultChannelGroup      string `json:"default_channel_group"`
	DefaultChannelAdminGroup string `json:"default_channel_admin_group"`
}

// Group is a regular (type 1) server group or channel group. On TS6, sort
// order, name mode, icon and persistence are permissions, so name and
// permission set describe a group completely.
type Group struct {
	Ref   string `json:"ref"`
	Name  string `json:"name"`
	Perms []Perm `json:"perms"`
}

// Perm is one permission assignment. Negated and Skip only apply to group
// permissions; they are always false for channel permissions.
type Perm struct {
	PermSID string `json:"permsid"`
	Value   int    `json:"value"`
	Negated bool   `json:"negated"`
	Skip    bool   `json:"skip"`
}

// Channel is a permanent or semi-permanent channel. Children are ordered as
// on the server, so parent and sibling order are implicit.
type Channel struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`

	// Properties are the channel_* properties from channelinfo, minus
	// read-only/volatile ones and those represented elsewhere (name,
	// parent, order, password).
	Properties map[string]string `json:"properties"`

	// HasPassword is true for password-protected channels. The password
	// itself cannot be recovered; PasswordHash (the server's deterministic
	// hash) lets restore verify a password the user supplies.
	HasPassword  bool   `json:"has_password"`
	PasswordHash string `json:"password_hash,omitempty"`

	Perms    []Perm     `json:"perms"`
	Children []*Channel `json:"children"`
}

// IsDefault reports whether this is the server's default channel.
func (c *Channel) IsDefault() bool {
	return c.Properties["channel_flag_default"] == "1"
}

// Walk calls fn for every channel in tree order (depth first, parents
// before children). It stops at the first error.
func Walk(channels []*Channel, fn func(ch *Channel, parent *Channel) error) error {
	var walk func([]*Channel, *Channel) error
	walk = func(list []*Channel, parent *Channel) error {
		for _, ch := range list {
			if err := fn(ch, parent); err != nil {
				return err
			}
			if err := walk(ch.Children, ch); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(channels, nil)
}

// Write encodes f as indented JSON. Map keys are sorted by encoding/json,
// and the exporter sorts permissions, so unchanged servers produce
// identical files apart from created_at.
func Write(w io.Writer, f *File) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(f)
}

// Read decodes and validates a backup file.
func Read(r io.Reader) (*File, error) {
	var f File
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode backup: %w", err)
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &f, nil
}

// Validate checks schema version and internal consistency: unique refs,
// default group pointers that resolve, and exactly one default channel.
func (f *File) Validate() error {
	if f.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported backup schema_version %d (this build supports %d)",
			f.SchemaVersion, SchemaVersion)
	}

	refs := map[string]bool{}
	addRef := func(kind, ref string) error {
		if ref == "" {
			return fmt.Errorf("%s with empty ref", kind)
		}
		if refs[ref] {
			return fmt.Errorf("duplicate ref %q", ref)
		}
		refs[ref] = true
		return nil
	}

	sgRefs, cgRefs := map[string]bool{}, map[string]bool{}
	for _, g := range f.ServerGroups {
		if err := addRef("server group", g.Ref); err != nil {
			return err
		}
		sgRefs[g.Ref] = true
	}
	for _, g := range f.ChannelGroups {
		if err := addRef("channel group", g.Ref); err != nil {
			return err
		}
		cgRefs[g.Ref] = true
	}
	for name, ref := range map[string]string{"default_server_group": f.Server.DefaultServerGroup} {
		if !sgRefs[ref] {
			return fmt.Errorf("server.%s %q is not a server group in this backup", name, ref)
		}
	}
	for name, ref := range map[string]string{
		"default_channel_group":       f.Server.DefaultChannelGroup,
		"default_channel_admin_group": f.Server.DefaultChannelAdminGroup,
	} {
		if !cgRefs[ref] {
			return fmt.Errorf("server.%s %q is not a channel group in this backup", name, ref)
		}
	}

	defaults := 0
	err := Walk(f.Channels, func(ch, _ *Channel) error {
		if err := addRef("channel", ch.Ref); err != nil {
			return err
		}
		if ch.IsDefault() {
			defaults++
		}
		return nil
	})
	if err != nil {
		return err
	}
	if defaults != 1 {
		return fmt.Errorf("backup must contain exactly one default channel, found %d", defaults)
	}
	return nil
}
