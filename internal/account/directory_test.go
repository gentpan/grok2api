package account

import (
	"context"
	"math"
	"testing"

	"github.com/gentpan/grok2api/internal/platform"
)

func newDirWith(t *testing.T, pool string, tokens ...string) (*Directory, *TxtRepository) {
	t.Helper()
	r, _ := newTestRepo(t)
	for _, tok := range tokens {
		mustUpsert(t, r, tok, pool)
	}
	d := NewDirectory(r)
	if err := d.Bootstrap(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d, r
}

func slotOf(d *Directory, tok string) slot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return *d.slots[tok]
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Bug #9: the quota strategy honours the configured per-account cap instead
// of a hard-coded 12.
func TestQuotaStrategyHonoursMaxInflight(t *testing.T) {
	const tok = "tok-inflight-0123456789"
	d, _ := newDirWith(t, "basic", tok)
	d.SetStrategy(StrategyQuota)
	d.SetMaxInflight(2)
	pools := []int{int(PoolBasic)}

	var leases []*Lease
	for i := 0; i < 2; i++ {
		l, _ := d.Reserve(pools, ModeFast, nil, nil)
		if l == nil {
			t.Fatalf("reservation %d refused below the cap", i)
		}
		leases = append(leases, l)
	}
	if l, _ := d.Reserve(pools, ModeFast, nil, nil); l != nil {
		t.Fatal("reservation beyond max_inflight was granted")
	}
	d.Release(leases[0])
	if l, _ := d.Reserve(pools, ModeFast, nil, nil); l == nil {
		t.Fatal("slot not reusable after release")
	}

	// Raising the cap takes effect at once; non-positive restores the default.
	d.SetMaxInflight(0)
	d.mu.Lock()
	got := d.maxInflight
	d.mu.Unlock()
	if got != DefaultMaxInflight {
		t.Fatalf("maxInflight = %d, want default %d", got, DefaultMaxInflight)
	}
}

func TestReconcileRuntimeSetsStrategyAndCap(t *testing.T) {
	d, _ := newDirWith(t, "basic")
	if got := ReconcileRuntime(d, true, 3); got != StrategyQuota || d.Strategy() != StrategyQuota {
		t.Fatalf("strategy = %v", got)
	}
	d.mu.Lock()
	if d.maxInflight != 3 {
		t.Fatalf("maxInflight = %d", d.maxInflight)
	}
	d.mu.Unlock()
	if got := ReconcileRuntime(d, false, 0); got != StrategyRandom || d.Strategy() != StrategyRandom {
		t.Fatalf("strategy = %v", got)
	}
}

// Characterisation of Directory.Feedback (quota strategy) so the function can
// be split safely.
func TestFeedbackQuotaStrategyTransitions(t *testing.T) {
	const tok = "tok-fb-q-0123456789"
	d, _ := newDirWith(t, "basic", tok)
	d.SetStrategy(StrategyQuota)
	pools := []int{int(PoolBasic)}

	// success: remaining-1, health capped at 1, fail count untouched.
	d.Feedback(tok, FbSuccess, ModeFast, nil, nil)
	s := slotOf(d, tok)
	if s.Quota.Fast.Remaining != BasicFastLimit-1 || !near(s.Health, 1.0) || s.FailCount != 0 {
		t.Fatalf("after success: %+v", s)
	}

	// server error: health * 0.75, fail count +1.
	d.Feedback(tok, FbServerError, ModeFast, nil, nil)
	s = slotOf(d, tok)
	if !near(s.Health, serverErrorFactor) || s.FailCount != 1 || s.LastFailAt == 0 {
		t.Fatalf("after server error: %+v", s)
	}

	// forbidden: health * 0.25 (floored), still selectable.
	d.Feedback(tok, FbForbidden, ModeFast, nil, nil)
	s = slotOf(d, tok)
	if !near(s.Health, serverErrorFactor*forbiddenFactor) || s.FailCount != 2 {
		t.Fatalf("after forbidden: %+v", s)
	}

	// success heals by successStep.
	d.Feedback(tok, FbSuccess, ModeFast, nil, nil)
	if got := slotOf(d, tok).Health; !near(got, serverErrorFactor*forbiddenFactor+successStep) {
		t.Fatalf("health after success = %v", got)
	}

	// rate limited: window zeroed with a reset time one window ahead.
	before := platform.NowMs()
	d.Feedback(tok, FbRateLimited, ModeFast, nil, nil)
	s = slotOf(d, tok)
	if s.Quota.Fast.Remaining != 0 || s.Quota.Fast.ResetAt == nil {
		t.Fatalf("after 429: %+v", s.Quota.Fast)
	}
	if got := *s.Quota.Fast.ResetAt - before; got < int64(BasicFastWindowSec)*1000 || got > int64(BasicFastWindowSec)*1000+5000 {
		t.Fatalf("reset offset = %dms", got)
	}
	if l, _ := d.Reserve(pools, ModeFast, nil, nil); l != nil {
		t.Fatal("exhausted account still reservable")
	}
	// Other modes are untouched by a 429 on one mode.
	if s.Quota.Console == nil || s.Quota.Console.Remaining != BasicConsoleLimit {
		t.Fatalf("console window changed: %+v", s.Quota.Console)
	}

	// Health never drops below the floor.
	for i := 0; i < 20; i++ {
		d.Feedback(tok, FbRateLimited, ModeFast, nil, nil)
	}
	if got := slotOf(d, tok).Health; got < minHealth-1e-12 {
		t.Fatalf("health %v below floor", got)
	}

	// unauthorized removes the account from selection immediately.
	d.Feedback(tok, FbUnauthorized, ModeConsole, nil, nil)
	if slotOf(d, tok).StatusID != StatusIDExpired || d.HasPoolMode(int(PoolBasic), ModeConsole) {
		t.Fatal("unauthorized account still indexed")
	}

	// Kinds without runtime meaning and unknown tokens are ignored.
	for _, k := range []FeedbackKind{FbDisable, FbDelete, FbRestore} {
		d.Feedback(tok, k, ModeFast, nil, nil)
	}
	d.Feedback("tok-unknown-0123456789", FbSuccess, ModeFast, nil, nil)
}

func TestFeedbackQuotaStrategyUpstreamQuotaOverrides(t *testing.T) {
	const tok = "tok-fb-o-0123456789"
	d, _ := newDirWith(t, "basic", tok)
	d.SetStrategy(StrategyQuota)

	rem := 7
	d.Feedback(tok, FbSuccess, ModeFast, &rem, nil)
	if got := slotOf(d, tok).Quota.Fast.Remaining; got != 7 {
		// remaining is written (clamped) after the success decrement.
		t.Fatalf("remaining override = %d, want 7", got)
	}
	// resetAtMs is a duration from now.
	var dur int64 = 90_000
	before := platform.NowMs()
	d.Feedback(tok, FbRateLimited, ModeFast, nil, &dur)
	s := slotOf(d, tok)
	if got := *s.Quota.Fast.ResetAt - before; got < dur || got > dur+5000 {
		t.Fatalf("reset offset = %d, want ~%d", got, dur)
	}
	// Negative remaining clamps to zero.
	neg := -3
	d.Feedback(tok, FbSuccess, ModeFast, &neg, nil)
	if got := slotOf(d, tok).Quota.Fast.Remaining; got != 0 {
		t.Fatalf("negative remaining = %d", got)
	}
}

func TestFeedbackRandomStrategyCooldowns(t *testing.T) {
	const tok = "tok-fb-r-0123456789"
	d, _ := newDirWith(t, "basic", tok)
	d.SetStrategy(StrategyRandom)
	nowSec := platform.NowMs() / 1000

	d.Feedback(tok, FbRateLimited, ModeFast, nil, nil)
	got := slotOf(d, tok).CoolingUntil - nowSec
	if got < 86400-2 || got > 86400+5 {
		t.Fatalf("basic cooldown = %ds, want ~86400", got)
	}
	d.Feedback(tok, FbRateLimited, ModeConsole, nil, nil)
	// The longer existing cooldown wins.
	if got := slotOf(d, tok).CoolingUntil - nowSec; got < 86400-2 {
		t.Fatalf("cooldown shortened to %d", got)
	}

	// Console cool-down is its own, shorter value.
	d2, _ := newDirWith(t, "basic", tok)
	d2.SetStrategy(StrategyRandom)
	d2.Feedback(tok, FbRateLimited, ModeConsole, nil, nil)
	got = slotOf(d2, tok).CoolingUntil - nowSec
	if got < consoleCoolingSec-2 || got > consoleCoolingSec+5 {
		t.Fatalf("console cooldown = %ds, want ~%d", got, consoleCoolingSec)
	}
	if l, _ := d2.Reserve([]int{int(PoolBasic)}, ModeFast, nil, nil); l != nil {
		t.Fatal("cooling account still selectable")
	}

	// Super pool: 2h.
	d3, _ := newDirWith(t, "super", tok)
	d3.SetStrategy(StrategyRandom)
	d3.Feedback(tok, FbRateLimited, ModeFast, nil, nil)
	if got := slotOf(d3, tok).CoolingUntil - nowSec; got < 7200-2 || got > 7200+5 {
		t.Fatalf("super cooldown = %ds", got)
	}
}

// Reserve hands out value-type leases and the directory exposes no live slots.
func TestReserveLeaseIsAValue(t *testing.T) {
	const tok = "tok-lease-0123456789"
	d, _ := newDirWith(t, "super", tok)
	l, mode := d.Reserve([]int{int(PoolSuper)}, ModeAuto, nil, nil)
	if l == nil || mode != ModeAuto || l.Token != tok || l.ModeID != ModeAuto || l.PoolID != PoolSuper {
		t.Fatalf("lease = %+v mode=%d", l, mode)
	}
	if slotOf(d, tok).Inflight != 1 {
		t.Fatal("inflight not counted")
	}
	d.Release(l)
	d.Release(l) // double release must not go negative
	if slotOf(d, tok).Inflight != 0 {
		t.Fatalf("inflight = %d", slotOf(d, tok).Inflight)
	}
}
