package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/gentpan/grok2api/internal/platform"
)

// seedN upserts n accounts one call at a time so each gets its own revision.
func seedN(t *testing.T, r *TxtRepository, n int, pool string) []string {
	t.Helper()
	toks := make([]string, 0, n)
	for i := 0; i < n; i++ {
		tok := fmt.Sprintf("tok-seed-%03d-0123456789", i)
		mustUpsert(t, r, tok, pool)
		toks = append(toks, tok)
	}
	return toks
}

// scanAll follows ChangeSet.HasMore like a well-behaved consumer.
func scanAll(t *testing.T, r *TxtRepository, since, limit int) (tokens []string, batches int) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		cs, err := r.ScanChanges(context.Background(), since, limit)
		if err != nil {
			t.Fatal(err)
		}
		batches++
		for _, it := range cs.Items {
			tokens = append(tokens, it.Token)
		}
		if !cs.HasMore {
			return tokens, batches
		}
		if cs.Revision <= since {
			t.Fatalf("HasMore but cursor did not advance: since=%d revision=%d", since, cs.Revision)
		}
		since = cs.Revision
	}
	t.Fatal("scan did not terminate")
	return nil, 0
}

// Bug #1: a truncated batch must not advance the cursor past records that
// were cut off.
func TestScanChangesTruncationDoesNotSkipRecords(t *testing.T) {
	r, _ := newTestRepo(t)
	want := seedN(t, r, 7, "basic")

	got, batches := scanAll(t, r, 0, 3)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records skipped by truncated scan:\n got  %v\n want %v", got, want)
	}
	if batches < 3 {
		t.Fatalf("expected the scan to be split in >=3 batches, got %d", batches)
	}
}

// Records sharing one revision (a batch mutation) must never be split across
// two scans, otherwise the second scan would start after them.
func TestScanChangesNeverSplitsARevisionGroup(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	ups := make([]Upsert, 0, 9)
	for i := 0; i < 9; i++ {
		ups = append(ups, Upsert{Token: fmt.Sprintf("tok-group-%02d-0123456789", i), Pool: "basic"})
	}
	if _, err := r.ReplacePool(ctx, "basic", ups); err != nil {
		t.Fatal(err)
	}
	mustUpsert(t, r, "tok-after-0123456789", "basic")

	got, _ := scanAll(t, r, 0, 4)
	if len(got) != 10 {
		t.Fatalf("got %d records (%v), want 10", len(got), got)
	}
	seen := map[string]bool{}
	for _, tok := range got {
		if seen[tok] {
			t.Fatalf("duplicate %s in scan", tok)
		}
		seen[tok] = true
	}
}

// Deletions must be reported through the same cursor.
func TestScanChangesReportsTombstones(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	toks := seedN(t, r, 3, "basic")
	cs, _ := r.ScanChanges(ctx, 0, 100)
	since := cs.Revision
	if _, err := r.DeleteAccounts(ctx, toks[:1]); err != nil {
		t.Fatal(err)
	}
	cs, err := r.ScanChanges(ctx, since, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.DeletedTokens) != 1 || cs.DeletedTokens[0] != toks[0] {
		t.Fatalf("deleted tokens = %v", cs.DeletedTokens)
	}
}

// Bug #1 (consumer side): Directory must keep pulling while HasMore is set.
func TestDirectorySyncFollowsHasMore(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	d := NewDirectory(r)
	if err := d.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	d.syncBatch = 3
	seedN(t, r, 10, "basic")

	changed, err := d.SyncIfChanged(ctx)
	if err != nil || !changed {
		t.Fatalf("SyncIfChanged: changed=%v err=%v", changed, err)
	}
	if d.Size() != 10 {
		t.Fatalf("directory has %d slots after sync, want 10", d.Size())
	}
	rev, _ := r.GetRevision(ctx)
	if d.Revision() != rev {
		t.Fatalf("directory revision %d, repo %d", d.Revision(), rev)
	}
	if changed, _ := d.SyncIfChanged(ctx); changed {
		t.Fatal("second sync should be a no-op")
	}
}

