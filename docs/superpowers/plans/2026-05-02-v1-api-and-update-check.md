# v1 API and Update Check — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the v1 ts-server-manager: a small sidecar that holds a long-lived TeamSpeak 3 ServerQuery connection and exposes liveness, readiness, server-info, and update-check HTTP endpoints — fulfilling the spec at `docs/superpowers/specs/2026-05-02-api-and-update-check-design.md`.

**Architecture:** Single Go binary, four loosely-coupled internal packages (`config`, `ts`, `dockerhub`, `api`). The HTTP layer depends on consumer-side interfaces satisfied by the concrete `ts.Client` and `dockerhub.Client`. `main.go` wires the units, runs the HTTP server, and handles signals. A second mode (`ts-server-manager healthcheck`) issues a localhost readiness probe for the Docker `HEALTHCHECK` directive.

**Tech Stack:** Go 1.22 stdlib (`net/http`, `log/slog`, `context`, `crypto/rand`), `github.com/multiplay/go-ts3 v1.2.0` (upstream — drop the `replace` directive that previously pointed at the `rocketsciencegg/go-ts3` fork), `golang.org/x/crypto/ssh`. No new external dependencies are required.

---

## File Structure

### New files
| Path | Purpose |
|------|---------|
| `internal/version/version.go` | `var Version = "dev"` — overridable via `-ldflags` |
| `internal/config/config.go` | Env-driven `Config` + `Load()` with validation |
| `internal/config/config_test.go` | `Load()` validation tests |
| `internal/ts/types.go` | Public `VersionInfo`, `ServerInfo`, sentinel errors |
| `internal/ts/transport.go` | `transport` interface + real `sshDialer` |
| `internal/ts/client.go` | Long-lived `Client` with reconnect loop + keepalive |
| `internal/ts/client_test.go` | State machine tests with a fake transport |
| `internal/dockerhub/client.go` | Docker Hub digest checker + 15-minute cache |
| `internal/dockerhub/client_test.go` | Tests using `httptest.Server` |
| `internal/api/api.go` | `Register()`, handler funcs, consumer interfaces |
| `internal/api/middleware.go` | Request ID, access logging, panic recovery |
| `internal/api/api_test.go` | Handler tests with fake `TSClient`/`DockerHubClient` |

### Modified files
| Path | Change |
|------|--------|
| `main.go` | Rewrite: subcommand dispatch (default = serve, `healthcheck`), config load, wire units, `http.Server`, signal handling |
| `Dockerfile` | Add `EXPOSE 9988` and `HEALTHCHECK` directive |
| `compose.yaml` | Add `ports: ["9988:9988"]` |
| `.env.example` | Document `HTTP_ADDR`, `LOG_LEVEL`, `DOCKERHUB_REPO`, `UPDATE_CHECK_TTL` |
| `go.mod` | Drop the `replace` directive; pin `github.com/multiplay/go-ts3 v1.2.0`; `go mod tidy` |

---

## Task 1: Bootstrap — pin upstream go-ts3, add version package

**Files:**
- Modify: `go.mod`
- Create: `internal/version/version.go`

- [ ] **Step 1: Replace `go.mod` with upstream go-ts3 v1.2.0**

Open `go.mod` and:
1. Remove the `replace github.com/multiplay/go-ts3 => github.com/rocketsciencegg/go-ts3 ...` line.
2. Pin `github.com/multiplay/go-ts3` to `v1.2.0`.

Resulting `go.mod`:

```go
module github.com/floholz/ts-server-manager

go 1.22

require (
	github.com/multiplay/go-ts3 v1.2.0
	golang.org/x/crypto v0.31.0
)
```

(Indirect dependencies will be filled in by `go mod tidy`.)

- [ ] **Step 2: Tidy and download**

Run: `go mod tidy`
Expected: `go.sum` is created/updated; `go.mod` may gain an `// indirect` block.

Then: `go build ./...`
Expected: builds clean against upstream go-ts3 v1.2.0. The existing `main.go` already uses the API that v1.2.0 exposes (`NewClient`, `SSH`, `Version`, `Use`, `Whoami`, `Close`).

- [ ] **Step 3: Create the version package**

`internal/version/version.go`:

```go
// Package version exposes the build-time version of the binary.
package version

// Version is overridden at build time via -ldflags="-X github.com/floholz/ts-server-manager/internal/version.Version=v0.1.0".
var Version = "dev"
```

