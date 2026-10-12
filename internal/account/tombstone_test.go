package account

import (
	"context"
	"os"
	"testing"
	"time"
)

func hasRecord(r *TxtRepository, tok string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.accounts[tok]
	return ok
}

func TestMethodsHonourCancelledContext(t *testing.T) {
	r, _ := newTestRepo(t)
	mustUpsert(t, r, "tok-ctx-0123456789", "basic")
	rev, _ := r.GetRevision(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := map[string]func() error{
		"Initialize":  func() error { return r.Initialize(ctx) },
		"GetRevision": func() error { _, err := r.GetRevision(ctx); return err },
		"Snapshot":    func() error { _, err := r.RuntimeSnapshot(ctx); return err },
		"Scan":        func() error { _, err := r.ScanChanges(ctx, 0, 10); return err },
		"Upsert":      func() error { _, err := r.UpsertAccounts(ctx, []Upsert{{Token: "tok-x-0123456789"}}); return err },
		"Patch": func() error {
			_, err := r.PatchAccounts(ctx, []Patch{{Token: "tok-ctx-0123456789", ClearFailures: true}})
			return err
		},
		"Delete":  func() error { _, err := r.DeleteAccounts(ctx, []string{"tok-ctx-0123456789"}); return err },
		"Get":     func() error { _, err := r.GetAccounts(ctx, []string{"tok-ctx-0123456789"}); return err },
		"List":    func() error { _, err := r.ListAccounts(ctx, ListQuery{}); return err },
		"Replace": func() error { _, err := r.ReplacePool(ctx, "basic", nil); return err },
		"Reset":   func() error { _, err := r.ResetExpiredConsoleWindows(ctx); return err },
		"Recover": func() error { _, err := r.RecoverConsoleExpiredAccounts(ctx); return err },
	}
	for name, call := range calls {
		if err := call(); err == nil {
			t.Errorf("%s ignored a cancelled context", name)
		}
	}
	if got, _ := r.GetRevision(context.Background()); got != rev {
		t.Fatalf("cancelled calls changed the revision: %d -> %d", rev, got)
	}
	if hasRecord(r, "tok-x-0123456789") {
		t.Fatal("cancelled upsert was applied")
	}
}

// A tombstone is kept while it is young, even after a consumer scanned it.
func TestTombstoneKeptUntilRetentionElapsed(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t) // default retention: 24h
	mustUpsert(t, r, "tok-young-0123456789", "basic")
	mustUpsert(t, r, "tok-other-0123456789", "basic")
	if _, err := r.DeleteAccounts(ctx, []string{"tok-young-0123456789"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := r.ScanChanges(ctx, 0, 100)
	_, _ = r.ScanChanges(ctx, cs.Revision, 100) // consumer acks everything
	mustUpsert(t, r, "tok-third-0123456789", "basic")

	if !hasRecord(r, "tok-young-0123456789") {
		t.Fatal("young tombstone purged")
	}
	if cs2, _ := r.ScanChanges(ctx, 0, 100); cs2.Stale {
		t.Fatal("fresh consumer reported stale")
	}
}

// An old tombstone nobody has scanned yet must survive: the consumer would
// otherwise never learn about the deletion.
func TestTombstoneNotPurgedBeforeAConsumerScannedIt(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	r.SetTombstoneRetention(time.Millisecond)
	mustUpsert(t, r, "tok-unseen-0123456789", "basic")
	if _, err := r.DeleteAccounts(ctx, []string{"tok-unseen-0123456789"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	mustUpsert(t, r, "tok-next-0123456789", "basic") // triggers a purge pass

	if !hasRecord(r, "tok-unseen-0123456789") {
		t.Fatal("tombstone purged before any consumer scanned past it")
	}
	cs, _ := r.ScanChanges(ctx, 0, 100)
	var sawDelete bool
	for _, tok := range cs.DeletedTokens {
		sawDelete = sawDelete || tok == "tok-unseen-0123456789"
	}
	if !sawDelete {
		t.Fatalf("deletion not visible in the feed: %+v", cs.DeletedTokens)
	}
}

// Old + scanned tombstones are purged from memory and from the file, and a
// consumer that missed them is told to resync.
func TestTombstonePurgeAndStaleSignal(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRepo(t)
	r.SetTombstoneRetention(time.Millisecond)
	mustUpsert(t, r, "tok-gone-0123456789", "basic")
	mustUpsert(t, r, "tok-stay-0123456789", "basic")
	cursorBeforeDelete, _ := r.GetRevision(ctx)

	if _, err := r.DeleteAccounts(ctx, []string{"tok-gone-0123456789"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := r.ScanChanges(ctx, cursorBeforeDelete, 100) // main consumer sees the deletion
	if len(cs.DeletedTokens) != 1 {
		t.Fatalf("deletion not delivered: %+v", cs)
	}
	_, _ = r.ScanChanges(ctx, cs.Revision, 100) // ...and moves past it
	time.Sleep(5 * time.Millisecond)
	mustUpsert(t, r, "tok-new-0123456789", "basic") // purge pass

	if hasRecord(r, "tok-gone-0123456789") {
		t.Fatal("scanned, old tombstone was not purged")
	}
	data, _ := os.ReadFile(path)
	if containsStr(string(data), "tok-gone-0123456789") {
		t.Fatal("purged tombstone still in the file")
	}

	// A consumer stuck before the deletion must be told it cannot trust the feed.
	lagging, _ := r.ScanChanges(ctx, cursorBeforeDelete, 100)
	if !lagging.Stale {
		t.Fatal("lagging consumer not flagged stale")
	}
	// Up-to-date and brand-new consumers are not.
	if cur, _ := r.ScanChanges(ctx, cs.Revision, 100); cur.Stale {
		t.Fatal("current consumer flagged stale")
	}
	if fresh, _ := r.ScanChanges(ctx, 0, 100); fresh.Stale {
		t.Fatal("fresh consumer flagged stale")
	}
	// Revisions never go backwards.
	if rev, _ := r.GetRevision(ctx); rev < cs.Revision {
		t.Fatalf("revision regressed: %d < %d", rev, cs.Revision)
	}
}

// Directory still observes deletions after the tombstone was purged: a
// lagging directory resynchronises from the snapshot.
func TestDirectoryObservesDeletionAcrossPurge(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	r.SetTombstoneRetention(time.Millisecond)
	mustUpsert(t, r, "tok-d1-0123456789", "basic")
	mustUpsert(t, r, "tok-d2-0123456789", "basic")

	main := NewDirectory(r)
	lagging := NewDirectory(r)
	for _, d := range []*Directory{main, lagging} {
		if err := d.Bootstrap(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.DeleteAccounts(ctx, []string{"tok-d1-0123456789"}); err != nil {
		t.Fatal(err)
	}
	// The main directory syncs promptly and observes the tombstone.
	if _, err := main.SyncIfChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if main.Size() != 1 {
		t.Fatalf("main directory size = %d, want 1", main.Size())
	}
	// The next poll presents the advanced cursor, which acknowledges the
	// tombstone to the repository.
	if _, err := main.SyncIfChanged(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	mustUpsert(t, r, "tok-d3-0123456789", "basic") // purges the tombstone
	if hasRecord(r, "tok-d1-0123456789") {
		t.Fatal("precondition: tombstone should be purged")
	}

	// The lagging directory never scanned: it must still drop the account.
	if _, err := lagging.SyncIfChanged(ctx); err != nil {
		t.Fatal(err)
	}
	if lagging.Size() != 2 { // d2 + d3
		t.Fatalf("lagging directory size = %d, want 2", lagging.Size())
	}
	lagging.mu.Lock()
	_, stillThere := lagging.slots["tok-d1-0123456789"]
	lagging.mu.Unlock()
	if stillThere {
		t.Fatal("deleted account survived in the lagging directory")
	}
	rev, _ := r.GetRevision(ctx)
	if lagging.Revision() != rev {
		t.Fatalf("lagging revision %d, repo %d", lagging.Revision(), rev)
	}
}

// Tombstones in a loaded file that are old are dropped at startup, but the
// revision counter is preserved.
func TestLoadPurgesOldTombstones(t *testing.T) {
	ctx := context.Background()
	r, path := newTestRepo(t)
	r.SetTombstoneRetention(-1) // keep: we want it in the file
	mustUpsert(t, r, "tok-old-0123456789", "basic")
	if _, err := r.DeleteAccounts(ctx, []string{"tok-old-0123456789"}); err != nil {
		t.Fatal(err)
	}
	rev, _ := r.GetRevision(ctx)

	r2 := NewTxtRepository(path)
	r2.SetTombstoneRetention(time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	if err := r2.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if hasRecord(r2, "tok-old-0123456789") {
		t.Fatal("old tombstone survived load")
	}
	if got, _ := r2.GetRevision(ctx); got != rev {
		t.Fatalf("revision %d, want %d", got, rev)
	}
	// With the default 24h retention the tombstone is kept.
	r3 := NewTxtRepository(path)
	if err := r3.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if !hasRecord(r3, "tok-old-0123456789") {
		t.Fatal("young tombstone dropped at load")
	}
}