// Bug #5: Ext numbers are int64 in-process and float64 after a JSON round
// trip; both must be understood.
func TestDeriveStatusMixedNumericTypes(t *testing.T) {
	now := int64(1_700_000_000_000)
	for name, past := range map[string]any{"int64": now - 1000, "float64": float64(now - 1000), "int": int(now - 1000)} {
		rec := &Record{Status: StatusCooling, Ext: map[string]any{"cooldown_until": past}}
		if got := rec.DeriveStatus(now); got != StatusActive {
			t.Errorf("%s past cooldown: status=%v, want active", name, got)
		}
	}
	for name, future := range map[string]any{"int64": now + 60_000, "float64": float64(now + 60_000)} {
		rec := &Record{Status: StatusCooling, Ext: map[string]any{"cooldown_until": future}}
		if got := rec.DeriveStatus(now); got != StatusCooling {
			t.Errorf("%s future cooldown: status=%v, want cooling", name, got)
		}
	}
}

func expireForConsole(t *testing.T, r *TxtRepository, tok string, expiredAt any) {
	t.Helper()
	st := StatusExpired
	reason := "console_429_threshold_exceeded"
	use := 10
	if _, err := r.PatchAccounts(context.Background(), []Patch{{
		Token: tok, Status: &st, StateReason: &reason, UsageUseDelta: &use,
		ExtMerge: map[string]any{"expired_at": expiredAt},
	}}); err != nil {
		t.Fatal(err)
	}
}

// Bug #5: an int64 expired_at written in-process must not read as 0 (which
// made a brand-new expiry look "long expired").
func TestRecoverConsoleExpiredAccountsMixedNumericTypes(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRepo(t)
	now := platform.NowMs()
	mustUpsert(t, r, "tok-fresh-int64-0123456789", "basic")
	mustUpsert(t, r, "tok-old-int64-0123456789", "basic")
	mustUpsert(t, r, "tok-old-float64-0123456789", "basic")
	expireForConsole(t, r, "tok-fresh-int64-0123456789", now)
	expireForConsole(t, r, "tok-old-int64-0123456789", now-2*3600_000)
	expireForConsole(t, r, "tok-old-float64-0123456789", float64(now-2*3600_000))

	n, err := r.RecoverConsoleExpiredAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("recovered %d, want 2 (the two old ones)", n)
	}
	status := func(tok string) Status {
		recs, _ := r.GetAccounts(ctx, []string{tok})
		return recs[0].Status
	}
	if s := status("tok-fresh-int64-0123456789"); s != StatusExpired {
		t.Fatalf("fresh int64 expiry was recovered: %v", s)
	}
	if status("tok-old-int64-0123456789") != StatusActive || status("tok-old-float64-0123456789") != StatusActive {
		t.Fatal("old expiries not recovered")
	}

	// And after a restart (JSON round trip -> float64) the fresh one still waits.
	r2 := NewTxtRepository(path)
	if err := r2.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := r2.RecoverConsoleExpiredAccounts(ctx); n != 0 {
		t.Fatalf("after reload recovered %d, want 0", n)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(`{"n":1}`), &m)
	if toInt64(m["n"]) != 1 {
		t.Fatal("toInt64 must accept JSON float64")
	}
}