- [ ] **Step 4: Verify it builds**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/version/version.go
git commit -m "build: pin upstream multiplay/go-ts3 v1.2.0 and add version package"
```

---

## Task 2: Config package — types and tests

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Write the config types and stub `Load()`**

`internal/config/config.go`:

```go
// Package config loads the manager's runtime configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TSHost           string
	TSPort           string
	TSUser           string
	TSPassword       string
	TSSID            int
	HTTPAddr         string
	LogLevel         string
	DockerHubRepo    string
	UpdateCheckTTL   time.Duration
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		TSHost:         getenv("TS_HOST"),
		TSPort:         envOr(getenv, "TS_PORT", "10022"),
		TSUser:         getenv("TS_USER"),
		TSPassword:     getenv("TS_PASSWORD"),
		HTTPAddr:       envOr(getenv, "HTTP_ADDR", ":9988"),
		LogLevel:       envOr(getenv, "LOG_LEVEL", "info"),
		DockerHubRepo:  envOr(getenv, "DOCKERHUB_REPO", "teamspeaksystems/teamspeak6-server"),
	}

	sidStr := envOr(getenv, "TS_SID", "1")
	sid, err := strconv.Atoi(sidStr)
	if err != nil {
		return cfg, fmt.Errorf("TS_SID must be an integer, got %q", sidStr)
	}
	cfg.TSSID = sid

	ttlStr := envOr(getenv, "UPDATE_CHECK_TTL", "15m")
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil {
		return cfg, fmt.Errorf("UPDATE_CHECK_TTL must be a Go duration (e.g. 15m), got %q", ttlStr)
	}
	if ttl < 0 {
		return cfg, fmt.Errorf("UPDATE_CHECK_TTL must be non-negative, got %s", ttl)
	}
	cfg.UpdateCheckTTL = ttl

	var missing []string
	if cfg.TSHost == "" {
		missing = append(missing, "TS_HOST")
	}
	if cfg.TSUser == "" {
		missing = append(missing, "TS_USER")
	}
	if cfg.TSPassword == "" {
		missing = append(missing, "TS_PASSWORD")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// LoadFromOS is a convenience wrapper that reads from os.Getenv.
func LoadFromOS() (Config, error) {
	return Load(os.Getenv)
}

func envOr(getenv func(string) string, key, fallback string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return fallback
}
```

- [ ] **Step 2: Write the failing tests**

`internal/config/config_test.go`:

```go
package config

import (
	"strings"
	"testing"
	"time"
)

func envFromMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(envFromMap(map[string]string{
		"TS_HOST":     "ts.example.com",
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TSPort != "10022" {
		t.Errorf("TSPort default: got %q, want %q", cfg.TSPort, "10022")
	}
	if cfg.TSSID != 1 {
		t.Errorf("TSSID default: got %d, want 1", cfg.TSSID)
	}
	if cfg.HTTPAddr != ":9988" {
		t.Errorf("HTTPAddr default: got %q, want %q", cfg.HTTPAddr, ":9988")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %q", cfg.LogLevel)
	}
	if cfg.DockerHubRepo != "teamspeaksystems/teamspeak6-server" {
		t.Errorf("DockerHubRepo default: got %q", cfg.DockerHubRepo)
	}
	if cfg.UpdateCheckTTL != 15*time.Minute {
		t.Errorf("UpdateCheckTTL default: got %s, want 15m", cfg.UpdateCheckTTL)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
	}))
	if err == nil {
		t.Fatal("expected error for missing TS_HOST")
	}
	if !strings.Contains(err.Error(), "TS_HOST") {
		t.Errorf("error should mention TS_HOST: %v", err)
	}
}

func TestLoad_InvalidSID(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":     "ts.example.com",
		"TS_USER":     "serveradmin",
		"TS_PASSWORD": "secret",
		"TS_SID":      "not-a-number",
	}))
	if err == nil || !strings.Contains(err.Error(), "TS_SID") {
		t.Fatalf("expected TS_SID error, got: %v", err)
	}
}

func TestLoad_InvalidTTL(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":          "ts.example.com",
		"TS_USER":          "serveradmin",
		"TS_PASSWORD":      "secret",
		"UPDATE_CHECK_TTL": "fifteen-minutes",
	}))
	if err == nil || !strings.Contains(err.Error(), "UPDATE_CHECK_TTL") {
		t.Fatalf("expected UPDATE_CHECK_TTL error, got: %v", err)
	}
}

func TestLoad_NegativeTTLRejected(t *testing.T) {
	_, err := Load(envFromMap(map[string]string{
		"TS_HOST":          "ts.example.com",
		"TS_USER":          "serveradmin",
		"TS_PASSWORD":      "secret",
		"UPDATE_CHECK_TTL": "-5m",
	}))
	if err == nil {
		t.Fatal("expected error for negative TTL")
	}
}
```

- [ ] **Step 3: Run the tests**

Run: `go test ./internal/config/ -v`
Expected: all 5 tests PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/config/
git commit -m "feat(config): env-driven configuration with validation"
```

---

## Task 3: TS package — types and transport interface

**Files:**
- Create: `internal/ts/types.go`
- Create: `internal/ts/transport.go`

- [ ] **Step 1: Define the public types and errors**

`internal/ts/types.go`:

```go
// Package ts wraps the TeamSpeak 3 ServerQuery client with a long-lived,
// auto-reconnecting connection plus accessors for /api/server-info and
// /api/update-check.
package ts

import (
	"errors"
	"time"
)

// Config is the connection-time configuration for the long-lived client.
type Config struct {
	Host       string
	Port       string
	User       string
	Password   string
	SID        int

	// DialTimeout bounds the SSH handshake + initial protocol exchange.
	// Defaults to 10 seconds when zero.
	DialTimeout time.Duration

	// KeepAliveInterval is how often the wrapper pings the server with a
	// `version` ServerQuery to detect logical disconnects. Defaults to 3
	// minutes when zero.
	KeepAliveInterval time.Duration

	// BackoffMin and BackoffMax bound the exponential reconnect backoff.
	// Default 1s and 30s.
	BackoffMin time.Duration
	BackoffMax time.Duration
}

// VersionInfo mirrors the TS3 `version` response.
type VersionInfo struct {
	Version  string
	Platform string
	Build    int
}

// ServerInfo is the aggregated payload for /api/server-info.
type ServerInfo struct {
	Version       string
	Platform      string
	Build         int
	SID           int
	ServerStatus  string
	ServerName    string
	UptimeSeconds int64
	ClientsOnline int
	ClientsMax    int
	LoginName     string
}

// ErrUnavailable is returned when the TS connection is down or the
// command failed at the transport layer. The API maps it to HTTP 503.
var ErrUnavailable = errors.New("ts: unavailable")
```

- [ ] **Step 2: Define the transport interface and real dialer**

`internal/ts/transport.go`:

