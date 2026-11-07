package ts

import (
	"fmt"
	"strconv"
	"strings"
)

// Command builds a single ServerQuery command line.
//
// Parameters are added to the current parameter group with Arg. Next starts
// a new group; groups are joined with '|', which is how ServerQuery batches
// several items into one command, e.g.
//
//	servergroupaddperm sgid=6 permsid=a permvalue=1 permnegated=0 permskip=0|permsid=b permvalue=1 ...
//
// Options (flags such as -permsid) are appended after all parameters.
// Keys and values are escaped as the protocol requires.
type Command struct {
	name    string
	groups  [][]param
	options []string
	raw     string // set by RawCommand: sent verbatim
}

type param struct {
	key, val string
}

// NewCommand starts a command with the given name (e.g. "channellist").
func NewCommand(name string) *Command {
	return &Command{name: name, groups: [][]param{nil}}
}

// RawCommand wraps a command line that is already in ServerQuery syntax
// (values escaped by the caller) so it can go through the same Execer
// chain, e.g. a Pacer. Arg, Next and Option have no effect on it. Intended
// for the interactive `query` subcommand; code should build commands with
// NewCommand.
func RawCommand(line string) *Command {
	line = strings.TrimSpace(line)
	name, _, _ := strings.Cut(line, " ")
	return &Command{name: name, groups: [][]param{nil}, raw: line}
}

// Arg adds key=val to the current parameter group. Booleans are sent as
// 1/0; every other value is formatted with fmt.Sprint.
func (c *Command) Arg(key string, val any) *Command {
	var s string
	switch v := val.(type) {
	case bool:
		if v {
			s = "1"
		} else {
			s = "0"
		}
	case string:
		s = v
	default:
		s = fmt.Sprint(v)
	}
	last := len(c.groups) - 1
	c.groups[last] = append(c.groups[last], param{key: key, val: s})
	return c
}

// Next starts a new '|'-separated parameter group. Calling Next on an empty
// current group is a no-op, so loops can call it unconditionally.
func (c *Command) Next() *Command {
	if len(c.groups[len(c.groups)-1]) == 0 {
		return c
	}
	c.groups = append(c.groups, nil)
	return c
}

// Option appends a flag such as "-permsid". A leading '-' is added if missing.
func (c *Command) Option(opt string) *Command {
	if !strings.HasPrefix(opt, "-") {
		opt = "-" + opt
	}
	c.options = append(c.options, opt)
	return c
}

// Name returns the command name, used for logging and error context.
func (c *Command) Name() string { return c.name }

// String renders the command line, without a trailing newline.
func (c *Command) String() string {
	if c.raw != "" {
		return c.raw
	}
	var b strings.Builder
	b.WriteString(c.name)

	var groups []string
	for _, g := range c.groups {
		if len(g) == 0 {
			continue
		}
		parts := make([]string, len(g))
		for i, p := range g {
			parts[i] = Escape(p.key) + "=" + Escape(p.val)
		}
		groups = append(groups, strings.Join(parts, " "))
	}
	if len(groups) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(groups, "|"))
	}
	for _, o := range c.options {
		b.WriteByte(' ')
		b.WriteString(o)
	}
	return b.String()
}

var (
	escaper = strings.NewReplacer(
		`\`, `\\`,
		`/`, `\/`,
		` `, `\s`,
		`|`, `\p`,
		"\a", `\a`,
		"\b", `\b`,
		"\f", `\f`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
		"\v", `\v`,
	)
	unescaper = strings.NewReplacer(
		`\\`, `\`,
		`\/`, `/`,
		`\s`, ` `,
		`\p`, `|`,
		`\a`, "\a",
		`\b`, "\b",
		`\f`, "\f",
		`\n`, "\n",
		`\r`, "\r",
		`\t`, "\t",
		`\v`, "\v",
	)
)

// Escape encodes s for use as a ServerQuery key or value.
func Escape(s string) string { return escaper.Replace(s) }

// Unescape decodes a ServerQuery-encoded key or value.
func Unescape(s string) string { return unescaper.Replace(s) }

// Row is one record of a ServerQuery response. Keys and values are
// unescaped; a key sent without '=' maps to "".
type Row map[string]string

// String returns the value for key, or "" when absent.
func (r Row) String(key string) string { return r[key] }

// Int returns the value for key parsed as an int. Missing or malformed
// values return 0 and false.
func (r Row) Int(key string) (int, bool) {
	v, ok := r[key]
	if !ok {
		return 0, false
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return i, true
}

// IntOr returns the value for key parsed as an int, or def.
func (r Row) IntOr(key string, def int) int {
	if i, ok := r.Int(key); ok {
		return i
	}
	return def
}

// ParseRows decodes the data lines of a ServerQuery response into rows.
// Records are separated by '|', fields by ' '. Multiple data lines are
// treated as a continuation of the same record list.
func ParseRows(lines []string) []Row {
	var rows []Row
	for _, line := range lines {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		for _, rec := range strings.Split(line, "|") {
			row := Row{}
			for _, field := range strings.Split(rec, " ") {
				if field == "" {
					continue
				}
				k, v, _ := strings.Cut(field, "=")
				row[Unescape(k)] = Unescape(v)
			}
			if len(row) > 0 {
				rows = append(rows, row)
			}
		}
	}
	return rows
}
