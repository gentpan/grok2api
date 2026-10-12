package account

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gentpan/grok2api/internal/platform"
)

func newTestRepo(t *testing.T) (*TxtRepository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "accounts.jsonl")
	r := NewTxtRepository(path)
	if err := r.Initialize(context.Background()); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return r, path
}

func mustUpsert(t *testing.T, r *TxtRepository, token, pool string) {
	t.Helper()
	_, err := r.UpsertAccounts(context.Background(), []Upsert{{
		Token: token, Pool: pool, Tags: []string{"a"}, Ext: map[string]any{"k": map[string]any{"n": 1}},
	}})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

// H5: every read path must hand out deep copies.
func TestRepositoryReturnsDeepCopies(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-aaaaaaaaaaaa", "super")

	mutate := func(rec *Record) {
		rec.Tags[0] = "mutated"
		rec.Quota["fast"].(map[string]any)["remaining"] = 12345
		rec.Quota["injected"] = true
		rec.Ext["k"].(map[string]any)["n"] = 999
		rec.Ext["injected"] = true
		rec.Status = StatusDisabled
	}
	assertPristine := func(where string) {
		t.Helper()
		recs, _ := r.GetAccounts(ctx, []string{"tok-aaaaaaaaaaaa"})
		if len(recs) != 1 {
			t.Fatalf("%s: want 1 record, got %d", where, len(recs))
		}
		got := recs[0]
		if got.Tags[0] != "a" || got.Status != StatusActive {
			t.Fatalf("%s: tags/status leaked: %v %v", where, got.Tags, got.Status)
		}
		if _, ok := got.Quota["injected"]; ok {
			t.Fatalf("%s: quota map aliased", where)
		}
		if got.Quota["fast"].(map[string]any)["remaining"] == 12345 {
			t.Fatalf("%s: nested quota map aliased", where)
		}
		if _, ok := got.Ext["injected"]; ok || got.Ext["k"].(map[string]any)["n"] == 999 {
			t.Fatalf("%s: ext map aliased", where)
		}
	}

	recs, _ := r.GetAccounts(ctx, []string{"tok-aaaaaaaaaaaa"})
	mutate(recs[0])
	assertPristine("GetAccounts")

	snap, _ := r.RuntimeSnapshot(ctx)
	mutate(snap.Items[0])
	assertPristine("RuntimeSnapshot")

	cs, _ := r.ScanChanges(ctx, 0, 10)
	mutate(cs.Items[0])
	assertPristine("ScanChanges")

	page, _ := r.ListAccounts(ctx, ListQuery{})
	mutate(page.Items[0])
	assertPristine("ListAccounts")
}

// H5 (run under -race): readers use returned records while patches write.
func TestRepositoryConcurrentPatchAndRead(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-bbbbbbbbbbbb", "basic")

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m := map[string]any{"remaining": i, "total": 30, "window_seconds": 86400}
			_, _ = r.PatchAccounts(ctx, []Patch{{
				Token:     "tok-bbbbbbbbbbbb",
				QuotaFast: &m,
				ExtMerge:  map[string]any{"x": i},
			}})
		}
	}()
	for i := 0; i < 200; i++ {
		cs, _ := r.ScanChanges(ctx, 0, 10)
		for _, rec := range cs.Items {
			_ = rec.QuotaSet()
			_ = rec.DeriveStatus(0)
			_ = rec.Ext["x"]
		}
		recs, _ := r.GetAccounts(ctx, []string{"tok-bbbbbbbbbbbb"})
		for _, rec := range recs {
			_ = rec.QuotaSet()
		}
	}
	close(stop)
	wg.Wait()
}

// H5: markCredentialsExpired must not write into the record it was given.
func TestMarkCredentialsExpiredDoesNotMutateRecord(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-cccccccccccc", "basic")
	svc := NewRefreshService(r, nil)

	recs, _ := r.GetAccounts(ctx, []string{"tok-cccccccccccc"})
	rec := recs[0]
	before := len(rec.Ext)
	if err := svc.markCredentialsExpired(ctx, rec); err != nil {
		t.Fatalf("markCredentialsExpired: %v", err)
	}
	if len(rec.Ext) != before {
		t.Fatalf("caller's rec.Ext was mutated: %v", rec.Ext)
	}
	got, _ := r.GetAccounts(ctx, []string{"tok-cccccccccccc"})
	if got[0].Status != StatusExpired || got[0].Ext["expired_reason"] != "invalid_credentials" {
		t.Fatalf("repo not updated: status=%v ext=%v", got[0].Status, got[0].Ext)
	}
}

// M6: short tokens must not panic anywhere in the logging path.
func TestMarkCredentialsExpiredShortToken(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "abc", "basic")
	svc := NewRefreshService(r, nil)
	recs, _ := r.GetAccounts(ctx, []string{"abc"})
	if err := svc.markCredentialsExpired(ctx, recs[0]); err != nil {
		t.Fatal(err)
	}
	if got := platform.MaskToken("abc"); strings.Contains(got, "abc") {
		t.Fatalf("short token leaked: %q", got)
	}
}

// M1: the file is 0600, no temp files are left behind, content round-trips.
func TestFlushAtomicAndPrivate(t *testing.T) {
	r, path := newTestRepo(t)
	mustUpsert(t, r, "tok-dddddddddddd", "basic")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			t.Fatalf("unexpected leftover file %q", e.Name())
		}
	}
	r2 := NewTxtRepository(path)
	if err := r2.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recs, _ := r2.GetAccounts(context.Background(), []string{"tok-dddddddddddd"}); len(recs) != 1 {
		t.Fatalf("round trip lost record")
	}
}

