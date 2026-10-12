package account

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// DefaultScanLimit is the batch size ScanChanges uses when the caller passes
// a non-positive limit.
const DefaultScanLimit = 5000

// TxtRepository stores accounts in a local JSONL file (one JSON object per line).
// All data is kept in memory; writes flush the full file atomically.
//
// Mutations consume one revision each, but only when they change something: a
// patch that matches no live account neither bumps the revision nor rewrites
// the file.
//
// Deleted accounts stay in the change feed as tombstones (DeletedAt set) so
// consumers can observe the deletion. Tombstones are purged once they are
// older than the retention (DefaultTombstoneRetention) AND a consumer has
// scanned past them; a consumer whose cursor is older than a purged tombstone
// is told so via ChangeSet.Stale and must resynchronise from RuntimeSnapshot.
type TxtRepository struct {
	mu       sync.Mutex
	path     string
	accounts map[string]*Record // token → record
	revision int

	tombstoneTTL time.Duration // 0 = DefaultTombstoneRetention, < 0 = keep forever
	scannedRev   int           // highest cursor any ScanChanges caller presented
	purgedRev    int           // highest revision of a purged tombstone
}

// DefaultTombstoneRetention is how long a deleted account stays in the change
// feed before it may be purged.
const DefaultTombstoneRetention = 24 * time.Hour

// SetTombstoneRetention sets the minimum age before a scanned tombstone is
// purged. Zero restores DefaultTombstoneRetention, a negative value keeps
// tombstones forever.
func (r *TxtRepository) SetTombstoneRetention(d time.Duration) {
	r.mu.Lock()
	r.tombstoneTTL = d
	r.mu.Unlock()
}

// NewTxtRepository opens (or creates) a JSONL file at path.
func NewTxtRepository(path string) *TxtRepository {
	return &TxtRepository{
		path:     path,
		accounts: make(map[string]*Record),
	}
}

func (r *TxtRepository) Initialize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Load existing file if present.
	if _, err := os.Stat(r.path); os.IsNotExist(err) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.flush()
	}
	return r.load()
}

func (r *TxtRepository) Close(ctx context.Context) error {
	return nil
}

func (r *TxtRepository) GetRevision(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision, nil
}

func (r *TxtRepository) RuntimeSnapshot(ctx context.Context) (*Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]*Record, 0, len(r.accounts))
	for _, rec := range r.accounts {
		if !rec.IsDeleted() {
			items = append(items, rec.Clone())
		}
	}
	return &Snapshot{Revision: r.revision, Items: items}, nil
}

// ScanChanges returns the records modified after revision `since`, ordered by
// (revision, token) and at most `limit` long. It is a pull cursor:
//
//   - A batch never splits a group of records that share one revision (batch
//     mutations stamp all touched records with the same revision), so the limit
//     is a soft target. If a single revision group alone exceeds the limit the
//     whole group is returned.
//   - When HasMore is true, Revision is the highest revision contained in the
//     batch; pass it as `since` to continue. Records beyond the batch are
//     never skipped.
//   - When HasMore is false, Revision is the repository's current revision.
//   - Deleted records are returned as tombstones (DeletedAt set) and listed in
//     DeletedTokens.
//   - Presenting `since` tells the repository that the caller has seen
//     everything up to it; tombstones at or below the highest such cursor may
//     be purged once they are old enough. If a tombstone newer than `since`
//     was already purged the result has Stale set and the caller must
//     resynchronise (RuntimeSnapshot) to learn about deletions.
func (r *TxtRepository) ScanChanges(ctx context.Context, since int, limit int) (*ChangeSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = DefaultScanLimit
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if since > r.scannedRev {
		r.scannedRev = since
	}

	var changed []*Record
	for _, rec := range r.accounts {
		if rec.Revision > since {
			changed = append(changed, rec)
		}
	}
	sort.Slice(changed, func(i, j int) bool {
		if changed[i].Revision != changed[j].Revision {
			return changed[i].Revision < changed[j].Revision
		}
		return changed[i].Token < changed[j].Token
	})
	batch, hasMore := cutAtRevisionBoundary(changed, limit)

	// Hand out deep copies: callers read them without holding r.mu.
	items := make([]*Record, len(batch))
	var deleted []string
	batchMax := since
	for i, rec := range batch {
		items[i] = rec.Clone()
		if rec.IsDeleted() {
			deleted = append(deleted, rec.Token)
		}
		if rec.Revision > batchMax {
			batchMax = rec.Revision
		}
	}
	cs := &ChangeSet{Items: items, DeletedTokens: deleted, HasMore: hasMore, BatchMaxRev: batchMax}
	cs.Stale = since > 0 && since < r.purgedRev
	if hasMore {
		cs.Revision = batchMax
	} else {
		cs.Revision = r.revision
	}
	return cs, nil
}

