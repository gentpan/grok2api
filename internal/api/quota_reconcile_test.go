package api

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gentpan/grok2api/internal/account"
)

// fakeUsage reports a healthy quota for every mode, like upstream does when a
// 429 was only a transient limit.
type fakeUsage struct{ calls int }

func (f *fakeUsage) FetchAllQuotas(_ context.Context, _ string, _ string, _ bool) (map[int]account.ModeQuota, error) {
	f.calls++
	return map[int]account.ModeQuota{
		1: {Remaining: 140, Total: 140, WindowSec: 7200},
		5: {Remaining: 20, Total: 20, WindowSec: 3600},
	}, nil
}
func (f *fakeUsage) FetchModeQuota(_ context.Context, _ string, _ string, _ int) (*account.ModeQuota, error) {
	return nil, nil
}

func TestRateLimitIsReconciledWithUpstreamTruth(t *testing.T) {
	ctx := context.Background()
	repo := account.NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	const tok = "tok-reconcile-0123456789abcdef"
	if _, err := repo.UpsertAccounts(ctx, []account.Upsert{{Token: tok, Pool: "basic"}}); err != nil {
		t.Fatal(err)
	}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	fetch := &fakeUsage{}
	s := &Server{Repo: repo, Directory: dir, Refresh: account.NewRefreshService(repo, fetch)}

	const consoleMode = 5
	reserve := func() bool {
		l, _ := dir.Reserve([]int{0}, consoleMode, nil, nil)
		if l != nil {
			dir.Release(l)
		}
		return l != nil
	}
	if !reserve() {
		t.Fatal("account should be reservable for console before any 429")
	}

	// One upstream 429 blacklists the mode locally for a whole window.
	dir.Feedback(tok, account.FbRateLimited, consoleMode, nil, nil)
	if reserve() {
		t.Fatal("precondition: mode should be blocked right after a 429")
	}

	// Reconcile with upstream (healthy) and let the directory sync.
	if !s.reconcileQuota(ctx, tok) {
		t.Fatal("reconcile was skipped")
	}
	if fetch.calls == 0 {
		t.Fatal("upstream quota was not consulted")
	}
	if _, err := dir.SyncIfChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if !reserve() {
		t.Fatal("mode is still blocked although upstream reports available quota")
	}

	// A burst of 429s must not hammer upstream: second call is debounced.
	before := fetch.calls
	if s.reconcileQuota(ctx, tok) {
		t.Fatal("second reconcile within the interval should be skipped")
	}
	if fetch.calls != before {
		t.Fatal("debounced reconcile still called upstream")
	}
}

// A success-triggered quota sync and a 429-triggered confirmation are
// debounced separately: a success must never swallow the confirmation of a
// 429 that follows it, or a transient limit zeroes the mode with nothing to
// heal it.
func TestRateLimitConfirmationIsNotSwallowedByRecentSuccessSync(t *testing.T) {
	ctx := context.Background()
	repo := account.NewTxtRepository(filepath.Join(t.TempDir(), "accounts.jsonl"))
	if err := repo.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	const tok = "tok-reconcile-success-0123456789"
	if _, err := repo.UpsertAccounts(ctx, []account.Upsert{{Token: tok, Pool: "basic"}}); err != nil {
		t.Fatal(err)
	}
	dir := account.NewDirectory(repo)
	if err := dir.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	fetch := &fakeUsage{}
	s := &Server{Repo: repo, Directory: dir, Refresh: account.NewRefreshService(repo, fetch)}

	if !s.reconcileQuotaKind(ctx, tok, reconcileAfterSuccess) {
		t.Fatal("success sync was skipped")
	}
	before := fetch.calls
	if !s.reconcileQuotaKind(ctx, tok, reconcileAfterRateLimit) {
		t.Fatal("429 confirmation was swallowed by the success sync")
	}
	if fetch.calls == before {
		t.Fatal("429 confirmation did not consult upstream")
	}
	// ... while a success right after a fresh confirmation is redundant.
	if s.reconcileQuotaKind(ctx, tok, reconcileAfterSuccess) {
		t.Fatal("success sync should be skipped right after a confirmation")
	}
}
