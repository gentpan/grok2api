package app

import (
	"context"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/logger"
)

// directorySyncer pulls repository changes into the in-memory account
// directory. *account.Directory implements it.
type directorySyncer interface {
	SyncIfChanged(ctx context.Context) (bool, error)
}

// refresher is the part of *account.RefreshService the scheduler drives.
type refresher interface {
	RunScheduledLoop(ctx context.Context, pool string, interval, initialDelay time.Duration)
	ResetExpiredConsoleWindows(ctx context.Context) (int, error)
	RecoverConsoleExpiredAccounts(ctx context.Context) (int, error)
}

// RefreshSettings is the config-driven part of the schedule. It is re-applied
// with Scheduler.Reconcile whenever the configuration changes.
type RefreshSettings struct {
	Enabled   bool
	Intervals map[string]time.Duration // pool -> interval; <= 0 disables the pool
}

// refreshPools is the fixed pool order with the config key and default
// interval of each pool's quota refresh.
var refreshPools = []struct {
	Pool    string
	Key     string
	Default int // seconds
}{
	{"basic", "account.refresh.basic_interval_sec", 86400},
	{"super", "account.refresh.super_interval_sec", 7200},
	{"heavy", "account.refresh.heavy_interval_sec", 7200},
}

// intGetter is the slice of *config.Snapshot SettingsFrom needs.
type intGetter interface {
	GetBool(key string, def bool) bool
	GetInt(key string, def int) int
}

// SettingsFrom reads the refresh schedule from config.
func SettingsFrom(cfg intGetter) RefreshSettings {
	s := RefreshSettings{
		Enabled:   cfg.GetBool("account.refresh.enabled", false),
		Intervals: make(map[string]time.Duration, len(refreshPools)),
	}
	for _, p := range refreshPools {
		sec := cfg.GetInt(p.Key, p.Default)
		if sec <= 0 {
			sec = p.Default
		}
		s.Intervals[p.Pool] = time.Duration(sec) * time.Second
	}
	return s
}

// Scheduler owns every periodic account job: the directory poll loop, the
// per-pool upstream quota refresh loops and the console maintenance loops. The
// quota refresh loops follow the configuration at runtime (Reconcile starts,
// stops or re-times them); the rest run for the life of the context.
type Scheduler struct {
	dir     directorySyncer
	refresh refresher

	syncIdle, syncActive time.Duration
	idleAfter            int

	consoleResetEvery    time.Duration
	consoleRecoveryEvery time.Duration

	mu    sync.Mutex
	ctx   context.Context
	wg    sync.WaitGroup
	pools map[string]*poolLoop
}

type poolLoop struct {
	interval time.Duration
	cancel   context.CancelFunc
}

// NewScheduler builds a Scheduler with production timings.
func NewScheduler(dir directorySyncer, refresh refresher, env Env) *Scheduler {
	return &Scheduler{
		dir:                  dir,
		refresh:              refresh,
		syncIdle:             env.SyncIdle,
		syncActive:           env.SyncActive,
		idleAfter:            5,
		consoleResetEvery:    30 * time.Second,
		consoleRecoveryEvery: 10 * time.Minute,
		pools:                map[string]*poolLoop{},
	}
}

// Start launches the fixed loops and applies settings. It returns immediately;
// Wait blocks until ctx is done and every loop has exited.
func (s *Scheduler) Start(ctx context.Context, settings RefreshSettings) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()

	s.goLoop(func() { runSyncLoop(ctx, s.dir, s.syncIdle, s.syncActive, s.idleAfter) })
	s.goLoop(func() {
		runCounterLoop(ctx, s.consoleResetEvery, "console quota reset", s.refresh.ResetExpiredConsoleWindows)
	})
	s.goLoop(func() {
		runCounterLoop(ctx, s.consoleRecoveryEvery, "console expired recovery", s.refresh.RecoverConsoleExpiredAccounts)
	})
	s.Reconcile(settings)
}

// Reconcile makes the running quota refresh loops match settings: pools are
// started when refresh is enabled, stopped when it is disabled, and restarted
// when their interval changed. It is safe to call concurrently and repeatedly.
func (s *Scheduler) Reconcile(settings RefreshSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	for pool, loop := range s.pools {
		want, ok := settings.Intervals[pool]
		if !settings.Enabled || !ok || want <= 0 || want != loop.interval {
			s.stopPoolLocked(pool, loop)
		}
	}
	if !settings.Enabled {
		return
	}
	for _, p := range refreshPools {
		interval := settings.Intervals[p.Pool]
		if interval <= 0 || s.pools[p.Pool] != nil {
			continue
		}
		s.startPoolLocked(p.Pool, interval)
	}
}

func (s *Scheduler) startPoolLocked(pool string, interval time.Duration) {
	ctx, cancel := context.WithCancel(s.ctx)
	loop := &poolLoop{interval: interval, cancel: cancel}
	s.pools[pool] = loop
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Wait one full interval before the first pass, on boot and after an
		// interval change alike: each pass costs ~5 upstream calls per
		// account, and a restart loop must not turn into a request burst.
		// Quotas are kept fresh on demand and after every 429/success.
		s.refresh.RunScheduledLoop(ctx, pool, interval, -1)
	}()
	logger.Infof("account refresh loop started: pool=%s interval=%s", pool, interval)
}

func (s *Scheduler) stopPoolLocked(pool string, loop *poolLoop) {
	loop.cancel()
	delete(s.pools, pool)
	logger.Infof("account refresh loop stopped: pool=%s", pool)
}

// ActivePools reports which quota refresh loops are running (for tests and
// diagnostics).
func (s *Scheduler) ActivePools() map[string]time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Duration, len(s.pools))
	for p, l := range s.pools {
		out[p] = l.interval
	}
	return out
}

// Wait blocks until every loop has exited (the Start context is done).
func (s *Scheduler) Wait() { s.wg.Wait() }

func (s *Scheduler) goLoop(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// runSyncLoop polls the directory aggressively while changes keep arriving and
// backs off to the idle pace after idleAfter consecutive empty polls.
func runSyncLoop(ctx context.Context, dir directorySyncer, idle, active time.Duration, idleAfter int) {
	streak := 0
	timer := time.NewTimer(active)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		changed, err := dir.SyncIfChanged(ctx)
		switch {
		case err != nil:
			logger.Debugf("account directory sync error: error=%v", err)
			streak = idleAfter
		case changed:
			streak = 0
		case streak < idleAfter:
			streak++
		}
		next := active
		if streak >= idleAfter {
			next = idle
		}
		timer.Reset(next)
	}
}

// runCounterLoop runs fn every period and logs when it reports work done.
func runCounterLoop(ctx context.Context, period time.Duration, what string, fn func(context.Context) (int, error)) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		n, err := fn(ctx)
		if err != nil {
			logger.Debugf("%s loop error: error=%v", what, err)
			continue
		}
		if n > 0 {
			logger.Infof("%s: count=%d", what, n)
		}
	}
}
