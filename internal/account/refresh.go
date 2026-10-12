package account

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
)

// ErrInvalidCredentials matches (errors.Is) any error caused by the upstream
// rejecting a token as invalid, expired or blocked.
var ErrInvalidCredentials = errors.New("account: invalid credentials")

// ErrNoQuotaData is returned for a refresh that produced no usable quota
// window (every upstream call failed, or none was for a mode the pool serves).
var ErrNoQuotaData = errors.New("account: upstream returned no usable quota data")

var (
	errAccountNotFound = errors.New("account: not found")
	errNoFetcher       = errors.New("account: no usage fetcher configured")
)

// errInvalidCredentials wraps the upstream error of a rejected token.
type errInvalidCredentials struct {
	Token string
	Err   error
}

func (e *errInvalidCredentials) Error() string {
	return "invalid credentials for token " + platform.MaskToken(e.Token) + ": " + e.Err.Error()
}

func (e *errInvalidCredentials) Unwrap() error { return e.Err }

func (e *errInvalidCredentials) Is(target error) bool { return target == ErrInvalidCredentials }

// isInvalidCredentialErr reports whether err is an upstream response that
// declares the token invalid (400/401/403 with a known marker in the body).
func isInvalidCredentialErr(err error) bool {
	var appErr *platform.AppError
	if !errors.As(err, &appErr) {
		return false
	}
	switch appErr.Status {
	case 400, 401, 403:
		return platform.IsInvalidCredentialsBody(appErr.Body)
	}
	return false
}

func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// ModeQuota is a single mode's live rate-limit window from the upstream API.
type ModeQuota struct {
	Remaining int
	Total     int
	WindowSec int
	ResetAtMs *int64
}

// UsageFetcher fetches live quota windows for a token/pool. Implemented by
// the grok transport package to avoid an import cycle.
//
// FetchAllQuotas may return (nil, nil) when every mode failed for a
// non-credential reason; the RefreshService treats that as a failure.
// FetchModeQuota is not called by this package (kept for the existing
// implementations).
type UsageFetcher interface {
	FetchAllQuotas(ctx context.Context, token, pool string, bootstrap bool) (map[int]ModeQuota, error)
	FetchModeQuota(ctx context.Context, token, pool string, modeID int) (*ModeQuota, error)
}

// RefreshOutcome is the per-token result of a refresh.
type RefreshOutcome struct {
	Token string
	// Refreshed is true when fresh quota data was fetched and stored.
	Refreshed bool
	// Expired is true when upstream rejected the credentials and the account
	// was marked expired. Err then matches ErrInvalidCredentials.
	Expired bool
	// Err explains a failure; nil when Refreshed.
	Err error
}

const (
	defaultRefreshConcurrency = 8
	defaultRefreshBatchSize   = 100
	defaultOnDemandWait       = 10 * time.Second
	defaultOnDemandMaxRun     = 5 * time.Minute
	defaultOnDemandMinDelta   = 300 * time.Second
	maxFailureReasonLen       = 300
)

// scheduledPageSize is the page size used to walk the pool (a variable so
// tests can force several pages).
var scheduledPageSize = MaxPageSize

// refreshCall is one in-flight refresh of one token; concurrent callers for the
// same token wait on done instead of hitting upstream again.
type refreshCall struct {
	done chan struct{}
	out  RefreshOutcome
}

type onDemandRun struct{ done chan struct{} }

// RefreshService syncs account quotas from the upstream API and records
// request failures.
//
// Pipeline: callers hand records to refreshRecords, which (per batch of
// batchSize) claims each token (per-token singleflight), fetches the claimed
// ones with a bounded worker pool, persists all resulting patches with ONE
// PatchAccounts call (one file flush) and then publishes the outcomes.
type RefreshService struct {
	repo    Repository
	fetcher UsageFetcher

	fetchSem  chan struct{} // bounds concurrent upstream refreshes, all callers
	batchSize int

	inflightMu sync.Mutex
	inflight   map[string]*refreshCall

	onDemandMu       sync.Mutex
	lastOnDemand     time.Time
	minOnDemandDelta time.Duration
	onDemandWait     time.Duration
	onDemandMaxRun   time.Duration
	onDemandRun      *onDemandRun
	closed           bool // set by Close; guarded by onDemandMu

	baseCtx context.Context
	cancel  context.CancelFunc
	bg      sync.WaitGroup
}

