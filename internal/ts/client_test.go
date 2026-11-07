package ts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTransport replays canned raw responses keyed by command name. It
// records every line sent and can inject errors per command.
type fakeTransport struct {
	mu        sync.Mutex
	responses map[string][]string
	errs      map[string]error
	sent      []string
	closed    bool

	// onExec, if set, runs (outside mu) before each command is answered.
	onExec func(line string)
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		responses: map[string][]string{
			"version": {`version=6.0.0-beta13.1 build=1758000000 platform=Linux`},
			"whoami": {`virtualserver_status=online virtualserver_id=1 virtualserver_unique_identifier=abc= ` +
				`client_channel_id=1 client_nickname=serveradmin client_database_id=1 client_login_name=serveradmin`},
			"serverinfo": {`virtualserver_name=Example\sTS virtualserver_uptime=123456 ` +
				`virtualserver_clientsonline=3 virtualserver_maxclients=32 virtualserver_status=online`},
		},
		errs: map[string]error{},
	}
}

func (f *fakeTransport) setErr(cmd string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[cmd] = err
}

func (f *fakeTransport) setResponse(cmd string, lines ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responses[cmd] = lines
}

func (f *fakeTransport) sentLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeTransport) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeTransport) Exec(line string) ([]string, error) {
	if f.onExec != nil {
		f.onExec(line)
	}
	name, _, _ := strings.Cut(line, " ")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, line)
	if err := f.errs[name]; err != nil {
		return nil, err
	}
	return f.responses[name], nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// fakeDialer returns a queue of (transport, error) results, in order.
type fakeDialer struct {
	mu    sync.Mutex
	queue []dialResult
	calls atomic.Int32
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

// startConnected starts a client on tr and waits until it is connected.
func startConnected(t *testing.T, tr transport) (*Client, *fakeDialer) {
	t.Helper()
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newTestClient(d)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Start(ctx)
	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reported connected")
	}
	return c, d
}

func TestClient_ConnectsAndIsReady(t *testing.T) {
	startConnected(t, newFakeTransport())
}

func TestClient_ReconnectsAfterDialError(t *testing.T) {
	d := &fakeDialer{queue: []dialResult{
		{err: errors.New("boom")},
		{t: newFakeTransport()},
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
	first := newFakeTransport()
	first.setErr("version", errors.New("io: broken pipe"))
	second := newFakeTransport() // healthy
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
	if !first.isClosed() {
		t.Error("first transport should have been closed after failure")
	}
}

func TestClient_TransportErrorMarksDisconnected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"io error", errors.New("io: connection reset")},
		// A timed-out reply may still arrive later and be read by the next
		// caller, so timeouts must drop the connection too.
		{"timeout", errors.New("timeout")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newFakeTransport()
			c, _ := startConnected(t, tr)

			tr.setErr("version", tc.err)
			_, err := c.Version()
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("expected ErrUnavailable, got %v", err)
			}
			if !waitFor(100*time.Millisecond, func() bool { return !c.IsConnected() }) {
				t.Fatal("client should report disconnected after transport error")
			}
		})
	}
}

func TestClient_QueryErrorKeepsConnection(t *testing.T) {
	tr := newFakeTransport()
	c, d := startConnected(t, tr)

	tr.setErr("channelinfo", &QueryError{ID: ErrIDInvalidChannel, Msg: "invalid channelID"})
	_, err := c.Exec(NewCommand("channelinfo").Arg("cid", 99))

	var qe *QueryError
	if !errors.As(err, &qe) {
		t.Fatalf("expected *QueryError, got %T: %v", err, err)
	}
	if qe.ID != ErrIDInvalidChannel || qe.Command != "channelinfo" {
		t.Errorf("unexpected QueryError: %+v", qe)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Error("query error must not be reported as ErrUnavailable")
	}

	// Give a would-be reconnect a chance to happen, then assert it didn't.
	time.Sleep(30 * time.Millisecond)
	if !c.IsConnected() {
		t.Error("query error must not drop the connection")
	}
	if tr.isClosed() {
		t.Error("query error must not close the transport")
	}
	if n := d.calls.Load(); n != 1 {
		t.Errorf("expected exactly 1 dial, got %d", n)
	}
}

