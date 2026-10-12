package account

import (
	"context"
	"errors"
	"sync"

	"github.com/gentpan/grok2api/internal/platform"
)

// slot is the in-memory runtime view of an account used for fast selection.
type slot struct {
	Token        string
	PoolID       PoolID
	StatusID     StatusID
	Quota        QuotaSet
	Inflight     int
	FailCount    int
	Health       float64
	LastUseAt    int64
	LastFailAt   int64
	CoolingUntil int64 // seconds (random strategy)
	Tags         []string
}

// Lease is a reserved account slot.
type Lease struct {
	ID         int64
	Token      string
	PoolID     PoolID
	ModeID     int
	SelectedAt int64
}

// Strategy is the active selection strategy.
type Strategy string

const (
	StrategyRandom Strategy = "random"
	StrategyQuota  Strategy = "quota"
)

// Directory is the in-memory account runtime table with selection/release/feedback.
type Directory struct {
	mu          sync.Mutex
	slots       map[string]*slot
	byMode      map[modeKey]map[string]struct{}
	revision    int
	repo        Repository
	strategy    Strategy
	leaseSeq    int64
	maxInflight int

	// syncMu serialises SyncIfChanged so two concurrent syncs cannot apply
	// scans out of order. Lock order: syncMu -> mu.
	syncMu    sync.Mutex
	syncBatch int // ScanChanges batch size; 0 = DefaultScanLimit
}

type modeKey struct {
	pool   PoolID
	modeID int
}

// NewDirectory creates a directory backed by the given repository.
func NewDirectory(repo Repository) *Directory {
	return &Directory{
		slots:       map[string]*slot{},
		byMode:      map[modeKey]map[string]struct{}{},
		repo:        repo,
		strategy:    StrategyQuota,
		maxInflight: DefaultMaxInflight,
	}
}

// SetStrategy switches the active selection strategy.
func (d *Directory) SetStrategy(s Strategy) {
	d.mu.Lock()
	d.strategy = s
	d.mu.Unlock()
}

// Strategy returns the active strategy.
func (d *Directory) Strategy() Strategy {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.strategy
}

// DefaultMaxInflight is the per-account concurrent request cap used when none
// is configured (config key account.selection.max_inflight).
const DefaultMaxInflight = 8

// SetMaxInflight sets the per-account concurrent request cap applied by both
// selection strategies. A non-positive value restores DefaultMaxInflight.
func (d *Directory) SetMaxInflight(n int) {
	if n <= 0 {
		n = DefaultMaxInflight
	}
	d.mu.Lock()
	d.maxInflight = n
	d.mu.Unlock()
}

// Bootstrap loads the full snapshot from the repository.
func (d *Directory) Bootstrap(ctx context.Context) error {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	snap, err := d.repo.RuntimeSnapshot(ctx)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.slots = map[string]*slot{}
	d.byMode = map[modeKey]map[string]struct{}{}
	for _, rec := range snap.Items {
		d.upsertLocked(rec)
	}
	d.revision = snap.Revision
	return nil
}

// SyncIfChanged pulls incremental changes since the last known revision and
// keeps pulling while the repository reports more. It returns true when at
// least one record was applied.
func (d *Directory) SyncIfChanged(ctx context.Context) (bool, error) {
	n, err := d.ApplyChanges(ctx)
	return n > 0, err
}

// ApplyChanges pulls every pending repository change into the directory right
// now and returns how many records (live or tombstone) were applied. It is
// what SyncIfChanged does on each poll tick; call it after a repository
// mutation (admin add/delete/disable) so the change is selectable without
// waiting for the next poll. Safe to call concurrently.
func (d *Directory) ApplyChanges(ctx context.Context) (int, error) {
	d.syncMu.Lock()
	defer d.syncMu.Unlock()
	batch := d.syncBatch
	if batch <= 0 {
		batch = DefaultScanLimit
	}
	applied := 0
	resynced := false
	for {
		d.mu.Lock()
		since := d.revision
		d.mu.Unlock()
		cs, err := d.repo.ScanChanges(ctx, since, batch)
		if err != nil {
			return applied, err
		}
		if cs.Stale {
			// Deletions at or below our cursor were purged from the feed:
			// reconcile against the full snapshot instead.
			if resynced {
				return applied, errStaleCursor
			}
			resynced = true
			n, err := d.resync(ctx, since)
			applied += n
			if err != nil {
				return applied, err
			}
			continue
		}
		d.mu.Lock()
		for _, rec := range cs.Items {
			d.applyRecordLocked(rec)
			applied++
		}
		if cs.Revision > d.revision {
			d.revision = cs.Revision
		}
		d.mu.Unlock()
		if !cs.HasMore || cs.Revision <= since {
			return applied, nil
		}
	}
}

