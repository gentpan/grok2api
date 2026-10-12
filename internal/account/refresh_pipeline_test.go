package account

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func seedTokens(t *testing.T, r *TxtRepository, n int, pool string) []string {
	t.Helper()
	ups := make([]Upsert, 0, n)
	toks := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tok := fmt.Sprintf("tok-pipe-%04d-0123456789", i)
		ups = append(ups, Upsert{Token: tok, Pool: pool})
		toks = append(toks, tok)
	}
	if _, err := r.UpsertAccounts(context.Background(), ups); err != nil {
		t.Fatal(err)
	}
	return toks
}

// One batch -> one PatchAccounts call -> exactly one revision / file flush.
func TestRefreshScheduledBatchesIntoOneFlush(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	toks := seedTokens(t, r, 25, "basic")
	svc := NewRefreshService(r, &fakeFetcher{})

	before, _ := r.GetRevision(ctx)
	refreshed, failed, err := svc.RefreshScheduled(ctx, "")
	if err != nil || refreshed != 25 || failed != 0 {
		t.Fatalf("RefreshScheduled = %d,%d,%v", refreshed, failed, err)
	}
	after, _ := r.GetRevision(ctx)
	if after != before+1 {
		t.Fatalf("revision %d -> %d, want exactly one flush for the batch", before, after)
	}
	for _, tok := range toks {
		if rec := getRec(t, r, tok); rec.UsageSyncCount != 1 || rec.QuotaSet().Fast.Remaining != 21 {
			t.Fatalf("%s not refreshed: %+v", tok, rec)
		}
	}

	// Larger than one batch: ceil(25/10) = 3 flushes.
	svc.batchSize = 10
	before = after
	if _, _, err := svc.RefreshScheduled(ctx, ""); err != nil {
		t.Fatal(err)
	}
	after, _ = r.GetRevision(ctx)
	if after != before+3 {
		t.Fatalf("revision %d -> %d, want 3 flushes", before, after)
	}
}