func TestClient_EmptyResultSetIsZeroRows(t *testing.T) {
	tr := newFakeTransport()
	c, _ := startConnected(t, tr)

	tr.setErr("channelgrouplist", &QueryError{ID: ErrIDEmptyResult, Msg: "database empty result set"})
	rows, err := c.Exec(NewCommand("channelgrouplist"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected zero rows, got %v", rows)
	}
}

func TestClient_ExecSendsEncodedCommandAndParsesRows(t *testing.T) {
	tr := newFakeTransport()
	c, _ := startConnected(t, tr)

	tr.setResponse("channelfind", `cid=1 channel_name=Lobby|cid=4 channel_name=AFK\sRoom`)
	rows, err := c.Exec(NewCommand("channelfind").Arg("pattern", "a b"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1]["channel_name"] != "AFK Room" || rows[1].IntOr("cid", 0) != 4 {
		t.Fatalf("unexpected rows: %v", rows)
	}

	var found bool
	for _, l := range tr.sentLines() {
		if l == `channelfind pattern=a\sb` {
			found = true
		}
	}
	if !found {
		t.Errorf("encoded command not sent; sent: %q", tr.sentLines())
	}
}

func TestClient_ServerInfoParsesResponses(t *testing.T) {
	c, _ := startConnected(t, newFakeTransport())

	si, err := c.ServerInfo()
	if err != nil {
		t.Fatal(err)
	}
	want := ServerInfo{
		Version: "6.0.0-beta13.1", Platform: "Linux", Build: 1758000000,
		SID: 1, ServerStatus: "online", ServerName: "Example TS",
		UptimeSeconds: 123456, ClientsOnline: 3, ClientsMax: 32,
		LoginName: "serveradmin",
	}
	if si != want {
		t.Errorf("got  %+v\nwant %+v", si, want)
	}
}

func TestClient_ContextCancelExits(t *testing.T) {
	tr := newFakeTransport()
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
	if !tr.isClosed() {
		t.Error("transport not closed on shutdown")
	}
}

// The real ts3.Client shares one response channel across callers, so any
// overlap would let callers receive each other's replies.
func TestClient_ConcurrentCallsAreSerialized(t *testing.T) {
	var inFlight atomic.Int32
	var overlap atomic.Bool
	var calls atomic.Int32

	tr := newFakeTransport()
	tr.onExec = func(string) {
		if inFlight.Add(1) > 1 {
			overlap.Store(true)
		}
		calls.Add(1)
		time.Sleep(time.Millisecond) // widen the race window
		inFlight.Add(-1)
	}

	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newWithDialer(Config{
		Host: "x", Port: "1", User: "u", Password: "p", SID: 1,
		KeepAliveInterval: 2 * time.Millisecond, // keepalive competes with handlers
		BackoffMin:        5 * time.Millisecond,
		BackoffMax:        20 * time.Millisecond,
	}, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reported connected")
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if i%2 == 0 {
					_, _ = c.Version()
				} else {
					_, _ = c.ServerInfo()
				}
			}
		}(i)
	}
	wg.Wait()

	if overlap.Load() {
		t.Fatal("two ServerQuery commands were in flight concurrently")
	}
	if calls.Load() < 16*5 {
		t.Fatalf("expected at least %d transport calls, got %d", 16*5, calls.Load())
	}
}

func TestClient_IsConnectedDoesNotBlockOnSlowCall(t *testing.T) {
	release := make(chan struct{})
	tr := newFakeTransport()
	var once sync.Once
	entered := make(chan struct{})
	tr.onExec = func(line string) {
		if strings.HasPrefix(line, "serverinfo") {
			once.Do(func() { close(entered) })
			<-release
		}
	}
	c, _ := startConnected(t, tr)
	defer close(release)

	go func() { _, _ = c.ServerInfo() }()
	<-entered // the slow command now holds the call lock

	done := make(chan bool, 1)
	go func() { done <- c.IsConnected() }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("IsConnected blocked behind an in-flight command")
	}
}

func TestConn_ExecAndQueryErrors(t *testing.T) {
	tr := newFakeTransport()
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	conn, err := connectWith(context.Background(), Config{}, d)
	if err != nil {
		t.Fatal(err)
	}

	v, err := conn.Version()
	if err != nil || v.Version != "6.0.0-beta13.1" {
		t.Fatalf("Version: %+v, %v", v, err)
	}

	tr.setErr("servergroupdel", &QueryError{ID: ErrIDPermissionDenied, Msg: "insufficient client permissions"})
	_, err = conn.Exec(NewCommand("servergroupdel").Arg("sgid", 6).Arg("force", true))
	if !IsQueryError(err, ErrIDPermissionDenied) {
		t.Fatalf("expected permission-denied QueryError, got %v", err)
	}

	if err := conn.Close(); err != nil || !tr.isClosed() {
		t.Fatalf("Close: %v, closed=%v", err, tr.isClosed())
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