// NewRefreshService creates a refresh service bound to the repository.
func NewRefreshService(repo Repository, fetcher UsageFetcher) *RefreshService {
	ctx, cancel := context.WithCancel(context.Background())
	return &RefreshService{
		repo:             repo,
		fetcher:          fetcher,
		fetchSem:         make(chan struct{}, defaultRefreshConcurrency),
		batchSize:        defaultRefreshBatchSize,
		inflight:         map[string]*refreshCall{},
		minOnDemandDelta: defaultOnDemandMinDelta,
		onDemandWait:     defaultOnDemandWait,
		onDemandMaxRun:   defaultOnDemandMaxRun,
		baseCtx:          ctx,
		cancel:           cancel,
	}
}

// Close cancels background work started by the service (on-demand passes)
// and waits for it to finish.
func (s *RefreshService) Close() {
	s.onDemandMu.Lock()
	s.closed = true
	s.onDemandMu.Unlock()
	s.cancel()
	s.bg.Wait()
}

// --- public refresh entry points ---

// RefreshTokens forces a full upstream quota refresh for the given tokens and
// returns how many succeeded and failed. Use RefreshTokensDetailed to learn
// which tokens failed and why. The error is non-nil only when ctx ended.
func (s *RefreshService) RefreshTokens(ctx context.Context, tokens []string) (int, int, error) {
	if s.fetcher == nil {
		return 0, 0, nil
	}
	refreshed, failed := countOutcomes(s.RefreshTokensDetailed(ctx, tokens))
	return refreshed, failed, ctx.Err()
}

// RefreshTokensDetailed refreshes the given tokens and reports one outcome per
// distinct token, in first-seen order. Concurrent refreshes of the same token
// (from any caller) share one upstream round trip.
func (s *RefreshService) RefreshTokensDetailed(ctx context.Context, tokens []string) []RefreshOutcome {
	seen := make(map[string]struct{}, len(tokens))
	uniq := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if _, dup := seen[tok]; !dup {
			seen[tok] = struct{}{}
			uniq = append(uniq, tok)
		}
	}
	out := make([]RefreshOutcome, len(uniq))
	for i, tok := range uniq {
		out[i] = RefreshOutcome{Token: tok}
	}
	if s.fetcher == nil {
		for i := range out {
			out[i].Err = errNoFetcher
		}
		return out
	}
	recs, err := s.repo.GetAccounts(ctx, uniq)
	if err != nil {
		for i := range out {
			out[i].Err = err
		}
		return out
	}
	byToken := make(map[string]*Record, len(recs))
	for _, rec := range recs {
		byToken[rec.Token] = rec
	}
	var targets []*Record
	var idx []int
	for i, tok := range uniq {
		rec := byToken[platform.SanitizeToken(tok)]
		if rec == nil {
			out[i].Err = errAccountNotFound
			continue
		}
		targets = append(targets, rec)
		idx = append(idx, i)
	}
	for k, o := range s.refreshRecords(ctx, targets, true) {
		o.Token = uniq[idx[k]]
		out[idx[k]] = o
	}
	return out
}

// RefreshScheduled refreshes every active and cooling account of a pool (all
// pools when pool is empty), walking all pages of the pool. It returns the
// number of accounts refreshed and failed; ctx.Err() is returned when the pass
// was cut short.
func (s *RefreshService) RefreshScheduled(ctx context.Context, pool string) (int, int, error) {
	if s.fetcher == nil {
		return 0, 0, nil
	}
	recs, err := s.listRefreshable(ctx, pool)
	if err != nil {
		return 0, 0, err
	}
	refreshed, failed := countOutcomes(s.refreshRecords(ctx, recs, false))
	return refreshed, failed, ctx.Err()
}

