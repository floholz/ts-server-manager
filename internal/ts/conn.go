package ts

import (
	"context"
	"strings"
	"sync"
)

// Conn is a single ServerQuery session without reconnect logic, for
// one-shot CLI commands (backup, restore, query). If the connection breaks,
// commands fail and the caller decides what to do; nothing is retried
// behind its back.
//
// Conn is safe for concurrent use; commands are serialized.
type Conn struct {
	mu sync.Mutex
	tr transport
}

// Connect opens a session to cfg's server and selects cfg.SID.
func Connect(ctx context.Context, cfg Config) (*Conn, error) {
	return connectWith(ctx, cfg, sshDialer{})
}

func connectWith(ctx context.Context, cfg Config, d dialer) (*Conn, error) {
	tr, err := d.dial(ctx, applyDefaults(cfg))
	if err != nil {
		return nil, err
	}
	return &Conn{tr: tr}, nil
}

// Exec runs a single command and returns its rows. Error replies from the
// server are returned as *QueryError.
func (c *Conn) Exec(cmd *Command) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return execRows(c.tr, cmd)
}

// ExecRaw sends line exactly as given (already in ServerQuery syntax, with
// values escaped by the caller) and returns the parsed rows. Intended for
// the interactive `query` subcommand; code should use Exec with a Command.
func (c *Conn) ExecRaw(line string) ([]Row, error) {
	line = strings.TrimSpace(line)
	name, _, _ := strings.Cut(line, " ")
	c.mu.Lock()
	defer c.mu.Unlock()
	return execLine(c.tr, line, name)
}

// Version returns the server's version metadata.
func (c *Conn) Version() (VersionInfo, error) {
	return queryVersion(c.Exec)
}

// ServerInfo returns the aggregated server metadata.
func (c *Conn) ServerInfo() (ServerInfo, error) {
	return queryServerInfo(c.Exec)
}

// Close ends the session.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tr.Close()
}