// cutAtRevisionBoundary truncates a (revision, token)-sorted slice to about
// `limit` records without splitting a revision group.
func cutAtRevisionBoundary(sorted []*Record, limit int) ([]*Record, bool) {
	if len(sorted) <= limit {
		return sorted, false
	}
	cutRev := sorted[limit-1].Revision
	if sorted[limit].Revision != cutRev {
		return sorted[:limit], true
	}
	// The group straddles the limit: back off to the previous boundary...
	start := limit - 1
	for start > 0 && sorted[start-1].Revision == cutRev {
		start--
	}
	if start > 0 {
		return sorted[:start], true
	}
	// ...unless the very first group is already larger than the limit.
	end := limit
	for end < len(sorted) && sorted[end].Revision == cutRev {
		end++
	}
	return sorted[:end], end < len(sorted)
}

// --- mutations ---

// finishLocked commits a mutation: when something changed it consumes rev and
// flushes; otherwise the repository is left untouched. res.Revision is set to
// the repository's revision afterwards.
func (r *TxtRepository) finishLocked(rev int, changed bool, res MutationResult) (*MutationResult, error) {
	if changed {
		r.revision = rev
		r.purgeTombstonesLocked(platform.NowMs(), r.scannedRev)
		if err := r.flush(); err != nil {
			return nil, err
		}
	}
	res.Revision = r.revision
	return &res, nil
}

// liveLocked returns the stored, non-deleted record for a raw token or nil.
func (r *TxtRepository) liveLocked(rawToken string) *Record {
	tok := platform.SanitizeToken(rawToken)
	if tok == "" {
		return nil
	}
	rec, ok := r.accounts[tok]
	if !ok || rec.IsDeleted() {
		return nil
	}
	return rec
}

// putNewLocked stores a fresh ACTIVE record with default quotas, keeping the
// CreatedAt of an existing record for the same token.
func (r *TxtRepository) putNewLocked(tok, pool string, it Upsert, now int64, rev int) {
	ext := cloneAnyMap(it.Ext)
	if ext == nil {
		ext = map[string]any{}
	}
	qs := DefaultQuotaSet(pool)
	rec := &Record{
		Token:     tok,
		Pool:      pool,
		Status:    StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
		Tags:      SortTags(it.Tags),
		Quota:     qs.ToMap(),
		Ext:       ext,
		Revision:  rev,
	}
	if old, ok := r.accounts[tok]; ok && old != nil {
		rec.CreatedAt = old.CreatedAt
	}
	r.accounts[tok] = rec
}

func (r *TxtRepository) UpsertAccounts(ctx context.Context, items []Upsert) (*MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rev := r.revision + 1
	now := platform.NowMs()
	upserted := 0
	for _, it := range items {
		tok := platform.SanitizeToken(it.Token)
		if tok == "" {
			continue
		}
		r.putNewLocked(tok, normalizePoolName(it.Pool), it, now, rev)
		upserted++
	}
	return r.finishLocked(rev, upserted > 0, MutationResult{Upserted: upserted})
}

func (r *TxtRepository) PatchAccounts(ctx context.Context, patches []Patch) (*MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rev := r.revision + 1
	now := platform.NowMs()
	patched := 0
	for i := range patches {
		rec := r.liveLocked(patches[i].Token)
		if rec == nil {
			continue
		}
		applyPatch(rec, &patches[i])
		rec.UpdatedAt = now
		rec.Revision = rev
		patched++
	}
	return r.finishLocked(rev, patched > 0, MutationResult{Patched: patched})
}

func (r *TxtRepository) DeleteAccounts(ctx context.Context, tokens []string) (*MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rev := r.revision + 1
	now := platform.NowMs()
	deleted := 0
	for _, raw := range tokens {
		rec := r.liveLocked(raw)
		if rec == nil {
			continue
		}
		rec.DeletedAt = &now
		rec.UpdatedAt = now
		rec.Revision = rev
		deleted++
	}
	return r.finishLocked(rev, deleted > 0, MutationResult{Deleted: deleted})
}