// listRefreshable snapshots every active/cooling record of the pool. The whole
// list is taken before any refresh runs so that status changes made by the
// refresh itself cannot shift pages and skip accounts.
func (s *RefreshService) listRefreshable(ctx context.Context, pool string) ([]*Record, error) {
	poolFilter := ""
	if pool != "" {
		poolFilter = pool
		if id, ok := PoolFromName(pool); ok {
			poolFilter = id.Name()
		}
	}
	var out []*Record
	for _, st := range []Status{StatusActive, StatusCooling} {
		st := st
		for page := 1; ; page++ {
			p, err := s.repo.ListAccounts(ctx, ListQuery{
				Page: page, PageSize: scheduledPageSize, Status: &st, Pool: poolFilter, SortBy: "token",
			})
			if err != nil {
				return nil, err
			}
			out = append(out, p.Items...)
			if page >= p.TotalPages || len(p.Items) == 0 {
				break
			}
		}
	}
	return out, nil
}

func countOutcomes(outs []RefreshOutcome) (refreshed, failed int) {
	for _, o := range outs {
		if o.Refreshed {
			refreshed++
		} else {
			failed++
		}
	}
	return refreshed, failed
}

// RefreshOnDemand triggers a full refresh after the pool ran out of accounts.
//
// It never blocks the caller for longer than the on-demand wait (10 s) or its
// context: the pass itself runs in the background, concurrent callers join
// the pass already running, and a new pass is started at most once per
// throttle interval (5 min). The result is intentionally not returned: callers
// retry their reservation afterwards.
func (s *RefreshService) RefreshOnDemand(ctx context.Context) error {
	if s.fetcher == nil {
		return nil
	}
	s.onDemandMu.Lock()
	if s.closed {
		// Shutting down: starting a pass now would race Close's wait.
		s.onDemandMu.Unlock()
		return nil
	}
	run := s.onDemandRun
	if run == nil {
		if time.Since(s.lastOnDemand) < s.minOnDemandDelta {
			s.onDemandMu.Unlock()
			return nil
		}
		s.lastOnDemand = time.Now()
		run = &onDemandRun{done: make(chan struct{})}
		s.onDemandRun = run
		s.bg.Add(1)
		go s.runOnDemand(run)
	}
	wait := s.onDemandWait
	s.onDemandMu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-run.done:
		return nil
	case <-timer.C:
		return nil // still running in the background
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *RefreshService) runOnDemand(run *onDemandRun) {
	defer s.bg.Done()
	ctx, cancel := context.WithTimeout(s.baseCtx, s.onDemandMaxRun)
	defer cancel()
	refreshed, failed, err := s.RefreshScheduled(ctx, "")
	if err != nil {
		logger.Warnf("account on-demand refresh incomplete: refreshed=%d failed=%d error=%v", refreshed, failed, err)
	} else {
		logger.Infof("account on-demand refresh completed: refreshed=%d failed=%d", refreshed, failed)
	}
	s.onDemandMu.Lock()
	s.onDemandRun = nil
	s.onDemandMu.Unlock()
	close(run.done)
}

// RunScheduledLoop refreshes the pool every interval until ctx is done. The
// first pass runs after initialDelay; a negative initialDelay waits one full
// interval instead. It blocks, so run it in a goroutine.
func (s *RefreshService) RunScheduledLoop(ctx context.Context, pool string, interval, initialDelay time.Duration) {
	if interval <= 0 {
		return
	}
	wait := initialDelay
	if wait < 0 {
		wait = interval
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		refreshed, failed, err := s.RefreshScheduled(ctx, pool)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			logger.Errorf("account refresh cycle failed: pool=%s error=%v", pool, err)
		default:
			logger.Infof("account refresh cycle completed: pool=%s refreshed=%d failed=%d", pool, refreshed, failed)
		}
		timer.Reset(interval)
	}
}

