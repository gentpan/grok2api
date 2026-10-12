package api

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
	"github.com/gentpan/grok2api/internal/storage"
)

// Version is reported by /meta; main sets it from the build-time value.
var Version = "dev"

// maxRequestBodyBytes caps every request body (JSON and multipart alike).
const maxRequestBodyBytes = 64 << 20

// conversationTTL is how long an upstream conversation may be continued.
const conversationTTL = 10 * time.Minute

// Server bundles the dependencies every handler needs.
//
// Everything a Server owns (key store, panel account, custom models, usage
// counters, video jobs, rate limiter) is a field of the Server: two Servers in
// one process do not share state, and a Server literal is usable — missing
// stores default to in-memory ones on first use.
type Server struct {
	Repo        account.Repository
	Directory   Pool
	Refresh     Refresher
	Transport   Upstream
	Media       MediaStore
	ConvTracker ConversationStore
	Usage       *usageRecorder

	// Config overrides the process-wide config singleton (tests).
	Config ConfigStore
	// Models overrides the process-wide model registry (tests).
	Models ModelRegistry

	// Owned state; nil fields are defaulted lazily (see ensureDefaults).
	keys    *apiKeyStore
	auth    *adminAuth
	custom  *modelStore
	limiter *failLimiter
	jobs    *videoJobs

	// Where media is served from (defaults to package storage).
	imageDirFn func() (string, error)
	videoDirFn func() (string, error)

	defaultsOnce sync.Once

	reconcileMu sync.Mutex
	reconcileAt map[string]reconcileStamp

	life lifecycle
}

// Deps is the full constructor input of a Server. Zero fields get defaults.
type Deps struct {
	Repo          account.Repository
	Directory     Pool
	Refresh       Refresher
	Transport     Upstream
	Media         MediaStore
	Conversations ConversationStore
	Config        ConfigStore
	Models        ModelRegistry

	// DataDir is where the Server keeps its JSON files (default: the
	// process data dir, platform.DataDir()). Empty with NoPersistence keeps
	// everything in memory.
	DataDir       string
	NoPersistence bool

	// BcryptCost lets tests lower the panel password hashing cost.
	BcryptCost int
	// AdminFailMax / AdminFailWindow tune the admin brute-force limiter.
	AdminFailMax    int
	AdminFailWindow time.Duration
}

// NewServer constructs a Server bound to the given dependencies, using the
// process data dir and config singleton.
func NewServer(repo account.Repository, dir *account.Directory, refresh *account.RefreshService, transport *grok.Transport, media *storage.LocalMediaCacheStore) *Server {
	d := Deps{Repo: repo}
	// Typed nil pointers must not become non-nil interfaces.
	if dir != nil {
		d.Directory = dir
	}
	if refresh != nil {
		d.Refresh = refresh
	}
	if transport != nil {
		d.Transport = grokUpstream{transport}
	}
	if media != nil {
		d.Media = media
	}
	return NewServerFromDeps(d)
}

// NewServerFromDeps constructs a Server from explicit dependencies and
// replays the persisted custom-model changes into its model registry.
func NewServerFromDeps(d Deps) *Server {
	dataDir := d.DataDir
	if dataDir == "" && !d.NoPersistence {
		dataDir = platform.DataDir()
	}
	path := func(name string) string {
		if d.NoPersistence || dataDir == "" {
			return ""
		}
		return filepath.Join(dataDir, name)
	}
	s := &Server{
		Repo:        d.Repo,
		Directory:   d.Directory,
		Refresh:     d.Refresh,
		Transport:   d.Transport,
		Media:       d.Media,
		ConvTracker: d.Conversations,
		Config:      d.Config,
		Models:      d.Models,
		keys:        newAPIKeyStore(path("api_keys.json")),
		auth:        newAdminAuth(path("admin_auth.json"), d.BcryptCost),
		custom:      newModelStore(path("custom_models.json")),
		Usage:       newUsageRecorder(path("usage.json")),
		limiter:     newFailLimiter(d.AdminFailMax, d.AdminFailWindow),
	}
	if s.ConvTracker == nil {
		// TODO(integrate): grok.NewConversationTracker starts a sweeper
		// goroutine that cannot be stopped; Close() calls Close on the tracker
		// when it grows one.
		s.ConvTracker = grok.NewConversationTracker(conversationTTL)
	}
	s.custom.apply(s.modelRegistry())
	return s
}

// ensureDefaults fills in whatever a Server literal left nil with in-memory
// state, once.
func (s *Server) ensureDefaults() {
	s.defaultsOnce.Do(func() {
		if s.keys == nil {
			s.keys = newAPIKeyStore("")
		}
		if s.auth == nil {
			s.auth = newAdminAuth("", 0)
		}
		if s.custom == nil {
			s.custom = newModelStore("")
		}
		if s.limiter == nil {
			s.limiter = newFailLimiter(0, 0)
		}
		if s.jobs == nil {
			s.jobs = newVideoJobs(videoJobRetention)
		}
	})
}

func (s *Server) keyStore() *apiKeyStore    { s.ensureDefaults(); return s.keys }
func (s *Server) authStore() *adminAuth     { s.ensureDefaults(); return s.auth }
func (s *Server) customModels() *modelStore { s.ensureDefaults(); return s.custom }
func (s *Server) adminLimiter() *failLimiter {
	s.ensureDefaults()
	return s.limiter
}
func (s *Server) videoJobStore() *videoJobs { s.ensureDefaults(); return s.jobs }

// modelRegistry is the active model catalogue.
func (s *Server) modelRegistry() ModelRegistry {
	if s.Models != nil {
		return s.Models
	}
	return globalModels{}
}