// M2: malformed lines are skipped, counted and the original file is backed up.
func TestLoadMalformedLinesBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.jsonl")
	good := `{"token":"tok-eeeeeeeeeeee","pool":"basic","status":"active","revision":3}`
	content := good + "\n{this is not json\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewTxtRepository(path)
	if err := r.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	recs, _ := r.GetAccounts(context.Background(), []string{"tok-eeeeeeeeeeee"})
	if len(recs) != 1 {
		t.Fatalf("good line not loaded")
	}
	b, err := os.ReadFile(path + ".corrupt")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(b) != content {
		t.Fatalf("backup content differs: %q", b)
	}
	// A second load must not overwrite the first backup.
	r2 := NewTxtRepository(path)
	_ = os.WriteFile(path, []byte(content), 0o644)
	if err := r2.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b2, _ := os.ReadFile(path + ".corrupt"); string(b2) != content {
		t.Fatalf("first backup was overwritten")
	}
}

// M3: only windows whose reset time has passed are reset; revision only moves
// when something changed.
func TestResetExpiredConsoleWindows(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	now := platform.NowMs()
	mustUpsert(t, r, "tok-default-window", "basic")   // default window: ResetAt == nil
	mustUpsert(t, r, "tok-exhausted-future", "basic") // exhausted, reset in the future
	mustUpsert(t, r, "tok-expired-window", "basic")   // reset time in the past

	future := now + 3_600_000
	past := now - 1000
	exhausted := map[string]any{"remaining": 0, "total": 20, "window_seconds": 3600, "reset_at": future}
	expired := map[string]any{"remaining": 2, "total": 20, "window_seconds": 3600, "reset_at": past}
	if _, err := r.PatchAccounts(ctx, []Patch{
		{Token: "tok-exhausted-future", QuotaConsole: &exhausted},
		{Token: "tok-expired-window", QuotaConsole: &expired},
	}); err != nil {
		t.Fatal(err)
	}

	revBefore, _ := r.GetRevision(ctx)
	n, err := r.ResetExpiredConsoleWindows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("reset = %d, want 1 (only the expired window)", n)
	}
	get := func(tok string) *Record {
		recs, _ := r.GetAccounts(ctx, []string{tok})
		return recs[0]
	}
	if w := get("tok-expired-window").QuotaSet().Console; w == nil || w.Remaining != 20 {
		t.Fatalf("expired window not reset: %+v", w)
	}
	if w := get("tok-exhausted-future").QuotaSet().Console; w.Remaining != 0 {
		t.Fatalf("exhausted window with future reset must be left alone: %+v", w)
	}
	if rec := get("tok-default-window"); rec.Revision >= revBefore+1 {
		t.Fatalf("default window record was touched: rev=%d", rec.Revision)
	}
	revAfter, _ := r.GetRevision(ctx)
	if revAfter != revBefore+1 {
		t.Fatalf("revision %d -> %d, want +1", revBefore, revAfter)
	}

	// Nothing left to reset: no revision bump, no change.
	n, err = r.ResetExpiredConsoleWindows(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second run: n=%d err=%v", n, err)
	}
	if rev2, _ := r.GetRevision(ctx); rev2 != revAfter {
		t.Fatalf("revision bumped without changes: %d -> %d", revAfter, rev2)
	}
}

// M11: random strategy no longer excludes an account forever.
func TestRandomStrategyFailCountRecovers(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-ffffffffffff", "basic")
	d := NewDirectory(r)
	if err := d.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	d.SetStrategy(StrategyRandom)
	pools := []int{int(PoolBasic)}

	for i := 0; i < randomMaxFails; i++ {
		d.Feedback("tok-ffffffffffff", FbServerError, 1, nil, nil)
	}
	if l, _ := d.Reserve(pools, 1, nil, nil); l != nil {
		t.Fatalf("account should be excluded right after %d failures", randomMaxFails)
	}

	// After the recovery window it is probed again; a success clears the streak.
	d.mu.Lock()
	d.slots["tok-ffffffffffff"].LastFailAt = platform.NowMs() - (randomFailRecoverSec+1)*1000
	d.mu.Unlock()
	l, _ := d.Reserve(pools, 1, nil, nil)
	if l == nil {
		t.Fatalf("account not selectable after recovery window")
	}
	d.Release(l)
	d.Feedback(l.Token, FbSuccess, 1, nil, nil)
	d.mu.Lock()
	fc := d.slots["tok-ffffffffffff"].FailCount
	d.mu.Unlock()
	if fc != 0 {
		t.Fatalf("FailCount = %d after success, want 0", fc)
	}
}

// M11: quota strategy path must be unchanged (success does not clear FailCount).
func TestQuotaStrategyFailCountUnchangedOnSuccess(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-gggggggggggg", "basic")
	d := NewDirectory(r)
	_ = d.Bootstrap(ctx)
	d.SetStrategy(StrategyQuota)
	d.Feedback("tok-gggggggggggg", FbServerError, 1, nil, nil)
	d.Feedback("tok-gggggggggggg", FbSuccess, 1, nil, nil)
	d.mu.Lock()
	fc := d.slots["tok-gggggggggggg"].FailCount
	d.mu.Unlock()
	if fc != 1 {
		t.Fatalf("quota strategy FailCount = %d, want 1", fc)
	}
}
