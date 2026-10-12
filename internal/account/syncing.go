package account

import (
	"context"

	"github.com/gentpan/grok2api/internal/logger"
)

// SyncingRepository decorates a Repository so that every mutation is applied
// to the Directory immediately (Directory.ApplyChanges) instead of at the next
// poll tick. It satisfies Repository, so it can be handed to the API layer in
// place of the raw repository:
//
//	repo := account.NewTxtRepository(path)
//	dir := account.NewDirectory(repo)          // the directory reads the raw repo
//	api.NewServer(account.NewSyncingRepository(repo, dir), dir, ...)
//
// Reads are passed through untouched.
type SyncingRepository struct {
	Repository
	dir *Directory
}

// NewSyncingRepository wraps repo; a nil dir makes it a plain pass-through.
func NewSyncingRepository(repo Repository, dir *Directory) *SyncingRepository {
	return &SyncingRepository{Repository: repo, dir: dir}
}

func (r *SyncingRepository) sync(ctx context.Context) {
	if r.dir == nil {
		return
	}
	// The mutation already happened; applying it must not depend on the
	// request still being alive.
	if _, err := r.dir.ApplyChanges(context.WithoutCancel(ctx)); err != nil {
		logger.Warnf("account directory sync after mutation failed: error=%v", err)
	}
}

// UpsertAccounts implements Repository.
func (r *SyncingRepository) UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error) {
	res, err := r.Repository.UpsertAccounts(ctx, items)
	if err != nil || res.Upserted > 0 {
		r.sync(ctx)
	}
	return res, err
}

// PatchAccounts implements Repository.
func (r *SyncingRepository) PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error) {
	res, err := r.Repository.PatchAccounts(ctx, patches)
	if err != nil || res.Patched > 0 {
		r.sync(ctx)
	}
	return res, err
}

// DeleteAccounts implements Repository.
func (r *SyncingRepository) DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error) {
	res, err := r.Repository.DeleteAccounts(ctx, tokens)
	if err != nil || res.Deleted > 0 {
		r.sync(ctx)
	}
	return res, err
}

// ReplacePool implements Repository.
func (r *SyncingRepository) ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error) {
	res, err := r.Repository.ReplacePool(ctx, pool, upserts)
	// A replace that emptied the pool reports Upserted == 0; the revision
	// tells whether anything changed at all.
	if err != nil || res.Upserted > 0 || r.dirBehind(res.Revision) {
		r.sync(ctx)
	}
	return res, err
}

// ResetExpiredConsoleWindows implements ConsoleMaintainer (a no-op when the
// wrapped repository has no such capability).
func (r *SyncingRepository) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	m, ok := r.Repository.(ConsoleMaintainer)
	if !ok {
		return 0, nil
	}
	n, err := m.ResetExpiredConsoleWindows(ctx)
	if err != nil || n > 0 {
		r.sync(ctx)
	}
	return n, err
}

// RecoverConsoleExpiredAccounts implements ConsoleMaintainer.
func (r *SyncingRepository) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	m, ok := r.Repository.(ConsoleMaintainer)
	if !ok {
		return 0, nil
	}
	n, err := m.RecoverConsoleExpiredAccounts(ctx)
	if err != nil || n > 0 {
		r.sync(ctx)
	}
	return n, err
}

// dirBehind reports whether the directory has not yet seen revision rev.
func (r *SyncingRepository) dirBehind(rev int) bool {
	return r.dir != nil && r.dir.Revision() < rev
}
