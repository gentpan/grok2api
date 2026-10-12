package account

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gentpan/grok2api/internal/platform"
)

// fakeFetcher is a scriptable UsageFetcher.
type fakeFetcher struct {
	mu      sync.Mutex
	calls   map[string]int
	total   int32
	cur     int32
	maxCur  int32
	fn      func(ctx context.Context, token, pool string) (map[int]ModeQuota, error)
	started chan string // optional: receives the token when a fetch begins
}

func (f *fakeFetcher) FetchAllQuotas(ctx context.Context, token, pool string, _ bool) (map[int]ModeQuota, error) {
	atomic.AddInt32(&f.total, 1)
	c := atomic.AddInt32(&f.cur, 1)
	for {
		m := atomic.LoadInt32(&f.maxCur)
		if c <= m || atomic.CompareAndSwapInt32(&f.maxCur, m, c) {
			break
		}
	}
	defer atomic.AddInt32(&f.cur, -1)
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[token]++
	f.mu.Unlock()
	if f.started != nil {
		select {
		case f.started <- token:
		default:
		}
	}
	if f.fn != nil {
		return f.fn(ctx, token, pool)
	}
	return healthyQuotas(), nil
}

func (f *fakeFetcher) FetchModeQuota(context.Context, string, string, int) (*ModeQuota, error) {
	return nil, nil
}

func (f *fakeFetcher) callsFor(tok string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[tok]
}

func healthyQuotas() map[int]ModeQuota {
	return map[int]ModeQuota{
		ModeFast:    {Remaining: 21, Total: 30, WindowSec: 86400},
		ModeConsole: {Remaining: 5, Total: 20, WindowSec: 3600},
	}
}

func getRec(t *testing.T, r *TxtRepository, tok string) *Record {
	t.Helper()
	recs, err := r.GetAccounts(context.Background(), []string{tok})
	if err != nil || len(recs) != 1 {
		t.Fatalf("GetAccounts(%s): %v %v", tok, recs, err)
	}
	return recs[0]
}

func invalidCredsErr() error {
	return fmt.Errorf("fetch rate limits: %w", platform.UpstreamError("rejected", 401, `{"code":"bad-credentials"}`))
}

// Bug #3: an all-modes-failed fetch ((nil, nil) from grok.UsageFetcher) is a
// failure that is recorded on the account, not a silent success.
func TestRefreshEmptyResultIsRecordedFailure(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-empty-0123456789", "basic")
	f := &fakeFetcher{fn: func(context.Context, string, string) (map[int]ModeQuota, error) { return nil, nil }}
	svc := NewRefreshService(r, f)

	refreshed, failed, err := svc.RefreshTokens(ctx, []string{"tok-empty-0123456789"})
	if err != nil || refreshed != 0 || failed != 1 {
		t.Fatalf("RefreshTokens = %d,%d,%v want 0,1,nil", refreshed, failed, err)
	}
	rec := getRec(t, r, "tok-empty-0123456789")
	if rec.UsageFailCount != 1 || rec.LastFailAt == nil || rec.LastFailReason == nil {
		t.Fatalf("failure not recorded: %+v", rec)
	}
	if rec.LastSyncAt != nil || rec.UsageSyncCount != 0 {
		t.Fatalf("empty refresh must not count as a sync: %+v", rec)
	}
	if rec.Status != StatusActive {
		t.Fatalf("transient failure changed status to %v", rec.Status)
	}

	outs := svc.RefreshTokensDetailed(ctx, []string{"tok-empty-0123456789"})
	if len(outs) != 1 || outs[0].Refreshed || !errors.Is(outs[0].Err, ErrNoQuotaData) {
		t.Fatalf("outcome = %+v", outs)
	}
}

// Modes the pool does not serve do not count as usable data either.
func TestRefreshOnlyUnsupportedModesIsFailure(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-unsup-0123456789", "basic")
	f := &fakeFetcher{fn: func(context.Context, string, string) (map[int]ModeQuota, error) {
		return map[int]ModeQuota{ModeExpert: {Remaining: 1, Total: 1, WindowSec: 60}}, nil
	}}
	svc := NewRefreshService(r, f)
	outs := svc.RefreshTokensDetailed(ctx, []string{"tok-unsup-0123456789"})
	if outs[0].Refreshed || !errors.Is(outs[0].Err, ErrNoQuotaData) {
		t.Fatalf("outcome = %+v", outs[0])
	}
}

