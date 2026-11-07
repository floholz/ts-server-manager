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

// overlapTransport records whether two commands were ever in flight at the
// same time. The real ts3.Client shares one response channel across callers,
// so any overlap would let callers receive each other's replies.
type overlapTransport struct {
	fakeTransport
	inFlight atomic.Int32
	overlap  atomic.Bool
	calls    atomic.Int32
}

func (o *overlapTransport) enter() {
	if o.inFlight.Add(1) > 1 {
		o.overlap.Store(true)
	}
	o.calls.Add(1)
	time.Sleep(time.Millisecond) // widen the race window
}

func (o *overlapTransport) leave() { o.inFlight.Add(-1) }

func (o *overlapTransport) Version() (VersionInfo, error) {
	o.enter()
	defer o.leave()
	return o.fakeTransport.Version()
}

func (o *overlapTransport) Whoami() (whoami, error) {
	o.enter()
	defer o.leave()
	return o.fakeTransport.Whoami()
}

func (o *overlapTransport) ServerInfo() (rawServerInfo, error) {
	o.enter()
	defer o.leave()
	return o.fakeTransport.ServerInfo()
}

func TestClient_ConcurrentCallsAreSerialized(t *testing.T) {
	tr := &overlapTransport{}
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

	if tr.overlap.Load() {
		t.Fatal("two ServerQuery commands were in flight concurrently")
	}
	if tr.calls.Load() < 16*5 {
		t.Fatalf("expected at least %d transport calls, got %d", 16*5, tr.calls.Load())
	}
}

func TestClient_IsConnectedDoesNotBlockOnSlowCall(t *testing.T) {
	tr := &slowTransport{release: make(chan struct{})}
	d := &fakeDialer{queue: []dialResult{{t: tr}}}
	c := newTestClient(d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	if !waitFor(100*time.Millisecond, c.IsConnected) {
		t.Fatal("client never reported connected")
	}

	go func() { _, _ = c.Version() }()
	defer close(tr.release)

	done := make(chan bool, 1)
	go func() { done <- c.IsConnected() }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("IsConnected blocked behind an in-flight command")
	}
}

// slowTransport blocks Version until release is closed.
type slowTransport struct {
	fakeTransport
	release chan struct{}
}

func (s *slowTransport) Version() (VersionInfo, error) {
	<-s.release
	return VersionInfo{}, nil
}
