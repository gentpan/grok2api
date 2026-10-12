package app

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDir struct {
	mu      sync.Mutex
	calls   int
	changed func(call int) (bool, error)
	times   []time.Time
}

func (d *fakeDir) SyncIfChanged(context.Context) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	d.times = append(d.times, time.Now())
	if d.changed == nil {
		return false, nil
	}
	return d.changed(d.calls)
}

func (d *fakeDir) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type fakeRefresher struct {
	mu      sync.Mutex
	started map[string]time.Duration // pool -> interval of the latest start
	starts  map[string]int
	running map[string]int
	resets  atomic.Int32
	recover atomic.Int32
	delays  []time.Duration // initialDelay of every loop start
}

func newFakeRefresher() *fakeRefresher {
	return &fakeRefresher{started: map[string]time.Duration{}, starts: map[string]int{}, running: map[string]int{}}
}

func (f *fakeRefresher) RunScheduledLoop(ctx context.Context, pool string, interval, initialDelay time.Duration) {
	f.mu.Lock()
	f.delays = append(f.delays, initialDelay)
	f.started[pool] = interval
	f.starts[pool]++
	f.running[pool]++
	f.mu.Unlock()
	<-ctx.Done()
	f.mu.Lock()
	f.running[pool]--
	f.mu.Unlock()
}

func (f *fakeRefresher) ResetExpiredConsoleWindows(context.Context) (int, error) {
	f.resets.Add(1)
	return 0, nil
}

func (f *fakeRefresher) RecoverConsoleExpiredAccounts(context.Context) (int, error) {
	f.recover.Add(1)
	return 0, nil
}

func (f *fakeRefresher) snapshot() (running, starts map[string]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	running, starts = map[string]int{}, map[string]int{}
	for k, v := range f.running {
		running[k] = v
	}
	for k, v := range f.starts {
		starts[k] = v
	}
	return
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func newTestScheduler(dir directorySyncer, r refresher) *Scheduler {
	s := NewScheduler(dir, r, Env{SyncIdle: 40 * time.Millisecond, SyncActive: 5 * time.Millisecond})
	s.consoleResetEvery = 5 * time.Millisecond
	s.consoleRecoveryEvery = 5 * time.Millisecond
	return s
}

func settings(enabled bool, basic, super, heavy time.Duration) RefreshSettings {
	return RefreshSettings{Enabled: enabled, Intervals: map[string]time.Duration{"basic": basic, "super": super, "heavy": heavy}}
}

func TestSchedulerStartsRefreshLoopsOnlyWhenEnabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFakeRefresher()
	s := newTestScheduler(&fakeDir{}, r)
	s.Start(ctx, settings(false, time.Hour, time.Hour, time.Hour))

	time.Sleep(30 * time.Millisecond)
	if got := s.ActivePools(); len(got) != 0 {
		t.Fatalf("refresh disabled but pools running: %v", got)
	}
	// The console maintenance loops run regardless.
	eventually(t, "console loops", func() bool { return r.resets.Load() > 0 && r.recover.Load() > 0 })

	cancel()
	s.Wait()
}

// Toggling account.refresh.enabled at runtime must start and stop the loops;
// before this, they were fixed at boot.
func TestSchedulerReconcileFollowsConfig(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFakeRefresher()
	s := newTestScheduler(&fakeDir{}, r)
	s.Start(ctx, settings(false, time.Hour, 2*time.Hour, 3*time.Hour))

	s.Reconcile(settings(true, time.Hour, 2*time.Hour, 3*time.Hour))
	eventually(t, "3 loops running", func() bool {
		run, _ := r.snapshot()
		return run["basic"] == 1 && run["super"] == 1 && run["heavy"] == 1
	})
	if got := s.ActivePools(); got["super"] != 2*time.Hour {
		t.Fatalf("active = %v", got)
	}

	// Same settings again: nothing restarts.
	s.Reconcile(settings(true, time.Hour, 2*time.Hour, 3*time.Hour))
	time.Sleep(20 * time.Millisecond)
	if _, starts := r.snapshot(); starts["basic"] != 1 {
		t.Fatalf("idempotent reconcile restarted a loop: %v", starts)
	}

	// A changed interval restarts only that pool.
	s.Reconcile(settings(true, time.Hour, 5*time.Hour, 3*time.Hour))
	eventually(t, "super restarted", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.started["super"] == 5*time.Hour
	})
	if _, starts := r.snapshot(); starts["basic"] != 1 || starts["heavy"] != 1 {
		t.Fatalf("unrelated pools restarted: %v", starts)
	}

	// Disabling stops everything.
	s.Reconcile(settings(false, time.Hour, 5*time.Hour, 3*time.Hour))
	eventually(t, "all loops stopped", func() bool {
		run, _ := r.snapshot()
		return run["basic"] == 0 && run["super"] == 0 && run["heavy"] == 0
	})
	if len(s.ActivePools()) != 0 {
		t.Fatalf("pools still tracked: %v", s.ActivePools())
	}

	cancel()
	s.Wait()
}

