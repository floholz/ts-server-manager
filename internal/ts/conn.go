package ts

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
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
	return connectRetry(ctx, cfg, d, sleepCtx)
}

// connectRetry dials once, and once more if the server's connection flood
// protection asked for a short wait ("ssh: disconnect, reason 11: please
// wait 1 seconds"), which happens when a command runs right after another
// one. Long waits mean the IP is blocked; those fail immediately.
func connectRetry(ctx context.Context, cfg Config, d dialer, sleep func(context.Context, time.Duration) bool) (*Conn, error) {
	cfg = applyDefaults(cfg)
	tr, err := d.dial(ctx, cfg)
	if wait, ok := dialFloodWait(err); ok && wait < maxFloodWait {
		if !sleep(ctx, wait+time.Second) {
			return nil, ctx.Err()
		}
		tr, err = d.dial(ctx, cfg)
	}
	if err != nil {
		if _, ok := dialFloodWait(err); ok {
			return nil, fmt.Errorf("%w: connecting was refused: add this host's IP to the "+
				"TeamSpeak query allowlist (query_ip_allowlist.txt) or wait a few minutes: %v", ErrFlooding, err)
		}
		if isHandshakeEOF(err) {
			return nil, fmt.Errorf("%w (the server closed the connection during the SSH handshake; with TeamSpeak "+
				"this usually means this IP is temporarily blocked by query flood protection: wait ~10 minutes, "+
				"or add the IP to query_ip_allowlist.txt)", err)
		}
		return nil, err
	}
	return &Conn{tr: tr}, nil
}

func isHandshakeEOF(err error) bool {
	s := err.Error()
	return strings.Contains(s, "handshake failed") && strings.HasSuffix(s, "EOF")
}

// dialFloodWait reports whether a dial error is the connection flood
// protection, and how long it asked to wait.
func dialFloodWait(err error) (time.Duration, bool) {
	if err == nil {
		return 0, false
	}
	m := pleaseWaitRe.FindStringSubmatch(err.Error())
	if m == nil {
		return 0, false
	}
	n, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0, false
	}
	return time.Duration(n) * time.Second, true
}

// Exec runs a single command and returns its rows. Error replies from the
// server are returned as *QueryError.
func (c *Conn) Exec(cmd *Command) ([]Row, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return execRows(c.tr, cmd)
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