```go
package ts

import (
	"context"
	"fmt"

	ts3 "github.com/multiplay/go-ts3"
	"golang.org/x/crypto/ssh"
)

// transport is the minimal surface the Client needs from a connected
// ServerQuery session. Tests substitute a fake.
type transport interface {
	Version() (VersionInfo, error)
	Whoami() (whoami, error)
	ServerInfo() (rawServerInfo, error)
	Close() error
}

type whoami struct {
	ServerStatus    string
	ServerID        int
	ClientLoginName string
}

type rawServerInfo struct {
	Name          string
	UptimeSeconds int64
	ClientsOnline int
	ClientsMax    int
	Status        string
	Version       string
	Platform      string
}

// dialer abstracts the act of opening a connected transport. Tests
// substitute a fake.
type dialer interface {
	dial(ctx context.Context, cfg Config) (transport, error)
}

// sshDialer dials a real TeamSpeak ServerQuery over SSH and selects the
// configured virtual server.
type sshDialer struct{}

func (sshDialer) dial(ctx context.Context, cfg Config) (transport, error) {
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.Password(cfg.Password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         cfg.DialTimeout,
	}

	addr := cfg.Host + ":" + cfg.Port
	c, err := ts3.NewClient(addr,
		ts3.SSH(sshCfg),
		ts3.Timeout(cfg.DialTimeout),
		ts3.KeepAlive(cfg.KeepAliveInterval),
	)
	if err != nil {
		return nil, fmt.Errorf("dial ts3: %w", err)
	}

	if err := c.Use(cfg.SID); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("use sid=%d: %w", cfg.SID, err)
	}

	_ = ctx // ts3 library doesn't accept a Context yet; reserved for future use.
	return &ts3Transport{c: c}, nil
}

// ts3Transport adapts *ts3.Client to the transport interface.
type ts3Transport struct {
	c *ts3.Client
}

func (t *ts3Transport) Version() (VersionInfo, error) {
	v, err := t.c.Version()
	if err != nil {
		return VersionInfo{}, err
	}
	return VersionInfo{Version: v.Version, Platform: v.Platform, Build: v.Build}, nil
}

func (t *ts3Transport) Whoami() (whoami, error) {
	w, err := t.c.Whoami()
	if err != nil {
		return whoami{}, err
	}
	return whoami{
		ServerStatus:    w.ServerStatus,
		ServerID:        w.ServerID,
		ClientLoginName: w.ClientLoginName,
	}, nil
}

func (t *ts3Transport) ServerInfo() (rawServerInfo, error) {
	s, err := t.c.Server.Info()
	if err != nil {
		return rawServerInfo{}, err
	}
	return rawServerInfo{
		Name:          s.Name,
		UptimeSeconds: int64(s.Uptime),
		ClientsOnline: s.ClientsOnline,
		ClientsMax:    s.MaxClients,
		Status:        s.Status,
		Version:       s.Version,
		Platform:      s.Platform,
	}, nil
}

func (t *ts3Transport) Close() error {
	return t.c.Close()
}
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./internal/ts/...`
Expected: no output, exit 0. (No tests yet.)

- [ ] **Step 4: Commit**

```bash
git add internal/ts/types.go internal/ts/transport.go
git commit -m "feat(ts): types, transport interface, and SSH dialer"
```

---

## Task 4: TS package — Client struct and connection state machine (TDD)

**Files:**
- Create: `internal/ts/client.go`
- Create: `internal/ts/client_test.go`

- [ ] **Step 1: Write the failing tests for the state machine**

`internal/ts/client_test.go`:

```go
package ts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTransport is a programmable transport for state-machine tests.
type fakeTransport struct {
	mu          sync.Mutex
	versionResp VersionInfo
	versionErr  error
	whoamiResp  whoami
	whoamiErr   error
	siResp      rawServerInfo
	siErr       error
	closed      bool
}

func (f *fakeTransport) Version() (VersionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.versionResp, f.versionErr
}
func (f *fakeTransport) Whoami() (whoami, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.whoamiResp, f.whoamiErr
}
func (f *fakeTransport) ServerInfo() (rawServerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.siResp, f.siErr
}
func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeDialer returns a queue of (transport, error) results, in order.
type fakeDialer struct {
	mu      sync.Mutex
	queue   []dialResult
	calls   atomic.Int32
}
type dialResult struct {
	t   transport
	err error
}

func (d *fakeDialer) dial(ctx context.Context, cfg Config) (transport, error) {
	d.calls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queue) == 0 {
		// Block forever simulating a hang; tests should cancel the context.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := d.queue[0]
	d.queue = d.queue[1:]
	return r.t, r.err
}

func newTestClient(d dialer) *Client {
	return newWithDialer(Config{
		Host: "x", Port: "1", User: "u", Password: "p", SID: 1,
		DialTimeout:       50 * time.Millisecond,
		KeepAliveInterval: 20 * time.Millisecond,
		BackoffMin:        5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
	}, d)
}

func TestClient_ConnectsAndIsReady(t *testing.T) {
	tr := &fakeTransport{versionResp: VersionInfo{Version: "6.0.0-beta9"}}
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newTestClient(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reported connected")
	}
}

func TestClient_ReconnectsAfterDialError(t *testing.T) {
	tr := &fakeTransport{}
	d := &fakeDialer{queue: []dialResult{
		{err: errors.New("boom")},
		{t: tr},
	}}
	c := newTestClient(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	if !waitFor(200*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reconnected after initial dial error")
	}
	if d.calls.Load() < 2 {
		t.Errorf("expected at least 2 dial attempts, got %d", d.calls.Load())
	}
}

func TestClient_KeepaliveFailureTriggersReconnect(t *testing.T) {
	first := &fakeTransport{versionErr: errors.New("io: broken pipe")}
	second := &fakeTransport{} // healthy
	d := &fakeDialer{queue: []dialResult{{t: first}, {t: second}}}
	c := newTestClient(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	// Wait for the second connection (after keepalive fails on the first).
	if !waitFor(300*time.Millisecond, func() bool {
		return d.calls.Load() >= 2 && c.IsConnected()
	}) {
		t.Fatal("client did not reconnect after keepalive failure")
	}
	if !first.closed {
		t.Error("first transport should have been closed after failure")
	}
}

func TestClient_HandlerErrorMarksDisconnected(t *testing.T) {
	tr := &fakeTransport{}
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newTestClient(d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)

	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reported connected")
	}

	// Inject a transport error into the next call.
	tr.mu.Lock()
	tr.versionErr = errors.New("io: connection reset")
	tr.mu.Unlock()

	_, err := c.Version()
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
	if !waitFor(100*time.Millisecond, func() bool { return !c.IsConnected() }) {
		t.Fatal("client should report disconnected after transport error")
	}
}

func TestClient_ContextCancelExits(t *testing.T) {
	tr := &fakeTransport{}
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newTestClient(d)

	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)

	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("connect timeout")
	}
	cancel()

	if !waitFor(100*time.Millisecond, func() bool { return !c.IsConnected() }) {
		t.Fatal("client did not disconnect after context cancel")
	}
	if !tr.closed {
		t.Error("transport not closed on shutdown")
	}
}

// waitFor polls cond every 5ms up to d. Returns true if cond ever returned true.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/ts/ -run . -v`
Expected: compile error (`undefined: newWithDialer`, `Client`, etc.) — that's our cue to implement.