func (r *TxtRepository) ReplacePool(ctx context.Context, pool string, upserts []Upsert) (*MutationResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id, ok := PoolFromName(pool); ok {
		pool = id.Name()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rev := r.revision + 1
	now := platform.NowMs()
	// Soft-delete all existing records in this pool.
	removed := 0
	for _, rec := range r.accounts {
		if rec.Pool == pool && !rec.IsDeleted() {
			rec.DeletedAt = &now
			rec.UpdatedAt = now
			rec.Revision = rev
			removed++
		}
	}
	upserted := 0
	for _, it := range upserts {
		tok := platform.SanitizeToken(it.Token)
		if tok == "" {
			continue
		}
		r.putNewLocked(tok, pool, it, now, rev)
		upserted++
	}
	return r.finishLocked(rev, removed+upserted > 0, MutationResult{Upserted: upserted})
}

// applyPatch applies the set fields of p to rec.
func applyPatch(rec *Record, p *Patch) {
	applyIdentityPatch(rec, p)
	applyQuotaPatch(rec, p)
	applyUsagePatch(rec, p)
	applyStatePatch(rec, p)
}

// applyIdentityPatch handles pool, status and tags.
func applyIdentityPatch(rec *Record, p *Patch) {
	if p.Pool != nil {
		// An unknown pool name would orphan the account; ignore it.
		if id, ok := PoolFromName(*p.Pool); ok {
			rec.Pool = id.Name()
		}
	}
	if p.Status != nil {
		rec.Status = *p.Status
	}
	if p.Tags != nil {
		rec.Tags = SortTags(p.Tags)
	}
	if len(p.AddTags) > 0 || len(p.RemoveTags) > 0 {
		rec.Tags = MergeTags(rec.Tags, p.AddTags, p.RemoveTags)
	}
}

// applyQuotaPatch replaces the quota windows set on the patch.
func applyQuotaPatch(rec *Record, p *Patch) {
	for modeID := ModeAuto; modeID <= ModeConsole; modeID++ {
		m := p.quota(modeID)
		if m == nil {
			continue
		}
		if rec.Quota == nil {
			rec.Quota = map[string]any{}
		}
		rec.Quota[QuotaKey(modeID)] = cloneAnyMap(*m)
	}
}

// applyUsagePatch applies counter deltas (floored at zero) and timestamps.
func applyUsagePatch(rec *Record, p *Patch) {
	addClamped := func(cur int, delta *int) int {
		if delta == nil {
			return cur
		}
		if cur += *delta; cur < 0 {
			return 0
		}
		return cur
	}
	rec.UsageUseCount = addClamped(rec.UsageUseCount, p.UsageUseDelta)
	rec.UsageFailCount = addClamped(rec.UsageFailCount, p.UsageFailDelta)
	rec.UsageSyncCount = addClamped(rec.UsageSyncCount, p.UsageSyncDelta)
	if p.LastUseAt != nil {
		rec.LastUseAt = p.LastUseAt
	}
	if p.LastFailAt != nil {
		rec.LastFailAt = p.LastFailAt
	}
	if p.LastFailReason != nil {
		rec.LastFailReason = p.LastFailReason
	}
	if p.LastSyncAt != nil {
		rec.LastSyncAt = p.LastSyncAt
	}
	if p.LastClearAt != nil {
		rec.LastClearAt = p.LastClearAt
	}
}

// applyStatePatch applies StateReason, ClearFailures and ExtMerge, in that
// order: ClearFailures resets the failure state first, then ExtMerge records
// new keys on top.
func applyStatePatch(rec *Record, p *Patch) {
	if rec.Ext == nil {
		rec.Ext = map[string]any{}
	}
	if p.StateReason != nil {
		rec.StateReason = p.StateReason
	}
	if p.ClearFailures {
		rec.UsageFailCount = 0
		rec.LastFailAt = nil
		rec.LastFailReason = nil
		rec.StateReason = nil
		rec.Status = StatusActive
		deleteExtKeys(rec.Ext, failureExtKeys[:]...)
	}
	for k, v := range p.ExtMerge {
		rec.Ext[k] = cloneAny(v)
	}
}

// --- reads ---

func (r *TxtRepository) GetAccounts(ctx context.Context, tokens []string) ([]*Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Record, 0, len(tokens))
	for _, raw := range tokens {
		tok := platform.SanitizeToken(raw)
		if rec, ok := r.accounts[tok]; ok {
			out = append(out, rec.Clone())
		}
	}
	return out, nil
}