// Bug #14: ties must be broken deterministically and pagination must visit
// every record exactly once, ascending and descending.
func TestListAccountsPaginationStableWithTies(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	ups := make([]Upsert, 0, 11)
	for i := 0; i < 11; i++ {
		ups = append(ups, Upsert{Token: fmt.Sprintf("tok-tie-%02d-0123456789", i), Pool: "basic"})
	}
	// One batch: identical CreatedAt/UpdatedAt for every record.
	if _, err := r.UpsertAccounts(ctx, ups); err != nil {
		t.Fatal(err)
	}
	walk := func(sortBy string, desc bool) []string {
		var order []string
		for page := 1; page <= 4; page++ {
			p, err := r.ListAccounts(ctx, ListQuery{Page: page, PageSize: 3, SortBy: sortBy, SortDesc: desc})
			if err != nil {
				t.Fatal(err)
			}
			if p.Total != 11 || p.TotalPages != 4 {
				t.Fatalf("total=%d pages=%d", p.Total, p.TotalPages)
			}
			for _, it := range p.Items {
				order = append(order, it.Token)
			}
		}
		return order
	}
	for _, sortBy := range []string{"", "created_at", "last_use_at", "usage_use_count", "usage_fail_count", "token"} {
		for _, desc := range []bool{false, true} {
			order := walk(sortBy, desc)
			seen := map[string]bool{}
			for _, tok := range order {
				if seen[tok] {
					t.Fatalf("sort=%q desc=%v: duplicate %s across pages", sortBy, desc, tok)
				}
				seen[tok] = true
			}
			if len(order) != 11 {
				t.Fatalf("sort=%q desc=%v: visited %d records, want 11", sortBy, desc, len(order))
			}
			if again := walk(sortBy, desc); fmt.Sprint(order) != fmt.Sprint(again) {
				t.Fatalf("sort=%q desc=%v: order not deterministic", sortBy, desc)
			}
		}
	}
}

// The page-size clamp is visible to the caller instead of silently
// truncating the list.
func TestListAccountsClampIsVisible(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-clamp-0123456789", "basic")

	p, err := r.ListAccounts(ctx, ListQuery{Page: 1, PageSize: MaxPageSize * 10})
	if err != nil {
		t.Fatal(err)
	}
	if p.PageSize != MaxPageSize || !p.Clamped {
		t.Fatalf("PageSize=%d Clamped=%v, want %d/true", p.PageSize, p.Clamped, MaxPageSize)
	}
	p, _ = r.ListAccounts(ctx, ListQuery{Page: 1, PageSize: 10})
	if p.Clamped {
		t.Fatal("Clamped set for an in-range page size")
	}
	p, _ = r.ListAccounts(ctx, ListQuery{})
	if p.PageSize != 50 {
		t.Fatalf("default page size = %d, want 50", p.PageSize)
	}
}

// Bug #13: a patch that matches nothing must not consume a revision nor
// rewrite the file.
func TestNoOpMutationsDoNotBumpRevisionOrFlush(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRepo(t)
	mustUpsert(t, r, "tok-noop-0123456789", "basic")
	rev0, _ := r.GetRevision(ctx)

	// If the repo flushed again the file would reappear.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	reason := "x"
	res, err := r.PatchAccounts(ctx, []Patch{
		{Token: "tok-not-in-pool-0123456789", LastFailReason: &reason},
		{Token: ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Patched != 0 || res.Revision != rev0 {
		t.Fatalf("result = %+v, want Patched=0 Revision=%d", res, rev0)
	}
	if _, err := r.DeleteAccounts(ctx, []string{"tok-not-in-pool-0123456789"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpsertAccounts(ctx, []Upsert{{Token: ""}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PatchAccounts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if rev, _ := r.GetRevision(ctx); rev != rev0 {
		t.Fatalf("revision %d -> %d on no-op mutations", rev0, rev)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no-op mutation flushed the file (stat err=%v)", err)
	}

	// A real patch still works and flushes.
	if res, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-noop-0123456789", LastFailReason: &reason}}); err != nil || res.Patched != 1 || res.Revision != rev0+1 {
		t.Fatalf("real patch: %+v %v", res, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("real patch did not flush: %v", err)
	}
}

// ClearFailures runs first, ExtMerge is applied after it.
func TestPatchClearFailuresAndExtMergeCompose(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-compose-0123456789", "basic")
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-compose-0123456789",
		ExtMerge: map[string]any{"expired_at": 1, "disabled_at": 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-compose-0123456789",
		ClearFailures: true, ExtMerge: map[string]any{"note": "kept"}}}); err != nil {
		t.Fatal(err)
	}
	recs, _ := r.GetAccounts(ctx, []string{"tok-compose-0123456789"})
	ext := recs[0].Ext
	if _, ok := ext["expired_at"]; ok {
		t.Fatalf("failure keys not cleared: %v", ext)
	}
	if ext["note"] != "kept" {
		t.Fatalf("ExtMerge ignored alongside ClearFailures: %v", ext)
	}
}