// --- pipeline ---

// refreshRecords refreshes recs in batches and returns one outcome per record
// (same order). bootstrap is forwarded to the fetcher.
func (s *RefreshService) refreshRecords(ctx context.Context, recs []*Record, bootstrap bool) []RefreshOutcome {
	out := make([]RefreshOutcome, len(recs))
	size := s.batchSize
	if size <= 0 {
		size = defaultRefreshBatchSize
	}
	for start := 0; start < len(recs); start += size {
		end := start + size
		if end > len(recs) {
			end = len(recs)
		}
		if err := ctx.Err(); err != nil {
			for i := start; i < len(recs); i++ {
				out[i] = RefreshOutcome{Token: recs[i].Token, Err: err}
			}
			break
		}
		s.refreshBatch(ctx, recs[start:end], out[start:end], bootstrap)
	}
	return out
}

func (s *RefreshService) refreshBatch(ctx context.Context, recs []*Record, out []RefreshOutcome, bootstrap bool) {
	calls := make([]*refreshCall, len(recs))
	var leaders, followers []int
	for i, rec := range recs {
		c, leader := s.claim(rec.Token)
		calls[i] = c
		if leader {
			leaders = append(leaders, i)
		} else {
			followers = append(followers, i)
		}
	}

	patches := make([]*Patch, len(recs))
	s.fetchLeaders(ctx, recs, leaders, out, patches, bootstrap)
	s.persistBatch(ctx, recs, leaders, out, patches)
	// Leaders publish before anyone waits, so two overlapping batches can
	// never wait on each other.
	for _, i := range leaders {
		s.finish(recs[i].Token, calls[i], out[i])
	}
	for _, i := range followers {
		out[i] = s.await(ctx, calls[i], recs[i], bootstrap)
	}
}

func (s *RefreshService) claim(token string) (*refreshCall, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if c, ok := s.inflight[token]; ok {
		return c, false
	}
	c := &refreshCall{done: make(chan struct{})}
	s.inflight[token] = c
	return c, true
}

func (s *RefreshService) finish(token string, c *refreshCall, out RefreshOutcome) {
	s.inflightMu.Lock()
	if s.inflight[token] == c {
		delete(s.inflight, token)
	}
	s.inflightMu.Unlock()
	c.out = out
	close(c.done)
}

func (s *RefreshService) await(ctx context.Context, c *refreshCall, rec *Record, bootstrap bool) RefreshOutcome {
	select {
	case <-c.done:
	case <-ctx.Done():
		return RefreshOutcome{Token: rec.Token, Err: ctx.Err()}
	}
	o := c.out
	if isContextErr(o.Err) && ctx.Err() == nil {
		// The leader's caller went away mid-fetch: do the work ourselves.
		return s.refreshRecords(ctx, []*Record{rec}, bootstrap)[0]
	}
	o.Token = rec.Token
	return o
}

// fetchLeaders runs the upstream fetches of the claimed records with at most
// min(len, concurrency) workers; the shared semaphore additionally bounds the
// total across concurrent callers.
func (s *RefreshService) fetchLeaders(ctx context.Context, recs []*Record, leaders []int, out []RefreshOutcome, patches []*Patch, bootstrap bool) {
	if len(leaders) == 0 {
		return
	}
	workers := cap(s.fetchSem)
	if workers > len(leaders) {
		workers = len(leaders)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				out[i], patches[i] = s.fetchOne(ctx, recs[i], bootstrap)
			}
		}()
	}
	for _, i := range leaders {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
}

