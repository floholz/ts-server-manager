package ts

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Client is a long-lived TeamSpeak ServerQuery client wrapper.
//
// Start a single instance per process; it connects in the background and
// reconnects automatically on transport failure.
//
// The underlying ts3.Client is not safe for concurrent use: all callers
// share a single response channel, so two in-flight commands can receive
// each other's replies. Every command (handler calls and the keepalive)
// therefore runs under callMu. State (the current transport) is guarded by
// the separate mu so IsConnected never blocks behind a slow query.
type Client struct {
	cfg    Config
	dialer dialer
	logger *slog.Logger

	// callMu serializes all commands sent over the transport.
	callMu sync.Mutex

	// mu guards tr.
	mu sync.Mutex
	tr transport

	// reconnectCh is buffered (cap 1) and used as a one-shot signal: the
	// connect loop drains it, drops the current transport, and dials again.
	reconnectCh chan struct{}
}

// New constructs a Client with the real SSH dialer.
func New(cfg Config, logger *slog.Logger) *Client {
	return newWithDialer(cfg, sshDialer{}, withLogger(logger))
}

func newWithDialer(cfg Config, d dialer, opts ...option) *Client {
	cfg = applyDefaults(cfg)
	c := &Client{
		cfg:         cfg,
		dialer:      d,
		logger:      slog.Default(),
		reconnectCh: make(chan struct{}, 1),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

type option func(*Client)

func withLogger(l *slog.Logger) option {
	return func(c *Client) {
		if l != nil {
			c.logger = l
		}
	}
}

func applyDefaults(cfg Config) Config {
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.KeepAliveInterval == 0 {
		cfg.KeepAliveInterval = 3 * time.Minute
	}
	if cfg.BackoffMin == 0 {
		cfg.BackoffMin = 1 * time.Second
	}
	if cfg.BackoffMax == 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	return cfg
}

// Start launches the connect/reconnect loop. It returns immediately; the
// loop runs until ctx is cancelled.
func (c *Client) Start(ctx context.Context) {
	go c.runLoop(ctx)
}

// IsConnected reports whether a usable transport is currently held.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tr != nil
}

// Version returns the TS version metadata, querying the live connection.
// Returns ErrUnavailable if not connected or the call fails at the
// transport layer.
func (c *Client) Version() (VersionInfo, error) {
	var v VersionInfo
	err := c.withTransport("Version", func(tr transport) error {
		var err error
		v, err = tr.Version()
		if err != nil {
			return fmt.Errorf("version: %w", err)
		}
		return nil
	})
	return v, err
}

// ServerInfo aggregates `version`, `whoami`, and `serverinfo` into the
// public ServerInfo type. The three commands run back to back under one
// lock. ErrUnavailable is returned when not connected or any call fails at
// the transport layer.
func (c *Client) ServerInfo() (ServerInfo, error) {
	var out ServerInfo
	err := c.withTransport("ServerInfo", func(tr transport) error {
		v, err := tr.Version()
		if err != nil {
			return fmt.Errorf("version: %w", err)
		}
		w, err := tr.Whoami()
		if err != nil {
			return fmt.Errorf("whoami: %w", err)
		}
		si, err := tr.ServerInfo()
		if err != nil {
			return fmt.Errorf("serverinfo: %w", err)
		}
		out = ServerInfo{
			Version:       v.Version,
			Platform:      v.Platform,
			Build:         v.Build,
			SID:           w.ServerID,
			ServerStatus:  w.ServerStatus,
			ServerName:    si.Name,
			UptimeSeconds: si.UptimeSeconds,
			ClientsOnline: si.ClientsOnline,
			ClientsMax:    si.ClientsMax,
			LoginName:     w.ClientLoginName,
		}
		return nil
	})
	return out, err
}

// withTransport runs fn against the current transport while holding callMu,
// so no other command can be in flight on the wire. If fn fails, the
// transport is dropped and the connect loop is signalled to reconnect; the
// error is returned wrapped in ErrUnavailable.
func (c *Client) withTransport(where string, fn func(transport) error) error {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	tr := c.currentTransport()
	if tr == nil {
		return ErrUnavailable
	}
	if err := fn(tr); err != nil {
		c.markUnavailable(tr, where, err)
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}

func (c *Client) currentTransport() transport {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tr
}

func (c *Client) setTransport(tr transport) {
	c.mu.Lock()
	old := c.tr
	c.tr = tr
	if old != nil {
		_ = old.Close()
	}
	c.mu.Unlock()
}

// markUnavailable drops failed (if it is still the current transport) and
// signals the connect loop to reconnect. Comparing against the failed
// transport ensures a stale failure never tears down a newer connection.
// Safe to call from any goroutine.
func (c *Client) markUnavailable(failed transport, reason string, err error) {
	c.mu.Lock()
	if c.tr != nil && c.tr == failed {
		_ = c.tr.Close()
		c.tr = nil
	}
	c.mu.Unlock()

	c.logger.Warn("ts.transport_failed", "where", reason, "err", err.Error())

	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

func (c *Client) runLoop(ctx context.Context) {
	backoff := c.cfg.BackoffMin
	for {
		if ctx.Err() != nil {
			c.setTransport(nil)
			return
		}

		tr, err := c.dialer.dial(ctx, c.cfg)
		if err != nil {
			c.logger.Warn("ts.dial_failed", "err", err.Error(), "retry_in", backoff.String())
			if !sleepCtx(ctx, backoff) {
				c.setTransport(nil)
				return
			}
			backoff = nextBackoff(backoff, c.cfg.BackoffMax)
			continue
		}

		// Drain any reconnect signal left over from the previous connection
		// so it doesn't immediately tear down the new one.
		select {
		case <-c.reconnectCh:
		default:
		}

		c.setTransport(tr)
		c.logger.Info("ts.connected", "host", c.cfg.Host, "port", c.cfg.Port, "sid", c.cfg.SID)
		backoff = c.cfg.BackoffMin

		c.runConnected(ctx)
	}
}

// runConnected ticks the keepalive and watches for reconnect signals.
// Returns when the connection is no longer usable or ctx is cancelled.
func (c *Client) runConnected(ctx context.Context) {
	t := time.NewTicker(c.cfg.KeepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.reconnectCh:
			return
		case <-t.C:
			// The keepalive goes through withTransport like any other
			// command, so it can never interleave with a handler call.
			err := c.withTransport("keepalive", func(tr transport) error {
				_, err := tr.Version()
				return err
			})
			if err != nil {
				return
			}
		}
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		next = max
	}
	return next
}

// sleepCtx sleeps for d or until ctx is cancelled. Returns false on cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