- [ ] **Step 3: Implement the Client**

`internal/ts/client.go`:

```go
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
// reconnects automatically on transport failure. Concurrent calls are
// serialized through an internal mutex (the underlying ts3.Client is not
// thread-safe).
type Client struct {
	cfg    Config
	dialer dialer
	logger *slog.Logger

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

// Version returns the cached TS version metadata, querying the live
// connection. Returns ErrUnavailable if not connected or the call fails
// at the transport layer.
func (c *Client) Version() (VersionInfo, error) {
	tr := c.currentTransport()
	if tr == nil {
		return VersionInfo{}, ErrUnavailable
	}
	v, err := tr.Version()
	if err != nil {
		c.markUnavailable("Version", err)
		return VersionInfo{}, fmt.Errorf("%w: version: %v", ErrUnavailable, err)
	}
	return v, nil
}

// ServerInfo aggregates `version`, `whoami`, and `serverinfo` into the
// public ServerInfo type. ErrUnavailable is returned when not connected
// or any call fails at the transport layer.
func (c *Client) ServerInfo() (ServerInfo, error) {
	tr := c.currentTransport()
	if tr == nil {
		return ServerInfo{}, ErrUnavailable
	}
	v, err := tr.Version()
	if err != nil {
		c.markUnavailable("ServerInfo.version", err)
		return ServerInfo{}, fmt.Errorf("%w: version: %v", ErrUnavailable, err)
	}
	w, err := tr.Whoami()
	if err != nil {
		c.markUnavailable("ServerInfo.whoami", err)
		return ServerInfo{}, fmt.Errorf("%w: whoami: %v", ErrUnavailable, err)
	}
	si, err := tr.ServerInfo()
	if err != nil {
		c.markUnavailable("ServerInfo.serverinfo", err)
		return ServerInfo{}, fmt.Errorf("%w: serverinfo: %v", ErrUnavailable, err)
	}
	return ServerInfo{
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
	}, nil
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
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// markUnavailable drops the current transport and signals the connect
// loop to reconnect. Safe to call from any goroutine.
func (c *Client) markUnavailable(reason string, err error) {
	c.mu.Lock()
	if c.tr != nil {
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

		c.setTransport(tr)
		c.logger.Info("ts.connected", "host", c.cfg.Host, "port", c.cfg.Port, "sid", c.cfg.SID)
		backoff = c.cfg.BackoffMin

		c.runConnected(ctx, tr)
	}
}

// runConnected ticks the keepalive and watches for reconnect signals.
// Returns when the connection is no longer usable or ctx is cancelled.
func (c *Client) runConnected(ctx context.Context, tr transport) {
	t := time.NewTicker(c.cfg.KeepAliveInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.reconnectCh:
			return
		case <-t.C:
			if _, err := tr.Version(); err != nil {
				c.markUnavailable("keepalive", err)
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
```

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/ts/ -race -v`
Expected: all 5 tests PASS, no race warnings.

- [ ] **Step 5: Commit**

```bash
git add internal/ts/client.go internal/ts/client_test.go
git commit -m "feat(ts): long-lived client with reconnect and keepalive"
```

---

## Task 5: dockerhub package — types and tests

**Files:**
- Create: `internal/dockerhub/client.go`
- Create: `internal/dockerhub/client_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/dockerhub/client_test.go`:

```go
package dockerhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubServer collects request paths and serves canned responses keyed by path.
type stubServer struct {
	mu        sync.Mutex
	responses map[string]stubResponse
	calls     atomic.Int32
	pathLog   []string
}

type stubResponse struct {
	status int
	body   string
}

func newStub() *stubServer {
	return &stubServer{responses: map[string]stubResponse{}}
}

func (s *stubServer) set(path string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responses[path] = stubResponse{status: status, body: body}
}

func (s *stubServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.mu.Lock()
		s.pathLog = append(s.pathLog, r.URL.Path+"?"+r.URL.RawQuery)
		resp, ok := s.responses[r.URL.Path]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "no stub", http.StatusNotFound)
			return
		}
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	})
}

func tagJSON(name, digest string) string {
	b, _ := json.Marshal(map[string]any{"name": name, "digest": digest})
	return string(b)
}

func tagListJSON(items []map[string]string) string {
	results := make([]map[string]string, 0, len(items))
	for _, it := range items {
		results = append(results, it)
	}
	b, _ := json.Marshal(map[string]any{"results": results})
	return string(b)
}

func newTestClient(t *testing.T, baseURL string, ttl time.Duration) *Client {
	c := New(Config{
		Repo:    "teamspeaksystems/teamspeak6-server",
		BaseURL: baseURL,
		TTL:     ttl,
	})
	t.Cleanup(func() {})
	return c
}

func TestCheck_DigestsMatchNoUpdate(t *testing.T) {
	stub := newStub()
	digest := "sha256:aaa"
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", digest))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", digest))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.UpdateAvailable {
		t.Error("update_available should be false")
	}
	if r.VersionLatest != "6.0.0-beta9" {
		t.Errorf("version_latest: got %q, want %q", r.VersionLatest, "6.0.0-beta9")
	}
	if stub.calls.Load() != 2 {
		t.Errorf("expected 2 calls (no tag list), got %d", stub.calls.Load())
	}
}

func TestCheck_DigestsDiffer_TagListResolvesLatestName(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:newer"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta8", 200, tagJSON("6.0.0-beta8", "sha256:older"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/", 200, tagListJSON([]map[string]string{
		{"name": "latest", "digest": "sha256:newer"},
		{"name": "6.0.0-beta9", "digest": "sha256:newer"},
		{"name": "6.0.0-beta8", "digest": "sha256:older"},
	}))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "6.0.0-beta8", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !r.UpdateAvailable {
		t.Error("update_available should be true")
	}
	if r.VersionLatest != "6.0.0-beta9" {
		t.Errorf("version_latest: got %q, want %q", r.VersionLatest, "6.0.0-beta9")
	}
	if stub.calls.Load() != 3 {
		t.Errorf("expected 3 calls (tag list on miss), got %d", stub.calls.Load())
	}
}