var errStaleCursor = errors.New("account: change feed cursor still stale after resync")

// resync reconciles the directory with the repository's full snapshot: slots
// whose account no longer exists are removed, accounts that are new or changed
// since revision `since` are (re)built. Unchanged slots keep their in-memory
// runtime state.
func (d *Directory) resync(ctx context.Context, since int) (int, error) {
	snap, err := d.repo.RuntimeSnapshot(ctx)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	applied := 0
	live := make(map[string]struct{}, len(snap.Items))
	for _, rec := range snap.Items {
		live[rec.Token] = struct{}{}
		if _, have := d.slots[rec.Token]; !have || rec.Revision > since {
			d.upsertLocked(rec)
			applied++
		}
	}
	for tok, s := range d.slots {
		if _, ok := live[tok]; !ok {
			d.removeFromIndexesLocked(s)
			delete(d.slots, tok)
			applied++
		}
	}
	d.revision = snap.Revision
	return applied, nil
}

// applyRecordLocked applies one change-feed record (live record or tombstone).
func (d *Directory) applyRecordLocked(rec *Record) {
	if rec.IsDeleted() {
		if s, ok := d.slots[rec.Token]; ok {
			d.removeFromIndexesLocked(s)
			delete(d.slots, rec.Token)
		}
		return
	}
	d.upsertLocked(rec)
}

func (d *Directory) upsertLocked(rec *Record) {
	nowMs := platform.NowMs()
	pool, _ := PoolFromName(rec.Pool)
	qs := NormalizeQuotaSet(rec.Pool, rec.QuotaSet())
	s := &slot{
		Token:      rec.Token,
		PoolID:     pool,
		StatusID:   rec.DeriveStatus(nowMs).ID(),
		Quota:      qs,
		Health:     1.0,
		Inflight:   0,
		FailCount:  rec.UsageFailCount,
		LastUseAt:  ptrVal(rec.LastUseAt),
		LastFailAt: ptrVal(rec.LastFailAt),
		Tags:       rec.Tags,
	}
	coolingUntil := toInt64(rec.Ext[ExtCooldownUntil])
	s.CoolingUntil = coolingUntil / 1000
	if old, ok := d.slots[rec.Token]; ok {
		s.Inflight = old.Inflight
		s.Health = old.Health
	}
	d.slots[rec.Token] = s
	d.reindexLocked(s)
}

func (d *Directory) reindexLocked(s *slot) {
	// Remove from all current mode buckets.
	for k := range d.byMode {
		delete(d.byMode[k], s.Token)
	}
	if s.StatusID != StatusIDActive {
		return
	}
	for _, modeID := range SupportedModeIDs(s.PoolID.Name()) {
		w := s.Quota.Get(modeID)
		if w == nil || w.WindowSeconds <= 0 {
			continue
		}
		k := modeKey{s.PoolID, modeID}
		if d.byMode[k] == nil {
			d.byMode[k] = map[string]struct{}{}
		}
		d.byMode[k][s.Token] = struct{}{}
	}
}

func (d *Directory) removeFromIndexesLocked(s *slot) {
	for k := range d.byMode {
		delete(d.byMode[k], s.Token)
	}
}

// Size returns the number of accounts in the directory (every non-deleted
// account, whatever its status).
func (d *Directory) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.slots)
}

// HasPoolMode reports whether at least one active account with a window for
// the mode is registered for the pool (a bucket may still be exhausted).
func (d *Directory) HasPoolMode(poolID, modeID int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	bucket := d.byMode[modeKey{PoolID(poolID), modeID}]
	return len(bucket) > 0
}

// Revision returns the last synced repository revision.
func (d *Directory) Revision() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.revision
}

func ptrVal(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