func TestSchedulerReconcileAfterShutdownIsANoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := newFakeRefresher()
	s := newTestScheduler(&fakeDir{}, r)
	s.Start(ctx, settings(false, time.Hour, time.Hour, time.Hour))
	cancel()
	s.Wait()
	s.Reconcile(settings(true, time.Hour, time.Hour, time.Hour))
	if got := s.ActivePools(); len(got) != 0 {
		t.Fatalf("started pools after shutdown: %v", got)
	}
}

func TestSettingsFromAppliesDefaultsAndFloors(t *testing.T) {
	cfg := mapCfg{bools: map[string]bool{"account.refresh.enabled": true},
		ints: map[string]int{"account.refresh.basic_interval_sec": 60, "account.refresh.super_interval_sec": -1}}
	got := SettingsFrom(cfg)
	if !got.Enabled {
		t.Fatal("enabled lost")
	}
	if got.Intervals["basic"] != time.Minute {
		t.Fatalf("basic = %v", got.Intervals["basic"])
	}
	if got.Intervals["super"] != 2*time.Hour {
		t.Fatalf("negative interval must fall back to default, got %v", got.Intervals["super"])
	}
	if got.Intervals["heavy"] != 2*time.Hour {
		t.Fatalf("heavy default = %v", got.Intervals["heavy"])
	}
}

type mapCfg struct {
	bools map[string]bool
	ints  map[string]int
}

func (m mapCfg) GetBool(k string, def bool) bool {
	if v, ok := m.bools[k]; ok {
		return v
	}
	return def
}

func (m mapCfg) GetInt(k string, def int) int {
	if v, ok := m.ints[k]; ok {
		return v
	}
	return def
}

// While changes keep arriving the loop polls at the active pace; after
// idleAfter quiet polls it backs off to the idle pace.
func TestSyncLoopBacksOffWhenQuiet(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &fakeDir{}
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, d, 80*time.Millisecond, 2*time.Millisecond, 3)
		close(done)
	}()
	eventually(t, "polls", func() bool { return d.count() >= 6 })
	cancel()
	<-done

	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.times) < 5 {
		t.Fatalf("polls = %d", len(d.times))
	}
	early := d.times[1].Sub(d.times[0])
	late := d.times[len(d.times)-1].Sub(d.times[len(d.times)-2])
	if late < 40*time.Millisecond || early > 30*time.Millisecond {
		t.Fatalf("no back-off: early gap %v, late gap %v", early, late)
	}
}

func TestSyncLoopBacksOffOnErrorsAndResumesOnChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &fakeDir{changed: func(call int) (bool, error) {
		switch call {
		case 1:
			return false, errors.New("boom")
		case 3:
			return true, nil
		}
		return false, nil
	}}
	go runSyncLoop(ctx, d, 30*time.Millisecond, 2*time.Millisecond, 2)
	eventually(t, "4 polls", func() bool { return d.count() >= 4 })
	cancel()

	d.mu.Lock()
	defer d.mu.Unlock()
	// call 1 errored -> idle pace before call 2.
	if gap := d.times[1].Sub(d.times[0]); gap < 20*time.Millisecond {
		t.Fatalf("error did not back off: %v", gap)
	}
}

func TestSyncLoopStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSyncLoop(ctx, &fakeDir{}, time.Hour, time.Hour, 1)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync loop ignored cancellation")
	}
}

// Starting (or re-timing) a pool loop waits a full interval before its first
// pass; a restart must not fire a burst of upstream quota calls.
func TestRefreshLoopsWaitAFullIntervalBeforeTheFirstPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newFakeRefresher()
	s := newTestScheduler(&fakeDir{}, r)
	s.Start(ctx, settings(true, time.Hour, time.Hour, time.Hour))
	eventually(t, "loops started", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.delays) == 3
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.delays {
		if d >= 0 {
			t.Fatalf("initial delay = %v, want negative (wait one interval)", d)
		}
	}
	cancel()
}