// matchesQuery reports whether rec passes the filters of q.
func matchesQuery(rec *Record, q *ListQuery) bool {
	if !q.IncludeDeleted && rec.IsDeleted() {
		return false
	}
	if q.Pool != "" && rec.Pool != q.Pool {
		return false
	}
	if q.Status != nil && rec.Status != *q.Status {
		return false
	}
	for _, want := range q.Tags {
		found := false
		for _, have := range rec.Tags {
			if have == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ListAccounts filters, sorts (see sortRecords) and paginates the accounts.
// PageSize defaults to 50 and is clamped to MaxPageSize (Page.Clamped).
func (r *TxtRepository) ListAccounts(ctx context.Context, q ListQuery) (*Page, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if q.Page < 1 {
		q.Page = 1
	}
	if q.PageSize < 1 {
		q.PageSize = 50
	}
	clamped := q.PageSize > MaxPageSize
	if clamped {
		q.PageSize = MaxPageSize
	}

	items := make([]*Record, 0, len(r.accounts))
	for _, rec := range r.accounts {
		if matchesQuery(rec, &q) {
			items = append(items, rec)
		}
	}
	sortRecords(items, q.SortBy, q.SortDesc)

	total := len(items)
	totalPages := 1
	if total > 0 {
		totalPages = (total + q.PageSize - 1) / q.PageSize
	}
	offset := (q.Page - 1) * q.PageSize
	if offset > total {
		offset = total
	}
	end := offset + q.PageSize
	if end > total {
		end = total
	}
	// Deep copy records so callers can neither mutate our state nor race
	// with later mutations while reading Quota/Ext/Tags.
	out := make([]*Record, end-offset)
	for i, rec := range items[offset:end] {
		out[i] = rec.Clone()
	}
	return &Page{
		Items: out, Total: total, Page: q.Page, PageSize: q.PageSize,
		TotalPages: totalPages, Revision: r.revision, Clamped: clamped,
	}, nil
}

// sortRecords orders records by the requested key. Ties are broken by token
// (always ascending) so the order is total and paginated listings are stable.
func sortRecords(items []*Record, sortBy string, desc bool) {
	key := func(rec *Record) int64 {
		switch sortBy {
		case "created_at":
			return rec.CreatedAt
		case "last_use_at":
			return ptrVal(rec.LastUseAt)
		case "usage_use_count":
			return int64(rec.UsageUseCount)
		case "usage_fail_count":
			return int64(rec.UsageFailCount)
		}
		return rec.UpdatedAt
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if sortBy == "token" {
			if desc {
				return a.Token > b.Token
			}
			return a.Token < b.Token
		}
		ka, kb := key(a), key(b)
		if ka != kb {
			if desc {
				return ka > kb
			}
			return ka < kb
		}
		return a.Token < b.Token
	})
}

// --- console maintenance (ConsoleMaintainer) ---

// ResetExpiredConsoleWindows restores the default console window of active
// accounts whose window carries a reset time that has passed.
func (r *TxtRepository) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Only consume a revision when something actually changes.
	rev := r.revision + 1
	now := platform.NowMs()
	def := DefaultQuotaWindow("basic", ModeConsole)
	reset := 0
	for _, rec := range r.accounts {
		if rec.IsDeleted() || rec.Status != StatusActive {
			continue
		}
		qs := rec.QuotaSet()
		w := qs.Console
		if w == nil {
			continue
		}
		// A window is "expired" only when it carries a reset time that has
		// passed. Default windows (ResetAt == nil) and exhausted windows whose
		// reset time is still in the future must be left alone; otherwise every
		// default account is reset and the whole file re-flushed on every tick.
		if w.ResetAt != nil && *w.ResetAt <= now {
			rec.Quota[QuotaKey(ModeConsole)] = def.ToMap()
			rec.LastSyncAt = &now
			rec.UpdatedAt = now
			rec.Revision = rev
			reset++
		}
	}
	if _, err := r.finishLocked(rev, reset > 0, MutationResult{}); err != nil {
		return 0, err
	}
	return reset, nil
}

// recoverAfterMs is how long an account stays expired for console 429s before
// RecoverConsoleExpiredAccounts re-activates it.
const recoverAfterMs = 3600_000

// RecoverConsoleExpiredAccounts re-activates accounts that were expired for
// exceeding the console 429 threshold more than an hour ago.
func (r *TxtRepository) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rev := r.revision + 1 // consumed only if something is recovered
	now := platform.NowMs()
	threshold := now - recoverAfterMs
	recovered := 0
	for _, rec := range r.accounts {
		if rec.IsDeleted() || rec.Status != StatusExpired {
			continue
		}
		if rec.StateReason == nil || *rec.StateReason != ReasonConsole429 {
			continue
		}
		if rec.UsageUseCount <= 5 {
			continue
		}
		// Ext numbers are int64 in-process and float64 after a JSON reload.
		if toInt64(rec.Ext[ExtExpiredAt]) > threshold {
			continue
		}
		rec.Status = StatusActive
		rec.StateReason = nil
		deleteExtKeys(rec.Ext, consoleRecoveryExtKeys[:]...)
		rec.UpdatedAt = now
		rec.Revision = rev
		recovered++
	}
	if _, err := r.finishLocked(rev, recovered > 0, MutationResult{}); err != nil {
		return 0, err
	}
	return recovered, nil
}