// Bug #6/errors.As: a wrapped AppError still marks the token expired, and the
// outcome exposes ErrInvalidCredentials without leaking the raw token.
func TestRefreshWrappedInvalidCredentialsExpiresAccount(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	const tok = "tok-badcred-0123456789abcdef"
	mustUpsert(t, r, tok, "basic")
	f := &fakeFetcher{fn: func(context.Context, string, string) (map[int]ModeQuota, error) { return nil, invalidCredsErr() }}
	svc := NewRefreshService(r, f)

	outs := svc.RefreshTokensDetailed(ctx, []string{tok})
	if len(outs) != 1 || !outs[0].Expired || outs[0].Refreshed || !errors.Is(outs[0].Err, ErrInvalidCredentials) {
		t.Fatalf("outcome = %+v", outs)
	}
	if msg := outs[0].Err.Error(); containsStr(msg, tok) {
		t.Fatalf("error leaks the full token: %q", msg)
	}
	rec := getRec(t, r, tok)
	if rec.Status != StatusExpired || rec.Ext[ExtExpiredReason] != ReasonInvalidCredentials {
		t.Fatalf("account not expired: status=%v ext=%v", rec.Status, rec.Ext)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRecordFailureHandlesWrappedInvalidCredentials(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-rf-0123456789", "basic")
	svc := NewRefreshService(r, nil)

	svc.RecordFailure(ctx, "tok-rf-0123456789", 1, errors.New("boom"))
	rec := getRec(t, r, "tok-rf-0123456789")
	if rec.UsageFailCount != 1 || rec.Status != StatusActive {
		t.Fatalf("plain failure: %+v", rec)
	}
	svc.RecordFailure(ctx, "tok-rf-0123456789", 1, invalidCredsErr())
	if rec := getRec(t, r, "tok-rf-0123456789"); rec.Status != StatusExpired {
		t.Fatalf("wrapped invalid credentials did not expire the account: %v", rec.Status)
	}
	// A token outside the pool must not consume a revision.
	before, _ := r.GetRevision(ctx)
	svc.RecordFailure(ctx, "tok-not-in-pool-0123456789", 1, errors.New("boom"))
	if after, _ := r.GetRevision(ctx); after != before {
		t.Fatalf("revision bumped for unknown token: %d -> %d", before, after)
	}
}

func TestRefreshTokensDetailedReportsEachToken(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-ok-0123456789", "basic")
	mustUpsert(t, r, "tok-bad-0123456789", "basic")
	f := &fakeFetcher{fn: func(_ context.Context, tok, _ string) (map[int]ModeQuota, error) {
		if tok == "tok-bad-0123456789" {
			return nil, errors.New("upstream 503")
		}
		return healthyQuotas(), nil
	}}
	svc := NewRefreshService(r, f)

	outs := svc.RefreshTokensDetailed(ctx, []string{"tok-ok-0123456789", "tok-bad-0123456789", "tok-missing-0123456789", "tok-ok-0123456789"})
	if len(outs) != 3 {
		t.Fatalf("want 3 distinct outcomes, got %d: %+v", len(outs), outs)
	}
	if outs[0].Token != "tok-ok-0123456789" || !outs[0].Refreshed || outs[0].Err != nil {
		t.Fatalf("ok outcome: %+v", outs[0])
	}
	if outs[1].Token != "tok-bad-0123456789" || outs[1].Refreshed || outs[1].Err == nil {
		t.Fatalf("bad outcome: %+v", outs[1])
	}
	if outs[2].Token != "tok-missing-0123456789" || outs[2].Refreshed || outs[2].Err == nil {
		t.Fatalf("missing outcome: %+v", outs[2])
	}
	if rec := getRec(t, r, "tok-ok-0123456789"); rec.LastSyncAt == nil || rec.UsageSyncCount != 1 {
		t.Fatalf("success not stored: %+v", rec)
	}
	w := getRec(t, r, "tok-ok-0123456789").QuotaSet().Fast
	if w.Remaining != 21 || w.Source != SourceReal {
		t.Fatalf("fast window not stored: %+v", w)
	}

	// Existing compact API keeps working.
	refreshed, failed, err := svc.RefreshTokens(ctx, []string{"tok-ok-0123456789", "tok-bad-0123456789"})
	if err != nil || refreshed != 1 || failed != 1 {
		t.Fatalf("RefreshTokens = %d,%d,%v", refreshed, failed, err)
	}
}