// fetchOne fetches one account and returns its outcome plus the patch that
// should be persisted for it (nil when nothing should be written).
func (s *RefreshService) fetchOne(ctx context.Context, rec *Record, bootstrap bool) (RefreshOutcome, *Patch) {
	out := RefreshOutcome{Token: rec.Token}
	select {
	case s.fetchSem <- struct{}{}:
		defer func() { <-s.fetchSem }()
	case <-ctx.Done():
		out.Err = ctx.Err()
		return out, nil
	}

	quotas, err := s.fetcher.FetchAllQuotas(ctx, rec.Token, rec.Pool, bootstrap)
	var patch Patch
	if err == nil {
		// (nil, nil) from the fetcher means every mode failed.
		patch, err = quotaPatch(rec, quotas)
	}
	switch {
	case err == nil:
		out.Refreshed = true
		return out, &patch
	case isInvalidCredentialErr(err):
		p := expiredPatch(rec.Token, platform.NowMs())
		out.Expired = true
		out.Err = &errInvalidCredentials{Token: rec.Token, Err: err}
		return out, &p
	case ctx.Err() != nil:
		// Our own cancellation says nothing about the account.
		out.Err = err
		return out, nil
	default:
		p := failurePatch(rec.Token, err.Error(), platform.NowMs())
		out.Err = err
		return out, &p
	}
}

// persistBatch writes the patches of all leaders with one PatchAccounts call.
// The write is not tied to the caller's cancellation: the upstream calls were
// already paid for.
func (s *RefreshService) persistBatch(ctx context.Context, recs []*Record, leaders []int, out []RefreshOutcome, patches []*Patch) {
	var list []Patch
	var idxs []int
	for _, i := range leaders {
		if patches[i] != nil {
			list = append(list, *patches[i])
			idxs = append(idxs, i)
		}
	}
	if len(list) == 0 {
		return
	}
	if _, err := s.repo.PatchAccounts(context.WithoutCancel(ctx), list); err != nil {
		logger.Warnf("account refresh batch not persisted: patches=%d error=%v", len(list), err)
		for _, i := range idxs {
			out[i].Refreshed = false
			if out[i].Err == nil {
				out[i].Err = err
			}
		}
		return
	}
	for _, i := range idxs {
		if out[i].Expired {
			logger.Warnf("account expired: token=%s reason=%s", platform.MaskToken(recs[i].Token), ReasonInvalidCredentials)
		}
	}
}

// --- patches ---

// quotaPatch builds the patch that stores freshly fetched quota windows. It
// fails with ErrNoQuotaData when nothing usable was fetched.
func quotaPatch(rec *Record, quotas map[int]ModeQuota) (Patch, error) {
	patch := Patch{Token: rec.Token}
	qs := rec.QuotaSet()
	applied := 0
	syncedAt := platform.NowMs()
	for modeID, mq := range quotas {
		if !SupportsMode(rec.Pool, modeID) {
			continue
		}
		w := NormalizeQuotaWindow(rec.Pool, modeID, QuotaWindow{
			Remaining:     mq.Remaining,
			Total:         mq.Total,
			WindowSeconds: mq.WindowSec,
			ResetAt:       mq.ResetAtMs,
			SyncedAt:      ptrInt64(syncedAt),
			Source:        SourceReal,
		})
		m := w.ToMap()
		patch.setQuota(modeID, &m)
		qs.Set(modeID, w)
		applied++
	}
	if applied == 0 {
		return Patch{}, ErrNoQuotaData
	}
	// Infer pool from live quota data and patch if changed.
	if inferred := InferPool(qs); inferred != "" && inferred != rec.Pool {
		logger.Infof("account pool updated from live quota: token=%s previous=%s current=%s", platform.MaskToken(rec.Token), rec.Pool, inferred)
		patch.Pool = &inferred
	}
	patch.LastSyncAt = &syncedAt
	patch.UsageSyncDelta = ptrInt(1)
	return patch, nil
}