// purgeTombstonesLocked drops tombstones that are older than the retention and
// at or below ackedRev (the highest revision a consumer has scanned past). It
// records the highest purged revision so lagging consumers can be told to
// resynchronise. Returns the number of purged tombstones.
func (r *TxtRepository) purgeTombstonesLocked(nowMs int64, ackedRev int) int {
	if r.tombstoneTTL < 0 {
		return 0
	}
	ttl := r.tombstoneTTL
	if ttl == 0 {
		ttl = DefaultTombstoneRetention
	}
	cutoff := nowMs - ttl.Milliseconds()
	purged := 0
	for tok, rec := range r.accounts {
		if rec.DeletedAt == nil || rec.Revision > ackedRev || *rec.DeletedAt > cutoff {
			continue
		}
		delete(r.accounts, tok)
		if rec.Revision > r.purgedRev {
			r.purgedRev = rec.Revision
		}
		purged++
	}
	return purged
}

// --- persistence ---

// load reads the JSONL file into the in-memory map.
func (r *TxtRepository) load() error {
	f, err := os.Open(r.path)
	if err != nil {
		return err
	}
	defer f.Close()

	recs, bad, err := readRecords(f)
	if err != nil {
		return err
	}

	r.mu.Lock()
	maxRev := 0
	for _, rec := range recs {
		r.accounts[rec.Token] = rec
		if rec.Revision > maxRev {
			maxRev = rec.Revision
		}
	}
	r.revision = maxRev
	// Nobody has scanned yet and every consumer starts from RuntimeSnapshot
	// (which has no deleted records), so old tombstones are not needed.
	r.purgeTombstonesLocked(platform.NowMs(), maxRev)
	r.mu.Unlock()

	if bad.count > 0 {
		// Skipped lines are dropped by the next flush, so keep a copy.
		backup, berr := backupCorruptFile(r.path)
		if berr != nil {
			logger.Warnf("account storage: skipped malformed lines but backup failed: path=%s skipped=%d first_line=%d error=%v",
				r.path, bad.count, bad.firstLine, berr)
		} else {
			logger.Warnf("account storage: skipped malformed lines: path=%s skipped=%d first_line=%d backup=%s",
				r.path, bad.count, bad.firstLine, backup)
		}
	}
	return nil
}

type badLines struct{ count, firstLine int }

// readRecords parses a JSONL stream, skipping (and counting) malformed lines.
func readRecords(rd io.Reader) ([]*Record, badLines, error) {
	var (
		recs []*Record
		bad  badLines
	)
	scanner := bufio.NewScanner(rd)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			bad.count++
			if bad.firstLine == 0 {
				bad.firstLine = lineNo
			}
			continue
		}
		if rec.Tags == nil {
			rec.Tags = []string{}
		}
		if rec.Ext == nil {
			rec.Ext = map[string]any{}
		}
		if rec.Quota == nil {
			rec.Quota = map[string]any{}
		}
		recs = append(recs, &rec)
	}
	return recs, bad, scanner.Err()
}

// backupCorruptFile copies path to path+".corrupt" (or a timestamped name when
// that already exists, so an earlier backup is never overwritten).
func backupCorruptFile(path string) (string, error) {
	dst := path + ".corrupt"
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.corrupt.%d", path, time.Now().Unix())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := platform.AtomicWriteBytes(dst, 0o600, data); err != nil {
		return "", err
	}
	return dst, nil
}

// flush writes the full in-memory state to the JSONL file atomically (see
// platform.AtomicWriteFile). Records are written sorted by token so the file
// content is deterministic and diff-friendly. Caller holds r.mu.
func (r *TxtRepository) flush() error {
	tokens := make([]string, 0, len(r.accounts))
	for tok := range r.accounts {
		tokens = append(tokens, tok)
	}
	sort.Strings(tokens)
	return platform.AtomicWriteFile(r.path, 0o600, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, tok := range tokens {
			if err := enc.Encode(r.accounts[tok]); err != nil {
				return err
			}
		}
		return nil
	})
}

// normalizePoolName returns the canonical lower-case pool name, falling back
// to "basic" for an empty or unknown name.
func normalizePoolName(name string) string {
	if id, ok := PoolFromName(name); ok {
		return id.Name()
	}
	return PoolBasic.Name()
}