func TestCheck_RunningVersionNotOnHub(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/custom-build", 404, `{"message":"not found"}`)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	r, err := c.Check(context.Background(), "custom-build", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.UpdateAvailable {
		t.Error("update_available should be false on 404")
	}
	if r.VersionLatest != "" {
		t.Errorf("version_latest should be empty, got %q", r.VersionLatest)
	}
	if r.Note == "" {
		t.Error("note should be set on 404")
	}
}

func TestCheck_UpstreamUnreachable(t *testing.T) {
	c := New(Config{
		Repo:    "teamspeaksystems/teamspeak6-server",
		BaseURL: "http://127.0.0.1:1", // closed port
		TTL:     time.Minute,
	})
	_, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if !errors.Is(err, ErrUpstreamUnreachable) {
		t.Fatalf("expected ErrUpstreamUnreachable, got %v", err)
	}
}

func TestCheck_Upstream5xx(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 503, `{"message":"upstream"}`)
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	_, err := c.Check(context.Background(), "6.0.0-beta9", false)
	if !errors.Is(err, ErrUpstreamError) {
		t.Fatalf("expected ErrUpstreamError, got %v", err)
	}
}

func TestCheck_CacheHitSkipsHTTP(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if stub.calls.Load() != first {
		t.Errorf("cached call should not hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

func TestCheck_ForceRefreshBypassesCache(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, time.Minute)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	if _, err := c.Check(context.Background(), "6.0.0-beta9", true); err != nil {
		t.Fatalf("forced call: %v", err)
	}
	if stub.calls.Load() <= first {
		t.Errorf("forced refresh should re-hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

func TestCheck_TTLExpiry(t *testing.T) {
	stub := newStub()
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/latest", 200, tagJSON("latest", "sha256:x"))
	stub.set("/v2/repositories/teamspeaksystems/teamspeak6-server/tags/6.0.0-beta9", 200, tagJSON("6.0.0-beta9", "sha256:x"))
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c := newTestClient(t, srv.URL, 5*time.Millisecond)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("first call: %v", err)
	}
	first := stub.calls.Load()
	time.Sleep(15 * time.Millisecond)
	if _, err := c.Check(context.Background(), "6.0.0-beta9", false); err != nil {
		t.Fatalf("post-ttl call: %v", err)
	}
	if stub.calls.Load() <= first {
		t.Errorf("expired cache should re-hit HTTP: before=%d after=%d", first, stub.calls.Load())
	}
}

// guard against the test file accidentally not compiling on its own.
var _ = strings.TrimSpace
```

- [ ] **Step 2: Run the tests to confirm they fail to compile**

Run: `go test ./internal/dockerhub/ -v`
Expected: compile error (`undefined: New, Config, Client, ErrUpstreamUnreachable, ErrUpstreamError`).

- [ ] **Step 3: Implement the dockerhub Client**

`internal/dockerhub/client.go`:

```go
// Package dockerhub queries Docker Hub to determine whether a newer
// image tag is available than the one currently running. It compares
// digests rather than version strings, with a small in-memory cache.
package dockerhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const defaultBaseURL = "https://hub.docker.com"

type Config struct {
	Repo    string        // e.g. "teamspeaksystems/teamspeak6-server"
	BaseURL string        // overridable for tests; defaults to https://hub.docker.com
	TTL     time.Duration // cache freshness window; <= 0 disables caching
}

type Client struct {
	repo    string
	baseURL string
	ttl     time.Duration
	http    *http.Client

	mu     sync.Mutex
	cached *cachedResult
}

type cachedResult struct {
	runningVersion string
	result         Result
	storedAt       time.Time
}

type Result struct {
	VersionRunning  string    `json:"version_running"`
	VersionLatest   string    `json:"version_latest"`
	UpdateAvailable bool      `json:"update_available"`
	CheckedAt       time.Time `json:"checked_at"`
	Note            string    `json:"note,omitempty"`
}

var (
	ErrUpstreamUnreachable = errors.New("dockerhub: upstream unreachable")
	ErrUpstreamError       = errors.New("dockerhub: upstream error")
)

func New(cfg Config) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return &Client{
		repo:    cfg.Repo,
		baseURL: base,
		ttl:     cfg.TTL,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Check compares the digest of `latest` against the digest of the running
// version's tag. If they differ, it makes one extra call to fetch the
// tag list and resolve the human-readable name of `latest`.
func (c *Client) Check(ctx context.Context, runningVersion string, forceRefresh bool) (Result, error) {
	if !forceRefresh {
		if r, ok := c.cacheGet(runningVersion); ok {
			return r, nil
		}
	}

	digestLatest, _, err := c.fetchTag(ctx, "latest")
	if err != nil {
		return Result{}, err
	}

	digestRunning, status, err := c.fetchTag(ctx, runningVersion)
	if err != nil && status != http.StatusNotFound {
		return Result{}, err
	}

	out := Result{
		VersionRunning: runningVersion,
		CheckedAt:      time.Now().UTC(),
	}

	switch {
	case status == http.StatusNotFound:
		out.VersionLatest = ""
		out.UpdateAvailable = false
		out.Note = "running version not found on docker hub"
	case digestRunning == digestLatest:
		out.VersionLatest = runningVersion
		out.UpdateAvailable = false
	default:
		out.UpdateAvailable = true
		name, err := c.resolveLatestName(ctx, digestLatest)
		if err != nil {
			return Result{}, err
		}
		out.VersionLatest = name
	}

	c.cacheStore(runningVersion, out)
	return out, nil
}

func (c *Client) cacheGet(runningVersion string) (Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached == nil || c.ttl <= 0 {
		return Result{}, false
	}
	if c.cached.runningVersion != runningVersion {
		return Result{}, false
	}
	if time.Since(c.cached.storedAt) > c.ttl {
		return Result{}, false
	}
	return c.cached.result, true
}

func (c *Client) cacheStore(runningVersion string, r Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cached = &cachedResult{runningVersion: runningVersion, result: r, storedAt: time.Now()}
}

// fetchTag returns the digest, the HTTP status (so callers can distinguish
// 404 without inspecting error wrapping), and any error.
func (c *Client) fetchTag(ctx context.Context, tag string) (string, int, error) {
	u := fmt.Sprintf("%s/v2/repositories/%s/tags/%s", c.baseURL, c.repo, url.PathEscape(tag))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", resp.StatusCode, fmt.Errorf("tag %q: 404", tag)
	}
	if resp.StatusCode >= 500 {
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", resp.StatusCode, fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode, fmt.Errorf("%w: read body: %v", ErrUpstreamUnreachable, err)
	}
	var t struct {
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", resp.StatusCode, fmt.Errorf("%w: parse: %v", ErrUpstreamError, err)
	}
	return t.Digest, resp.StatusCode, nil
}

// resolveLatestName returns the name of the first tag (excluding "latest")
// whose digest matches digestLatest, looking only at the first page of
// 100 tags. Empty string when not found.
func (c *Client) resolveLatestName(ctx context.Context, digestLatest string) (string, error) {
	u := fmt.Sprintf("%s/v2/repositories/%s/tags/?page_size=100", c.baseURL, c.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstreamUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return "", fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("%w: status %d", ErrUpstreamError, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("%w: read body: %v", ErrUpstreamUnreachable, err)
	}
	var page struct {
		Results []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", fmt.Errorf("%w: parse: %v", ErrUpstreamError, err)
	}
	for _, r := range page.Results {
		if r.Name == "latest" {
			continue
		}
		if r.Digest == digestLatest {
			return r.Name, nil
		}
	}
	return "", nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/dockerhub/ -race -v`
Expected: all 7 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/dockerhub/
git commit -m "feat(dockerhub): digest-based update check with in-memory cache"
```

---

## Task 6: api package — middleware

**Files:**
- Create: `internal/api/middleware.go`

- [ ] **Step 1: Implement the middleware**

`internal/api/middleware.go`:

```go
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const ctxRequestID ctxKey = 1

// requestIDFromContext returns the request ID stored on ctx, or "" if absent.
func requestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// RecoverAndLog wraps next with panic recovery, request-ID injection, and
// one access log line per request. Exported so main.go can apply it to
// the top-level handler.
func RecoverAndLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			rid := newRequestID()
			ctx := context.WithValue(r.Context(), ctxRequestID, rid)

			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("http.panic",
						"request_id", rid,
						"path", r.URL.Path,
						"err", rec,
						"stack", string(debug.Stack()),
					)
					if !rw.wroteHeader {
						http.Error(rw, `{"error":"internal error"}`, http.StatusInternalServerError)
					}
				}
				logger.Info("http_request",
					"method", r.Method,
					"path", r.URL.Path,
					"status", rw.status,
					"duration_ms", time.Since(start).Milliseconds(),
					"remote_addr", r.RemoteAddr,
					"request_id", rid,
				)
			}()

			rw.Header().Set("X-Request-Id", rid)
			next.ServeHTTP(rw, r.WithContext(ctx))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.wroteHeader {
		return
	}
	s.status = code
	s.wroteHeader = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./internal/api/`
Expected: no output, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/api/middleware.go
git commit -m "feat(api): request-id, access-log, panic-recovery middleware"
```

---

## Task 7: api package — handlers (TDD)

**Files:**
- Create: `internal/api/api.go`
- Create: `internal/api/api_test.go`

- [ ] **Step 1: Write the failing tests**

`internal/api/api_test.go`:

```go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
)

type fakeTS struct {
	connected bool
	version   ts.VersionInfo
	versionErr error
	info      ts.ServerInfo
	infoErr   error
}

func (f *fakeTS) IsConnected() bool                     { return f.connected }
func (f *fakeTS) Version() (ts.VersionInfo, error)      { return f.version, f.versionErr }
func (f *fakeTS) ServerInfo() (ts.ServerInfo, error)    { return f.info, f.infoErr }

type fakeHub struct {
	result dockerhub.Result
	err    error
	lastForce bool
}

func (f *fakeHub) Check(_ context.Context, runningVersion string, force bool) (dockerhub.Result, error) {
	f.lastForce = force
	return f.result, f.err
}

func newRouter(t *testing.T, ts TSClient, hub DockerHubClient) http.Handler {
	mux := http.NewServeMux()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	Register(mux, ts, hub, logger)
	return RecoverAndLog(logger)(mux)
}

func doGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthz_AlwaysOK(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false}, &fakeHub{})
	rr := doGet(t, h, "/healthz")
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rr.Code)
	}
}

func TestReadyz_NotConnected(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false}, &fakeHub{})
	rr := doGet(t, h, "/readyz")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rr.Code)
	}
}