// Bug #2: RefreshScheduled walks every page, not just the first one.
func TestRefreshScheduledPaginatesThroughAllAccounts(t *testing.T) {
	ctx := context.Background()
	old := scheduledPageSize
	scheduledPageSize = 4
	defer func() { scheduledPageSize = old }()

	r, _ := newTestRepo(t)
	seedTokens(t, r, 11, "basic")
	// A cooling account is refreshed too, an expired one is not.
	cooling := StatusCooling
	expired := StatusExpired
	mustUpsert(t, r, "tok-cooling-0123456789", "basic")
	mustUpsert(t, r, "tok-expired-0123456789", "basic")
	if _, err := r.PatchAccounts(ctx, []Patch{
		{Token: "tok-cooling-0123456789", Status: &cooling},
		{Token: "tok-expired-0123456789", Status: &expired},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{}
	svc := NewRefreshService(r, f)
	refreshed, failed, err := svc.RefreshScheduled(ctx, "")
	if err != nil || refreshed != 12 || failed != 0 {
		t.Fatalf("RefreshScheduled = %d,%d,%v want 12,0,nil", refreshed, failed, err)
	}
	if f.callsFor("tok-expired-0123456789") != 0 || f.callsFor("tok-cooling-0123456789") != 1 {
		t.Fatalf("wrong accounts refreshed: %v", f.calls)
	}
}

func TestRefreshScheduledFiltersByPool(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-b-0123456789", "basic")
	mustUpsert(t, r, "tok-s-0123456789", "super")
	f := &fakeFetcher{fn: func(context.Context, string, string) (map[int]ModeQuota, error) {
		return map[int]ModeQuota{ModeAuto: {Remaining: 50, Total: 50, WindowSec: 7200}, ModeFast: {Remaining: 140, Total: 140, WindowSec: 7200}}, nil
	}}
	svc := NewRefreshService(r, f)
	if n, _, _ := svc.RefreshScheduled(ctx, "Super"); n != 1 {
		t.Fatalf("refreshed %d, want 1", n)
	}
	if f.callsFor("tok-b-0123456789") != 0 || f.callsFor("tok-s-0123456789") != 1 {
		t.Fatalf("calls = %v", f.calls)
	}
}

// Upstream concurrency is bounded no matter how many accounts there are.
func TestRefreshConcurrencyIsBounded(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	seedTokens(t, r, 40, "basic")
	f := &fakeFetcher{fn: func(context.Context, string, string) (map[int]ModeQuota, error) {
		time.Sleep(5 * time.Millisecond)
		return healthyQuotas(), nil
	}}
	svc := NewRefreshService(r, f)
	svc.fetchSem = make(chan struct{}, 3)
	if n, _, err := svc.RefreshScheduled(ctx, ""); err != nil || n != 40 {
		t.Fatalf("refreshed %d err=%v", n, err)
	}
	if m := atomic.LoadInt32(&f.maxCur); m > 3 || m < 2 {
		t.Fatalf("max concurrent fetches = %d, want 2..3", m)
	}
}

// Concurrent refreshes of one token share a single upstream round trip.
func TestRefreshSingleflightPerToken(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-sf-0123456789", "basic")
	gate := make(chan struct{})
	f := &fakeFetcher{
		started: make(chan string, 64),
		fn: func(context.Context, string, string) (map[int]ModeQuota, error) {
			<-gate
			return healthyQuotas(), nil
		},
	}
	svc := NewRefreshService(r, f)

	const callers = 8
	var wg sync.WaitGroup
	results := make([]RefreshOutcome, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = svc.RefreshTokensDetailed(ctx, []string{"tok-sf-0123456789"})[0]
		}(i)
	}
	<-f.started // the leader is inside the fetcher
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	if n := f.callsFor("tok-sf-0123456789"); n != 1 {
		t.Fatalf("upstream called %d times, want 1", n)
	}
	for i, o := range results {
		if !o.Refreshed || o.Err != nil {
			t.Fatalf("caller %d: %+v", i, o)
		}
	}
	if rec := getRec(t, r, "tok-sf-0123456789"); rec.UsageSyncCount != 1 {
		t.Fatalf("sync counted %d times", rec.UsageSyncCount)
	}
}

// A cancelled caller neither hangs nor penalises the account.
func TestRefreshCancellationDoesNotRecordFailure(t *testing.T) {
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-cx-0123456789", "basic")
	f := &fakeFetcher{
		started: make(chan string, 1),
		fn: func(ctx context.Context, _ string, _ string) (map[int]ModeQuota, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	svc := NewRefreshService(r, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan []RefreshOutcome, 1)
	go func() { done <- svc.RefreshTokensDetailed(ctx, []string{"tok-cx-0123456789"}) }()
	<-f.started
	cancel()
	select {
	case outs := <-done:
		if outs[0].Refreshed || !errors.Is(outs[0].Err, context.Canceled) {
			t.Fatalf("outcome = %+v", outs[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not return after cancellation")
	}
	if rec := getRec(t, r, "tok-cx-0123456789"); rec.UsageFailCount != 0 || rec.LastFailAt != nil {
		t.Fatalf("cancellation recorded as account failure: %+v", rec)
	}
	// The singleflight slot was released.
	f.fn = nil
	if outs := svc.RefreshTokensDetailed(context.Background(), []string{"tok-cx-0123456789"}); !outs[0].Refreshed {
		t.Fatalf("follow-up refresh failed: %+v", outs[0])
	}
}

// RefreshOnDemand: bounded wait, coalesced callers, throttled afterwards.
func TestRefreshOnDemandIsBoundedAndCoalesced(t *testing.T) {
	r, _ := newTestRepo(t)
	seedTokens(t, r, 3, "basic")
	gate := make(chan struct{})
	f := &fakeFetcher{
		started: make(chan string, 16),
		fn: func(ctx context.Context, _ string, _ string) (map[int]ModeQuota, error) {
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return healthyQuotas(), nil
		},
	}
	svc := NewRefreshService(r, f)
	svc.onDemandWait = 80 * time.Millisecond

	// Several concurrent callers: none blocks longer than the wait.
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := svc.RefreshOnDemand(context.Background()); err != nil {
				t.Errorf("RefreshOnDemand: %v", err)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("callers blocked for %v", d)
	}
	<-f.started // pass is running in the background
	close(gate)

	// Join the finished pass: exactly one fetch per account in total.
	deadline := time.Now().Add(3 * time.Second)
	for {
		svc.onDemandMu.Lock()
		running := svc.onDemandRun != nil
		svc.onDemandMu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("on-demand pass never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&f.total); got != 3 {
		t.Fatalf("upstream fetches = %d, want 3 (one per account)", got)
	}

	// Within the throttle interval another call is a no-op.
	if err := svc.RefreshOnDemand(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&f.total); got != 3 {
		t.Fatalf("throttled call hit upstream: %d fetches", got)
	}
	svc.Close()
}

func TestRefreshOnDemandCallerContextEndsWait(t *testing.T) {
	r, _ := newTestRepo(t)
	seedTokens(t, r, 1, "basic")
	f := &fakeFetcher{fn: func(ctx context.Context, _ string, _ string) (map[int]ModeQuota, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := NewRefreshService(r, f)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := svc.RefreshOnDemand(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	svc.Close() // cancels the background pass and waits for it
}

func TestRunScheduledLoopRunsFirstPassImmediately(t *testing.T) {
	r, _ := newTestRepo(t)
	seedTokens(t, r, 2, "basic")
	f := &fakeFetcher{}
	svc := NewRefreshService(r, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.RunScheduledLoop(ctx, "basic", time.Hour, 0)
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&f.total) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("first pass did not run at startup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}

	// Negative initial delay waits a full interval: nothing runs.
	g := &fakeFetcher{}
	svc2 := NewRefreshService(r, g)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	svc2.RunScheduledLoop(ctx2, "basic", time.Hour, -1)
	if atomic.LoadInt32(&g.total) != 0 {
		t.Fatal("pass ran despite a negative initial delay")
	}
}

// A refresh written through a SyncingRepository is visible in the directory at
// once, without waiting for the poll loop.
func TestRefreshThroughSyncingRepositoryIsVisibleImmediately(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-att-0123456789", "basic")
	d := NewDirectory(r)
	if err := d.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	svc := NewRefreshService(NewSyncingRepository(r, d), &fakeFetcher{})
	rev := d.Revision()
	if _, _, err := svc.RefreshTokens(ctx, []string{"tok-att-0123456789"}); err != nil {
		t.Fatal(err)
	}
	if d.Revision() <= rev {
		t.Fatalf("directory revision did not advance: %d -> %d", rev, d.Revision())
	}
	d.mu.Lock()
	rem := d.slots["tok-att-0123456789"].Quota.Fast.Remaining
	d.mu.Unlock()
	if rem != 21 {
		t.Fatalf("slot quota = %d, want the refreshed 21", rem)
	}
}

// After Close, an on-demand refresh must not start a background pass: it would
// race Close's wait group and outlive the service.
func TestRefreshOnDemandAfterCloseStartsNothing(t *testing.T) {
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-closed-0123456789", "basic")
	g := &fakeFetcher{}
	svc := NewRefreshService(r, g)
	svc.Close()

	if err := svc.RefreshOnDemand(context.Background()); err != nil {
		t.Fatalf("RefreshOnDemand after Close = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&g.total); n != 0 {
		t.Fatalf("a pass ran after Close: %d fetches", n)
	}
}
