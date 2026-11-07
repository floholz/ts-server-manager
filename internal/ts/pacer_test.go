package ts

import (
	"errors"
	"testing"
	"time"
)

// fakeClock records sleeps and advances virtual time.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
}

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) sleep(d time.Duration) {
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
}

// scriptedExec returns queued errors (nil = success) and records call times.
type scriptedExec struct {
	clock *fakeClock
	errs  []error
	calls []time.Time
}

func (s *scriptedExec) Exec(*Command) ([]Row, error) {
	s.calls = append(s.calls, s.clock.t)
	if len(s.errs) == 0 {
		return []Row{{"ok": "1"}}, nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return nil, err
}

func newTestPacer(rate float64, errs ...error) (*Pacer, *scriptedExec, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	ex := &scriptedExec{clock: clk, errs: errs}
	p := NewPacer(ex, rate)
	p.now, p.sleep = clk.now, clk.sleep
	return p, ex, clk
}

func flood(extra string) error {
	return &QueryError{ID: ErrIDFlooding, Msg: "client is flooding", ExtraMsg: extra}
}

func TestPacer_SpacesCommands(t *testing.T) {
	p, ex, _ := newTestPacer(2) // one command per 500ms
	for i := 0; i < 4; i++ {
		if _, err := p.Exec(NewCommand("version")); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i < len(ex.calls); i++ {
		if gap := ex.calls[i].Sub(ex.calls[i-1]); gap < 500*time.Millisecond {
			t.Errorf("gap %d = %s, want >= 500ms", i, gap)
		}
	}
}

func TestPacer_NoSleepWhenCommandsAreAlreadySlow(t *testing.T) {
	p, _, clk := newTestPacer(2)
	_, _ = p.Exec(NewCommand("version"))
	clk.t = clk.t.Add(2 * time.Second) // caller was busy
	_, _ = p.Exec(NewCommand("version"))
	if len(clk.sleeps) != 0 {
		t.Errorf("unexpected sleeps: %v", clk.sleeps)
	}
}

func TestPacer_RateZeroDisablesPacing(t *testing.T) {
	p, _, clk := newTestPacer(0)
	for i := 0; i < 5; i++ {
		_, _ = p.Exec(NewCommand("version"))
	}
	if len(clk.sleeps) != 0 {
		t.Errorf("unexpected sleeps: %v", clk.sleeps)
	}
}

func TestPacer_ShortFloodIsWaitedOutAndRetried(t *testing.T) {
	p, ex, clk := newTestPacer(0, flood("please wait 1 seconds"))
	rows, err := p.Exec(NewCommand("version"))
	if err != nil {
		t.Fatalf("expected retry to succeed, got %v", err)
	}
	if len(rows) != 1 || len(ex.calls) != 2 {
		t.Fatalf("rows=%v calls=%d", rows, len(ex.calls))
	}
	if gap := ex.calls[1].Sub(ex.calls[0]); gap < 2*time.Second {
		t.Errorf("retry after %s, want >= wait+1s = 2s", gap)
	}
	_ = clk
}

func TestPacer_FloodWithoutWaitHintDefaultsToOneSecond(t *testing.T) {
	p, ex, _ := newTestPacer(0, flood(""))
	if _, err := p.Exec(NewCommand("version")); err != nil {
		t.Fatal(err)
	}
	if gap := ex.calls[1].Sub(ex.calls[0]); gap < 2*time.Second {
		t.Errorf("retry after %s, want >= 2s", gap)
	}
}

func TestPacer_SecondFloodAborts(t *testing.T) {
	p, ex, _ := newTestPacer(0, flood("please wait 1 seconds"), flood("please wait 1 seconds"))
	_, err := p.Exec(NewCommand("version"))
	if !errors.Is(err, ErrFlooding) {
		t.Fatalf("expected ErrFlooding, got %v", err)
	}
	if len(ex.calls) != 2 {
		t.Errorf("expected exactly 2 attempts, got %d", len(ex.calls))
	}
}

func TestPacer_LongWaitAbortsWithoutWaiting(t *testing.T) {
	p, ex, clk := newTestPacer(0, flood("please wait 600 seconds"))
	_, err := p.Exec(NewCommand("version"))
	if !errors.Is(err, ErrFlooding) {
		t.Fatalf("expected ErrFlooding, got %v", err)
	}
	if len(ex.calls) != 1 || len(clk.sleeps) != 0 {
		t.Errorf("must not retry or wait out a ban: calls=%d sleeps=%v", len(ex.calls), clk.sleeps)
	}
}

func TestPacer_OtherErrorsPassThrough(t *testing.T) {
	qe := &QueryError{ID: ErrIDInvalidChannel, Msg: "invalid channelID"}
	p, ex, _ := newTestPacer(0, qe)
	_, err := p.Exec(NewCommand("channelinfo"))
	if !errors.Is(err, qe) || len(ex.calls) != 1 {
		t.Fatalf("err=%v calls=%d", err, len(ex.calls))
	}
}