func TestReadyz_Connected(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: true}, &fakeHub{})
	rr := doGet(t, h, "/readyz")
	if rr.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rr.Code)
	}
}

func TestServerInfo_Success(t *testing.T) {
	info := ts.ServerInfo{
		Version: "6.0.0-beta9", Platform: "Linux", Build: 1700000000,
		SID: 1, ServerStatus: "online", ServerName: "Example TS",
		UptimeSeconds: 12345, ClientsOnline: 3, ClientsMax: 32, LoginName: "serveradmin",
	}
	h := newRouter(t, &fakeTS{connected: true, info: info}, &fakeHub{})
	rr := doGet(t, h, "/api/server-info")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["version"] != "6.0.0-beta9" {
		t.Errorf("version: got %v", got["version"])
	}
	if got["server_name"] != "Example TS" {
		t.Errorf("server_name: got %v", got["server_name"])
	}
}

func TestServerInfo_Unavailable(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: true, infoErr: ts.ErrUnavailable}, &fakeHub{})
	rr := doGet(t, h, "/api/server-info")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d, want 503", rr.Code)
	}
}

func TestUpdateCheck_Success(t *testing.T) {
	hub := &fakeHub{result: dockerhub.Result{
		VersionRunning:  "6.0.0-beta9",
		VersionLatest:   "6.0.0-beta10",
		UpdateAvailable: true,
		CheckedAt:       time.Now().UTC(),
	}}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d", rr.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["update_available"] != true {
		t.Errorf("update_available: got %v", got["update_available"])
	}
}

