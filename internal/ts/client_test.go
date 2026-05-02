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
