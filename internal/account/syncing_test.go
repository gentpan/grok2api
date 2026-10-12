package account

import (
	"context"
	"testing"
)

func reservable(d *Directory, pool, mode int) bool {
	l, _ := d.Reserve([]int{pool}, mode, nil, nil)
	if l != nil {
		d.Release(l)
	}
	return l != nil
}

func TestApplyChangesReturnsAppliedCount(t *testing.T) {
	ctx := context.Background()
	d, r := newDirWith(t, "basic")
	if n, err := d.ApplyChanges(ctx); err != nil || n != 0 {
		t.Fatalf("empty ApplyChanges = %d,%v", n, err)
	}
	seedN(t, r, 3, "basic")
	if n, err := d.ApplyChanges(ctx); err != nil || n != 3 {
		t.Fatalf("ApplyChanges = %d,%v want 3", n, err)
	}
	if d.Size() != 3 {
		t.Fatalf("size = %d", d.Size())
	}
	if n, _ := d.ApplyChanges(ctx); n != 0 {
		t.Fatalf("repeat ApplyChanges = %d", n)
	}
}

// Admin add / disable / enable / delete are visible without a poll tick.
func TestSyncingRepositoryReflectsAdminEditsImmediately(t *testing.T) {
	ctx := context.Background()
	d, raw := newDirWith(t, "basic")
	repo := NewSyncingRepository(raw, d)
	var _ Repository = repo // must be usable wherever a Repository is
	const tok = "tok-admin-0123456789"

	if reservable(d, int(PoolBasic), ModeFast) {
		t.Fatal("empty directory reservable")
	}
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: tok, Pool: "basic"}}); err != nil {
		t.Fatal(err)
	}
	if !reservable(d, int(PoolBasic), ModeFast) {
		t.Fatal("added account not reservable immediately")
	}

	disabled := StatusDisabled
	if _, err := repo.PatchAccounts(ctx, []Patch{{Token: tok, Status: &disabled}}); err != nil {
		t.Fatal(err)
	}
	if reservable(d, int(PoolBasic), ModeFast) {
		t.Fatal("disabled account still reservable")
	}

	if _, err := repo.PatchAccounts(ctx, []Patch{{Token: tok, ClearFailures: true}}); err != nil {
		t.Fatal(err)
	}
	if !reservable(d, int(PoolBasic), ModeFast) {
		t.Fatal("re-enabled account not reservable")
	}

	if _, err := repo.DeleteAccounts(ctx, []string{tok}); err != nil {
		t.Fatal(err)
	}
	if reservable(d, int(PoolBasic), ModeFast) || d.Size() != 0 {
		t.Fatalf("deleted account still present (size=%d)", d.Size())
	}

	// ReplacePool: replace and then empty the pool.
	if _, err := repo.ReplacePool(ctx, "basic", []Upsert{{Token: "tok-r1-0123456789"}, {Token: "tok-r2-0123456789"}}); err != nil {
		t.Fatal(err)
	}
	if d.Size() != 2 {
		t.Fatalf("size after replace = %d", d.Size())
	}
	if _, err := repo.ReplacePool(ctx, "basic", nil); err != nil {
		t.Fatal(err)
	}
	if d.Size() != 0 {
		t.Fatalf("size after emptying replace = %d", d.Size())
	}

	// The directory revision tracks the repository.
	rev, _ := raw.GetRevision(ctx)
	if d.Revision() != rev {
		t.Fatalf("directory revision %d, repo %d", d.Revision(), rev)
	}
}

func TestSyncingRepositoryNilDirectoryIsPassThrough(t *testing.T) {
	ctx := context.Background()
	r, _ := newTestRepo(t)
	repo := NewSyncingRepository(r, nil)
	if _, err := repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-pt-0123456789"}}); err != nil {
		t.Fatal(err)
	}
	if recs, _ := repo.GetAccounts(ctx, []string{"tok-pt-0123456789"}); len(recs) != 1 {
		t.Fatal("pass-through lost the record")
	}
}

// The directory keeps working when Directory and decorated repo race.
func TestApplyChangesConcurrent(t *testing.T) {
	ctx := context.Background()
	d, raw := newDirWith(t, "basic")
	repo := NewSyncingRepository(raw, d)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			_, _ = repo.UpsertAccounts(ctx, []Upsert{{Token: "tok-cc-" + string(rune('a'+i%26)) + "-0123456789"}})
		}
	}()
	for i := 0; i < 30; i++ {
		_, _ = d.SyncIfChanged(ctx)
	}
	<-done
	if _, err := d.ApplyChanges(ctx); err != nil {
		t.Fatal(err)
	}
	rev, _ := raw.GetRevision(ctx)
	if d.Revision() != rev || d.Size() != 26 {
		t.Fatalf("revision %d/%d size %d", d.Revision(), rev, d.Size())
	}
}