func TestUpdateCheck_TSDownReturns503(t *testing.T) {
	h := newRouter(t, &fakeTS{connected: false, versionErr: ts.ErrUnavailable}, &fakeHub{})
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d", rr.Code)
	}
}

func TestUpdateCheck_UpstreamUnreachableReturns503(t *testing.T) {
	hub := &fakeHub{err: dockerhub.ErrUpstreamUnreachable}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: got %d", rr.Code)
	}
}

func TestUpdateCheck_UpstreamErrorReturns502(t *testing.T) {
	hub := &fakeHub{err: dockerhub.ErrUpstreamError}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check")
	if rr.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want 502", rr.Code)
	}
}

func TestUpdateCheck_RefreshParam(t *testing.T) {
	hub := &fakeHub{result: dockerhub.Result{VersionRunning: "6.0.0-beta9", VersionLatest: "6.0.0-beta9"}}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)

	doGet(t, h, "/api/update-check?refresh=1")
	if !hub.lastForce {
		t.Error("expected forceRefresh=true when refresh=1")
	}
	doGet(t, h, "/api/update-check")
	if hub.lastForce {
		t.Error("expected forceRefresh=false when refresh missing")
	}
}

func TestUpdateCheck_BadRefreshParam(t *testing.T) {
	hub := &fakeHub{}
	h := newRouter(t, &fakeTS{connected: true, version: ts.VersionInfo{Version: "6.0.0-beta9"}}, hub)
	rr := doGet(t, h, "/api/update-check?refresh=foo")
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rr.Code)
	}
}

// silence unused-import warnings if test set shrinks during refactor.
var _ = errors.New
```

- [ ] **Step 2: Run the tests to confirm compile failure**

Run: `go test ./internal/api/ -v`
Expected: compile error (`undefined: TSClient, DockerHubClient, Register`).

- [ ] **Step 3: Implement the handlers**

`internal/api/api.go`:

```go
// Package api wires HTTP handlers for the manager's endpoints.
//
// Handlers depend on consumer-side interfaces (TSClient, DockerHubClient)
// satisfied by the concrete clients in internal/ts and internal/dockerhub.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
)

// TSClient is the subset of *ts.Client the API depends on.
type TSClient interface {
	IsConnected() bool
	Version() (ts.VersionInfo, error)
	ServerInfo() (ts.ServerInfo, error)
}

// DockerHubClient is the subset of *dockerhub.Client the API depends on.
type DockerHubClient interface {
	Check(ctx context.Context, runningVersion string, forceRefresh bool) (dockerhub.Result, error)
}

// Register attaches all routes to mux. Wrap the mux with recoverAndLog
// (or a chain that includes it) to get request-id and access logging.
func Register(mux *http.ServeMux, tsClient TSClient, hub DockerHubClient, logger *slog.Logger) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if tsClient.IsConnected() {
			writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "ts": "down"})
	})

	mux.HandleFunc("GET /api/server-info", func(w http.ResponseWriter, r *http.Request) {
		info, err := tsClient.ServerInfo()
		if err != nil {
			if errors.Is(err, ts.ErrUnavailable) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ts unavailable"})
				return
			}
			logger.Error("server_info", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version":         info.Version,
			"platform":        info.Platform,
			"build":           info.Build,
			"sid":             info.SID,
			"server_status":   info.ServerStatus,
			"server_name":     info.ServerName,
			"uptime_seconds":  info.UptimeSeconds,
			"clients_online":  info.ClientsOnline,
			"clients_max":     info.ClientsMax,
			"login_name":      info.LoginName,
		})
	})

	mux.HandleFunc("GET /api/update-check", func(w http.ResponseWriter, r *http.Request) {
		force, ok := parseRefreshParam(r.URL.Query().Get("refresh"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid refresh value"})
			return
		}

		v, err := tsClient.Version()
		if err != nil {
			if errors.Is(err, ts.ErrUnavailable) {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ts unavailable"})
				return
			}
			logger.Error("update_check.version", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}

		result, err := hub.Check(r.Context(), v.Version, force)
		if err != nil {
			switch {
			case errors.Is(err, dockerhub.ErrUpstreamUnreachable):
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "upstream unreachable"})
			case errors.Is(err, dockerhub.ErrUpstreamError):
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream error"})
			default:
				logger.Error("update_check.hub", "err", err.Error(), "request_id", requestIDFromContext(r.Context()))
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			}
			return
		}
		writeJSON(w, http.StatusOK, result)
	})
}

func parseRefreshParam(s string) (force bool, ok bool) {
	switch s {
	case "":
		return false, true
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	default:
		return false, false
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/api/ -race -v`
Expected: all 11 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/api_test.go
git commit -m "feat(api): handlers for healthz, readyz, server-info, update-check"
```

---

## Task 8: main.go — serve mode and signal handling

**Files:**
- Modify: `main.go` (full rewrite)

- [ ] **Step 1: Replace `main.go`**

`main.go`:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/floholz/ts-server-manager/internal/api"
	"github.com/floholz/ts-server-manager/internal/config"
	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
	"github.com/floholz/ts-server-manager/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(runHealthcheck())
	}
	if err := runServe(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func runServe() error {
	cfg, err := config.LoadFromOS()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	tsClient := ts.New(ts.Config{
		Host: cfg.TSHost, Port: cfg.TSPort,
		User: cfg.TSUser, Password: cfg.TSPassword,
		SID: cfg.TSSID,
	}, logger)
	tsClient.Start(ctx)

	hubClient := dockerhub.New(dockerhub.Config{
		Repo: cfg.DockerHubRepo,
		TTL:  cfg.UpdateCheckTTL,
	})

	mux := http.NewServeMux()
	api.Register(mux, tsClient, hubClient, logger)

	handler := api.RecoverAndLog(logger)(mux)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("http.listen", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutdown.signal_received")
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http.shutdown", "err", err.Error())
	}
	logger.Info("shutdown.done")
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info", "":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With(
		slog.String("service.name", "ts-server-manager"),
		slog.String("service.version", version.Version),
	)
}
```

- [ ] **Step 2: Add the `net` import for `BaseContext`**

The `net.Listener` type used in `BaseContext` requires `import "net"`. Update the import block in `main.go` to include `"net"`.

After update, the import block should read:

```go
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/floholz/ts-server-manager/internal/api"
	"github.com/floholz/ts-server-manager/internal/config"
	"github.com/floholz/ts-server-manager/internal/dockerhub"
	"github.com/floholz/ts-server-manager/internal/ts"
	"github.com/floholz/ts-server-manager/internal/version"
)
```

- [ ] **Step 3: Verify it builds**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat(main): wire serve mode with graceful shutdown"
```