// expiredPatch marks an account EXPIRED because upstream rejected its token.
// ExtMerge is a fresh map: the repository merges it into the stored Ext.
func expiredPatch(token string, nowMs int64) Patch {
	reason := ReasonInvalidCredentials
	st := StatusExpired
	return Patch{
		Token:          token,
		Status:         &st,
		LastFailAt:     &nowMs,
		LastFailReason: &reason,
		StateReason:    &reason,
		ExtMerge: map[string]any{
			ExtExpiredAt:     nowMs,
			ExtExpiredReason: reason,
		},
	}
}

// failurePatch records one failed request/refresh without changing status.
func failurePatch(token, reason string, nowMs int64) Patch {
	reason = truncateReason(reason)
	return Patch{
		Token:          token,
		UsageFailDelta: ptrInt(1),
		LastFailAt:     &nowMs,
		LastFailReason: &reason,
	}
}

// truncateReason bounds upstream-supplied text stored on the account.
func truncateReason(s string) string {
	if len(s) <= maxFailureReasonLen {
		return s
	}
	return strings.ToValidUTF8(s[:maxFailureReasonLen], "") + "..."
}

// --- request-path failure recording ---

// RecordFailure records a single request failure (fire-and-forget).
// If the error indicates invalid credentials, the account is marked EXPIRED.
// modeID is accepted for symmetry with Directory.Feedback and is not used.
func (s *RefreshService) RecordFailure(ctx context.Context, token string, modeID int, err error) {
	// The failure happened; persisting it must not depend on the request.
	ctx = context.WithoutCancel(ctx)
	if isInvalidCredentialErr(err) {
		recs, _ := s.repo.GetAccounts(ctx, []string{token})
		if len(recs) > 0 {
			_ = s.markCredentialsExpired(ctx, recs[0])
		}
		return
	}
	reason := "rate_limited"
	if err != nil {
		reason = err.Error()
	}
	patch := failurePatch(token, reason, platform.NowMs())
	if _, perr := s.repo.PatchAccounts(ctx, []Patch{patch}); perr != nil {
		logger.Warnf("account failure record not persisted: token=%s error=%v", platform.MaskToken(token), perr)
		return
	}
}

// markCredentialsExpired sets the account status to EXPIRED with reason.
func (s *RefreshService) markCredentialsExpired(ctx context.Context, rec *Record) error {
	patch := expiredPatch(rec.Token, platform.NowMs())
	if _, err := s.repo.PatchAccounts(ctx, []Patch{patch}); err != nil {
		logger.Warnf("account expired but not persisted: token=%s reason=%s error=%v", platform.MaskToken(rec.Token), ReasonInvalidCredentials, err)
		return err
	}
	logger.Warnf("account expired: token=%s reason=%s", platform.MaskToken(rec.Token), ReasonInvalidCredentials)
	return nil
}

// ResetExpiredConsoleWindows delegates to the repository's ConsoleMaintainer
// capability (a no-op when the repository has none).
func (s *RefreshService) ResetExpiredConsoleWindows(ctx context.Context) (int, error) {
	if m, ok := s.repo.(ConsoleMaintainer); ok {
		return m.ResetExpiredConsoleWindows(ctx)
	}
	return 0, nil
}

// RecoverConsoleExpiredAccounts delegates to the repository's
// ConsoleMaintainer capability (a no-op when the repository has none).
func (s *RefreshService) RecoverConsoleExpiredAccounts(ctx context.Context) (int, error) {
	if m, ok := s.repo.(ConsoleMaintainer); ok {
		return m.RecoverConsoleExpiredAccounts(ctx)
	}
	return 0, nil
}

// ReconcileRuntime applies the config-driven runtime settings to the
// directory and returns the effective strategy: quota when refresh is enabled,
// random otherwise. maxInflight is account.selection.max_inflight (<= 0 means
// DefaultMaxInflight); it caps concurrent requests per account for both
// strategies.
func ReconcileRuntime(dir *Directory, enabled bool, maxInflight int) Strategy {
	target := StrategyRandom
	if enabled {
		target = StrategyQuota
	}
	dir.SetStrategy(target)
	dir.SetMaxInflight(maxInflight)
	return target
}
