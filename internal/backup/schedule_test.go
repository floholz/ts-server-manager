package backup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/floholz/ts-server-manager/internal/ts"
)

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time { return c.t }

func newTestScheduler(t *testing.T, dir string, q ts.Execer, keep int) (*Scheduler, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	s, err := NewScheduler(q, ScheduleConfig{
		Dir: dir, Interval: 24 * time.Hour, Keep: keep, ToolVersion: "test",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, clk
}

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

type failingExec struct{}

func (failingExec) Exec(*ts.Command) ([]ts.Row, error) { return nil, errors.New("ts: unavailable") }

func TestNewScheduler_RejectsMissingDir(t *testing.T) {
	_, err := NewScheduler(newFixture(), ScheduleConfig{Dir: filepath.Join(t.TempDir(), "nope"), Interval: time.Hour})
	if err == nil {
		t.Fatal("expected error for missing dir")
	}
}

func TestNewScheduler_PicksUpNewestBackupAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "ts-backup-20261105T030000Z.json")
	touch(t, dir, "ts-backup-20261106T030000Z.json")
	touch(t, dir, ".ts-backup-20261107T030000Z.json.tmp-123")
	touch(t, dir, "notes.txt")

	s, _ := newTestScheduler(t, dir, newFixture(), 14)
	st := s.Status()
	if st.LastFile != "ts-backup-20261106T030000Z.json" || st.LastSuccess == nil ||
		!st.LastSuccess.Equal(time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("status: %+v", st)
	}
	if strings.Contains(strings.Join(listDir(t, dir), ","), ".tmp-") {
		t.Errorf("stale temp file not removed: %v", listDir(t, dir))
	}
	// Last backup 33h ago with a 24h interval: due now, and not yet stale.
	if next := s.nextRun(); !next.Equal(time.Date(2026, 11, 7, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("next run: %s", next)
	}
	if !st.Healthy || st.StatusMsg != "ok" {
		t.Errorf("health: %v %q", st.Healthy, st.StatusMsg)
	}
}

func TestRunOnce_WritesValidBackupAndPrunes(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{
		"ts-backup-20261101T120000Z.json", "ts-backup-20261102T120000Z.json",
		"ts-backup-20261103T120000Z.json", "my-own-backup.json",
	} {
		touch(t, dir, n)
	}
	s, _ := newTestScheduler(t, dir, newFixture(), 2)
	if err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}

	want := "ts-backup-20261107T120000Z.json"
	got := strings.Join(listDir(t, dir), ",")
	if got != "my-own-backup.json,ts-backup-20261103T120000Z.json,"+want {
		t.Errorf("files after prune: %s", got)
	}
	fi, err := os.Stat(filepath.Join(dir, want))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	fh, _ := os.Open(filepath.Join(dir, want))
	defer fh.Close()
	if f, err := Read(fh); err != nil || f.Source.ServerName != "Example TS" {
		t.Errorf("written backup unreadable: %v", err)
	}

	st := s.Status()
	if st.LastFile != want || st.LastError != "" || !st.Healthy || st.StatusMsg != "ok" {
		t.Errorf("status: %+v", st)
	}
}

func TestRunOnce_KeepZeroKeepsAll(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, "ts-backup-20200101T000000Z.json")
	s, _ := newTestScheduler(t, dir, newFixture(), 0)
	if err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	if n := len(listDir(t, dir)); n != 2 {
		t.Errorf("expected 2 files, got %v", listDir(t, dir))
	}
}

func TestRunOnce_FailureIsReportedAndRetried(t *testing.T) {
	dir := t.TempDir()
	s, clk := newTestScheduler(t, dir, failingExec{}, 14)
	if err := s.RunOnce(); err == nil {
		t.Fatal("expected error")
	}
	st := s.Status()
	if st.Healthy || !strings.HasPrefix(st.StatusMsg, "failed: ") || !strings.Contains(st.LastError, "unavailable") {
		t.Errorf("status: %+v", st)
	}
	if next := s.nextRun(); !next.Equal(clk.t.Add(10 * time.Minute)) {
		t.Errorf("retry at %s", next)
	}
	if len(listDir(t, dir)) != 0 {
		t.Errorf("failed backup left files: %v", listDir(t, dir))
	}
}

func TestStatus_PendingThenNoBackupThenStale(t *testing.T) {
	dir := t.TempDir()
	s, clk := newTestScheduler(t, dir, newFixture(), 14)
	if st := s.Status(); !st.Healthy || !strings.HasPrefix(st.StatusMsg, "pending") {
		t.Errorf("fresh: %+v", st)
	}
	clk.t = clk.t.Add(25 * time.Hour)
	if st := s.Status(); st.Healthy || !strings.Contains(st.StatusMsg, "no backup since start") {
		t.Errorf("no backup for a full interval: %+v", st)
	}
	if err := s.RunOnce(); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(49 * time.Hour)
	if st := s.Status(); st.Healthy || !strings.HasPrefix(st.StatusMsg, "stale:") {
		t.Errorf("after 49h: %+v", st)
	}
}

func TestRun_WaitsForReadyThenBacksUp(t *testing.T) {
	dir := t.TempDir()
	var ready atomic.Bool
	s, err := NewScheduler(newFixture(), ScheduleConfig{
		Dir: dir, Interval: time.Hour, Keep: 3,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ready:        ready.Load,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	time.Sleep(30 * time.Millisecond)
	if n := len(listDir(t, dir)); n != 0 {
		t.Fatalf("backup taken while not ready: %v", listDir(t, dir))
	}
	ready.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for len(listDir(t, dir)) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(listDir(t, dir)) != 1 {
		t.Fatalf("expected one backup, got %v", listDir(t, dir))
	}
	if st := s.Status(); st.NextRun == nil || st.NextRun.Before(time.Now().Add(50*time.Minute)) {
		t.Errorf("next run should be ~1h out: %+v", st.NextRun)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop on cancel")
	}
}