---

## Task 9: main.go — `healthcheck` subcommand

**Files:**
- Modify: `main.go` (add `runHealthcheck`)

- [ ] **Step 1: Add the `runHealthcheck` function**

Append to `main.go`:

```go
// runHealthcheck issues a single GET /readyz against the local server and
// returns 0 on 200, 1 otherwise. Used by the Dockerfile HEALTHCHECK.
func runHealthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":9988"
	}
	// HTTP_ADDR may be ":9988" or "0.0.0.0:9988"; normalise to a localhost URL.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid HTTP_ADDR:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s/readyz", net.JoinHostPort(host, port))

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
```

- [ ] **Step 2: Verify it builds**

Run: `go build ./...`
Expected: no output, exit 0.

- [ ] **Step 3: Smoke-test the subcommand**

Run: `./ts-server-manager healthcheck`
Expected: exits 1 with a connection-refused error (since no server is running). Confirms the path is correct.

- [ ] **Step 4: Commit**

```bash
git add main.go
git commit -m "feat(main): add healthcheck subcommand for docker HEALTHCHECK"
```

---

## Task 10: Dockerfile, compose.yaml, .env.example

**Files:**
- Modify: `Dockerfile`
- Modify: `compose.yaml`
- Modify: `.env.example`

- [ ] **Step 1: Replace `Dockerfile`**

```dockerfile
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/floholz/ts-server-manager/internal/version.Version=$(git -C /src describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/ts-server-manager .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ts-server-manager /ts-server-manager
USER nonroot:nonroot
EXPOSE 9988
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/ts-server-manager", "healthcheck"]
ENTRYPOINT ["/ts-server-manager"]
```

Note: `git describe` inside the build stage requires the `.git` directory to be in the build context. If your Dockerfile is built without `.git` (e.g. CI uses a shallow checkout), the `|| echo dev` fallback ensures it still builds.

- [ ] **Step 2: Replace `compose.yaml`**

```yaml
services:
  ts-server-manager:
    build: .
    image: ts-server-manager:latest
    container_name: ts-server-manager
    env_file: .env
    restart: unless-stopped
    ports:
      - "9988:9988"
```

- [ ] **Step 3: Replace `.env.example`**

```env
# TeamSpeak 3 ServerQuery (SSH) connection
TS_HOST=your.teamspeak.host
TS_PORT=10022
TS_USER=serveradmin
TS_PASSWORD=changeme
TS_SID=1

# HTTP server
HTTP_ADDR=:9988
LOG_LEVEL=info

# Update check
DOCKERHUB_REPO=teamspeaksystems/teamspeak6-server
UPDATE_CHECK_TTL=15m
```

- [ ] **Step 4: Verify the image still builds**

Run: `docker build -t ts-server-manager:dev .`
Expected: image built, no errors. (Skip if Docker isn't available locally — CI will catch it.)

- [ ] **Step 5: Commit**

```bash
git add Dockerfile compose.yaml .env.example
git commit -m "build: expose 9988, add HEALTHCHECK, document env vars"
```

---

## Task 11: Final verification

**Files:** none

- [ ] **Step 1: Tidy module dependencies**

Run: `go mod tidy`
Expected: may add `go.sum` and prune unused entries.

- [ ] **Step 2: Run the full test suite with race detector**

Run: `go test -race ./...`
Expected: all tests PASS, no race warnings.

- [ ] **Step 3: Vet**

Run: `go vet ./...`
Expected: no output.

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: no output.

- [ ] **Step 5: Manual smoke test (if a TS server is available)**

Set TS env vars and run:

```bash
export TS_HOST=...
export TS_USER=serveradmin
export TS_PASSWORD=...
go run .
```

In another shell:

```bash
curl -sS http://127.0.0.1:9988/healthz
curl -sS http://127.0.0.1:9988/readyz
curl -sS http://127.0.0.1:9988/api/server-info | jq .
curl -sS http://127.0.0.1:9988/api/update-check | jq .
curl -sS 'http://127.0.0.1:9988/api/update-check?refresh=1' | jq .
```

Expected: all four endpoints respond as documented in the spec. Stop with Ctrl-C and confirm `shutdown.done` log line appears.

- [ ] **Step 6: Commit anything outstanding**

```bash
git status
# if go.sum or go.mod changed:
git add go.mod go.sum
git commit -m "chore: tidy module dependencies"
```

---

## Self-review notes

**Spec coverage check:**
- `/healthz`, `/readyz`, `/api/server-info`, `/api/update-check` — Tasks 7, 8.
- Long-lived TS connection with reconnect/backoff/keepalive — Task 4.
- 15-min cache + force-refresh — Task 5.
- Tag-list lookup only on digest mismatch — Task 5.
- 404 on running version → soft success with `note` — Task 5.
- Structured JSON logs (slog) with OTel-shaped fields — Task 6 (middleware) + Task 8 (logger setup).
- `request_id` per request — Task 6.
- Distroless `HEALTHCHECK` via subcommand — Tasks 9, 10.
- Configurable env vars — Task 2 + Task 10.
- Tests for `ts`, `dockerhub`, `api` — Tasks 4, 5, 7.

**No spec gaps detected.**
