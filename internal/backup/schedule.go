package backup

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/floholz/ts-server-manager/internal/ts"
)

// Scheduled backup files are named ts-backup-<UTC timestamp>.json, which
// sorts chronologically. Retention only ever deletes files matching this
// pattern, so other files in the directory are safe.
const fileTimeLayout = "20060102T150405Z"

var scheduledFileRe = regexp.MustCompile(`^ts-backup-(\d{8}T\d{6}Z)\.json$`)

func scheduledFileName(t time.Time) string {
	return "ts-backup-" + t.UTC().Format(fileTimeLayout) + ".json"
}

// ScheduleConfig configures a Scheduler.
type ScheduleConfig struct {
	Dir         string
	Interval    time.Duration
	Keep        int // newest backups to keep; 0 keeps all
	ToolVersion string
	Logger      *slog.Logger

	// Ready reports whether the TeamSpeak connection is up. Backups wait
	// for it instead of failing while the server restarts.
	Ready func() bool

	// For tests.
	Now          func() time.Time
	RetryAfter   time.Duration // default 10m, capped at Interval
	PollInterval time.Duration // readiness poll, default 5s
}

// Status is the scheduler state reported by /api/backup-status.
type Status struct {
	Enabled      bool       `json:"enabled"`
	Dir          string     `json:"dir,omitempty"`
	Interval     string     `json:"interval,omitempty"`
	Keep         int        `json:"keep"`
	LastAttempt  *time.Time `json:"last_attempt,omitempty"`
	LastSuccess  *time.Time `json:"last_success,omitempty"`
	LastFile     string     `json:"last_file,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	LastDuration float64    `json:"last_duration_seconds,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"`
	// Healthy is false when the last attempt failed or the newest backup
	// is older than twice the interval.
	Healthy bool `json:"healthy"`
	// StatusMsg is a one-line summary for monitors that can only show one
	// value (Uptime Kuma JSON query): "ok", "pending: ...", "failed: ...",
	// "stale: ...".
	StatusMsg string `json:"status_msg"`
}

// Scheduler takes a backup every Interval and prunes old ones.
type Scheduler struct {
	q   ts.Execer
	cfg ScheduleConfig

	mu      sync.Mutex
	started time.Time
	st      Status
}

// NewScheduler checks that Dir is a writable directory, removes temp files
// left by an interrupted write, and picks up the newest existing backup so
// a restart does not trigger an extra backup.
func NewScheduler(q ts.Execer, cfg ScheduleConfig) (*Scheduler, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RetryAfter <= 0 {
		cfg.RetryAfter = 10 * time.Minute
	}
	if cfg.RetryAfter > cfg.Interval {
		cfg.RetryAfter = cfg.Interval
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Ready == nil {
		cfg.Ready = func() bool { return true }
	}
	if err := checkWritableDir(cfg.Dir); err != nil {
		return nil, err
	}
	removeStaleTemp(cfg.Dir, cfg.Logger)

	s := &Scheduler{q: q, cfg: cfg, started: cfg.Now()}
	s.st = Status{Enabled: true, Dir: cfg.Dir, Interval: cfg.Interval.String(), Keep: cfg.Keep}
	if name, t, ok := newestBackup(cfg.Dir); ok {
		s.st.LastSuccess = &t
		s.st.LastFile = name
	}
	return s, nil
}

// Run takes backups until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next := s.nextRun()
		s.setNext(next)
		if !sleepUntil(ctx, s.cfg.Now, next) {
			return
		}
		for !s.cfg.Ready() {
			if !sleepFor(ctx, s.cfg.PollInterval) {
				return
			}
		}
		_ = s.RunOnce()
	}
}

// RunOnce takes one backup now, writes it and applies retention.
func (s *Scheduler) RunOnce() error {
	start := s.cfg.Now()
	s.mu.Lock()
	s.st.LastAttempt = &start
	s.mu.Unlock()

	log := s.cfg.Logger
	log.Info("backup.started", "dir", s.cfg.Dir)
	name, err := s.take(start)
	took := s.cfg.Now().Sub(start)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastDuration = took.Seconds()
	if err != nil {
		s.st.LastError = err.Error()
		log.Error("backup.failed", "err", err.Error(), "duration_ms", took.Milliseconds())
		return err
	}
	s.st.LastError = ""
	s.st.LastSuccess = &start
	s.st.LastFile = name
	log.Info("backup.done", "file", name, "duration_ms", took.Milliseconds())

	if pruned, err := prune(s.cfg.Dir, s.cfg.Keep); err != nil {
		log.Warn("backup.prune_failed", "err", err.Error())
	} else if len(pruned) > 0 {
		log.Info("backup.pruned", "files", strings.Join(pruned, ","))
	}
	return nil
}

func (s *Scheduler) take(at time.Time) (string, error) {
	f, err := Export(s.q, ExportOptions{
		ToolVersion: s.cfg.ToolVersion,
		Now:         func() time.Time { return at },
	})
	if err != nil {
		return "", err
	}
	name := scheduledFileName(at)
	if err := WriteFile(filepath.Join(s.cfg.Dir, name), f); err != nil {
		return "", err
	}
	return name, nil
}

// nextRun: right away when there is no backup yet, Interval after the last
// success, and RetryAfter after a failure.
func (s *Scheduler) nextRun() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	if s.st.LastError != "" && s.st.LastAttempt != nil {
		return s.st.LastAttempt.Add(s.cfg.RetryAfter)
	}
	if s.st.LastSuccess == nil {
		return now
	}
	return s.st.LastSuccess.Add(s.cfg.Interval)
}

func (s *Scheduler) setNext(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.NextRun = &t
}

// Status returns the current state with Healthy and StatusMsg filled in.
func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.st
	now := s.cfg.Now()
	staleAfter := 2 * s.cfg.Interval
	switch {
	case st.LastError != "":
		st.StatusMsg = "failed: " + st.LastError
	case st.LastSuccess == nil:
		// Give the first backup a full interval (TS may be down at start).
		if now.Sub(s.started) > s.cfg.Interval {
			st.StatusMsg = fmt.Sprintf("failed: no backup since start %s ago", now.Sub(s.started).Round(time.Minute))
		} else {
			st.Healthy = true
			st.StatusMsg = "pending: first backup not taken yet"
		}
	case now.Sub(*st.LastSuccess) > staleAfter:
		st.StatusMsg = fmt.Sprintf("stale: last backup %s ago", now.Sub(*st.LastSuccess).Round(time.Minute))
	default:
		st.Healthy = true
		st.StatusMsg = "ok"
	}
	return st
}

// --- files ------------------------------------------------------------------

func checkWritableDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("backup dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("backup dir %s is not a directory", dir)
	}
	f, err := os.CreateTemp(dir, ".ts-backup-writetest-*")
	if err != nil {
		return fmt.Errorf("backup dir %s is not writable (the container runs as uid 65532): %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

// removeStaleTemp deletes temp files an interrupted WriteFile left behind.
func removeStaleTemp(dir string, log *slog.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".ts-backup-") && strings.Contains(n, ".tmp-") {
			if err := os.Remove(filepath.Join(dir, n)); err == nil {
				log.Info("backup.removed_stale_temp", "file", n)
			}
		}
	}
}

// scheduledBackups returns scheduled backup files, oldest first.
func scheduledBackups(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && scheduledFileRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func newestBackup(dir string) (string, time.Time, bool) {
	names, err := scheduledBackups(dir)
	if err != nil || len(names) == 0 {
		return "", time.Time{}, false
	}
	name := names[len(names)-1]
	t, err := time.Parse(fileTimeLayout, scheduledFileRe.FindStringSubmatch(name)[1])
	if err != nil {
		return "", time.Time{}, false
	}
	return name, t, true
}

// prune deletes the oldest scheduled backups beyond keep. keep <= 0 keeps all.
func prune(dir string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	names, err := scheduledBackups(dir)
	if err != nil {
		return nil, err
	}
	if len(names) <= keep {
		return nil, nil
	}
	victims := names[:len(names)-keep]
	var removed []string
	for _, n := range victims {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			return removed, err
		}
		removed = append(removed, n)
	}
	return removed, nil
}

// --- waiting ------------------------------------------------------------------

func sleepUntil(ctx context.Context, now func() time.Time, t time.Time) bool {
	return sleepFor(ctx, t.Sub(now()))
}

func sleepFor(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