// config returns the active configuration.
func (s *Server) config() ConfigStore {
	if s.Config != nil {
		return s.Config
	}
	return globalConfig{}
}

// imageDir / videoDir resolve the on-disk media directories.
func (s *Server) imageDir() (string, error) {
	if s.imageDirFn != nil {
		return s.imageDirFn()
	}
	return storage.ImageFilesDir()
}

func (s *Server) videoDir() (string, error) {
	if s.videoDirFn != nil {
		return s.videoDirFn()
	}
	return storage.VideoFilesDir()
}

// --- lifecycle ---

// lifecycle owns the server-level base context and the tracked background
// goroutines. The zero value is ready to use.
type lifecycle struct {
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards closed and orders wg.Add against Close's wg.Wait.
	mu     sync.Mutex
	closed bool
}

// closeWaitTimeout bounds how long Close waits for background work.
const closeWaitTimeout = 10 * time.Second

// baseContext is the server-level context background work derives from. It
// is cancelled by Close, never by an individual request.
func (s *Server) baseContext() context.Context {
	s.life.once.Do(func() {
		s.life.ctx, s.life.cancel = context.WithCancel(context.Background())
	})
	return s.life.ctx
}

// background runs fn in a tracked goroutine under the base context (bounded by
// timeout when > 0). A panic is logged and contained: unlike a handler panic
// it would otherwise take the whole process down.
func (s *Server) background(name string, timeout time.Duration, fn func(ctx context.Context)) {
	base := s.baseContext()
	s.life.mu.Lock()
	if s.life.closed {
		// Shutting down (a handler outlived http.Shutdown): the work would
		// race Close's wait and its result could not be flushed anyway.
		s.life.mu.Unlock()
		logger.Debugf("background task dropped, server closed: task=%s", name)
		return
	}
	s.life.wg.Add(1)
	s.life.mu.Unlock()
	go func() {
		defer s.life.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("background task panicked: task=%s panic=%v", name, r)
			}
		}()
		ctx := base
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(base, timeout)
			defer cancel()
		}
		fn(ctx)
	}()
}

// Close cancels background work, waits (bounded) for it, then flushes every
// debounced store (key usage counters, usage statistics) to disk. It is safe
// to call more than once.
func (s *Server) Close() error {
	s.ensureDefaults()
	s.baseContext()
	s.life.mu.Lock()
	s.life.closed = true
	s.life.mu.Unlock()
	s.life.cancel()
	done := make(chan struct{})
	go func() {
		s.life.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeWaitTimeout):
		logger.Warnf("server close: background tasks still running after %s", closeWaitTimeout)
	}

	var errs []error
	if err := s.keys.flush(); err != nil {
		errs = append(errs, err)
	}
	if err := s.auth.flush(); err != nil {
		errs = append(errs, err)
	}
	if err := s.custom.f.Close(); err != nil {
		errs = append(errs, err)
	}
	if s.Usage != nil {
		s.Usage.flush()
	}
	if c, ok := s.ConvTracker.(interface{ Close() }); ok {
		c.Close()
	}
	return errors.Join(errs...)
}

// --- quota reconciliation ---

// reconcileMinInterval bounds upstream quota look-ups per account.
const reconcileMinInterval = 15 * time.Second

// reconcileKind says why a quota look-up is wanted. The two kinds are
// debounced independently: a routine post-success sync must not swallow the
// confirmation of a 429 that follows it.
type reconcileKind int

const (
	reconcileAfterSuccess   reconcileKind = iota // keep local quota in step with upstream
	reconcileAfterRateLimit                      // confirm a 429 before trusting it
)

// reconcileStamp is when each kind last ran for one account.
type reconcileStamp struct{ success, rateLimit time.Time }

// reconcileQuota re-reads an account's real quota from upstream and stores it,
// which makes the directory rebuild the slot from upstream truth. It is the
// 429-confirmation path and returns false when it was skipped because one ran
// for this account very recently.
func (s *Server) reconcileQuota(ctx context.Context, token string) bool {
	return s.reconcileQuotaKind(ctx, token, reconcileAfterRateLimit)
}

// reconcileQuotaKind is reconcileQuota for an explicit reason. A 429
// confirmation is only debounced against other confirmations; a success sync is
// skipped when any look-up ran within the interval.
func (s *Server) reconcileQuotaKind(ctx context.Context, token string, kind reconcileKind) bool {
	if s.Refresh == nil {
		return false
	}
	s.reconcileMu.Lock()
	if s.reconcileAt == nil {
		s.reconcileAt = map[string]reconcileStamp{}
	}
	now := time.Now()
	recent := func(t time.Time) bool { return !t.IsZero() && now.Sub(t) < reconcileMinInterval }
	st := s.reconcileAt[token]
	skip := recent(st.rateLimit)
	if kind == reconcileAfterSuccess {
		skip = skip || recent(st.success)
	}
	if skip {
		s.reconcileMu.Unlock()
		return false
	}
	// Forget stale entries so the map is bounded by the tokens seen within
	// one interval, not by every token ever seen.
	for t, e := range s.reconcileAt {
		if !recent(e.success) && !recent(e.rateLimit) {
			delete(s.reconcileAt, t)
		}
	}
	if kind == reconcileAfterSuccess {
		st.success = now
	} else {
		st.rateLimit = now
	}
	s.reconcileAt[token] = st
	s.reconcileMu.Unlock()
	_, _, _ = s.Refresh.RefreshTokens(ctx, []string{token})
	return true
}

func (s *Server) reconcileQuotaAsync(token string, kind reconcileKind) {
	if s.Refresh == nil {
		return
	}
	s.background("quota-reconcile", 30*time.Second, func(ctx context.Context) {
		s.reconcileQuotaKind(ctx, token, kind)
	})
}
