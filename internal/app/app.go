// Package app wires the service together: configuration, account storage, the
// upstream transport, the HTTP API and the background schedule, plus their
// start-up order and graceful shutdown. cmd-level code (main.go) only has to
// hand it the build-time bits.
package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gentpan/grok2api/internal/account"
	"github.com/gentpan/grok2api/internal/api"
	"github.com/gentpan/grok2api/internal/config"
	"github.com/gentpan/grok2api/internal/grok"
	"github.com/gentpan/grok2api/internal/logger"
	"github.com/gentpan/grok2api/internal/platform"
	"github.com/gentpan/grok2api/internal/storage"
)

// Options carries what only the binary knows.
type Options struct {
	// Version is the build version reported by the API.
	Version string
	// EmbeddedDefaults is config.defaults.toml compiled into the binary, used
	// when the file is not present on disk.
	EmbeddedDefaults []byte
	// Getenv overrides os.Getenv (tests).
	Getenv func(string) string
}

const (
	logRetentionFiles = 7
	// shutdownTimeout bounds the HTTP drain. It stays below Docker's default
	// 10 s stop grace period so the stores are still flushed (Server.Close)
	// before SIGKILL arrives; longer SSE streams are cut at shutdown anyway.
	shutdownTimeout = 8 * time.Second
)

// App is the assembled service. Build it with New, run it with Run.
type App struct {
	env Env

	repo      *account.TxtRepository
	directory *account.Directory
	refresh   *account.RefreshService
	transport *grok.Transport
	media     *storage.LocalMediaCacheStore
	server    *api.Server
	sched     *Scheduler
	http      *http.Server

	unsubscribe func()

	// mu serialises config-driven reconfiguration against release: a reload
	// that fires while the App shuts down must not touch closed components.
	mu       sync.Mutex
	released bool
}

// Main runs the service until SIGINT/SIGTERM or a fatal error and returns the
// process exit code.
func Main(opts Options) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a, err := New(ctx, opts)
	if err != nil {
		logger.Errorf("startup failed: error=%v", err)
		_ = logger.Close()
		return 1
	}
	err = a.Run(ctx)
	_ = logger.Close()
	if err != nil {
		return 1
	}
	return 0
}

// New loads configuration and builds every component. Nothing is listening or
// scheduled until Run.
func New(ctx context.Context, opts Options) (*App, error) {
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	env, envWarnings := ParseEnv(getenv)

	// Logging comes first (env-driven) so configuration problems are visible.
	logDir := platform.LogDir()
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		env.FileLogging = false // stderr only
	}
	logger.Setup(env.LogLevel, env.FileLogging, logDir, logRetentionFiles)
	// Apply the LOG_LEVEL cap to the file right away, before configuration
	// (and its logging.file_level) is available.
	logger.Reload(env.LogLevel, env.FileLogLevel(""), 0)
	logger.Infof("application startup: service=grok2api version=%s platform=%s", opts.Version, runtime.GOOS)
	for _, w := range envWarnings {
		logger.Warnf("environment: %s", w)
	}

	api.Version = opts.Version
	config.SetEmbeddedDefaults(opts.EmbeddedDefaults)
	config.SetPaths(defaultsConfigPath(env), userConfigPath(env))
	if err := config.Load(); err != nil {
		return nil, fmt.Errorf("config load: %w", err)
	}
	cfg := config.Global()
	logger.Reload(env.LogLevel, env.FileLogLevel(cfg.GetStr("logging.file_level", "")), cfg.GetInt("logging.max_files", logRetentionFiles))
	for _, w := range config.Validate() {
		logger.Warnf("config: %s", w)
	}

	a := &App{env: env}
	ok := false
	defer func() {
		if !ok {
			a.release()
		}
	}()

	// Account storage (JSONL) and the in-memory directory built from it.
	path := accountPath(env, cfg)
	logger.Infof("account storage configured: backend=text target=%s", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("account storage dir: %w", err)
	}
	a.repo = account.NewTxtRepository(path)
	if err := a.repo.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("account repository: %w", err)
	}
	a.directory = account.NewDirectory(a.repo)
	if err := a.directory.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("account directory bootstrap: %w", err)
	}
	logger.Infof("account directory bootstrapped: accounts=%d revision=%d", a.directory.Size(), a.directory.Revision())

	// Upstream transport and the services that use it. Every account mutation
	// (admin edits as well as refresh results) goes through the syncing
	// repository so the directory reflects it immediately.
	var err error
	if a.transport, err = grok.NewTransport(); err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	syncing := account.NewSyncingRepository(a.repo, a.directory)
	a.refresh = account.NewRefreshService(syncing, grok.NewUsageFetcher(a.transport))
	account.ReconcileRuntime(a.directory,
		cfg.GetBool("account.refresh.enabled", false),
		cfg.GetInt("account.selection.max_inflight", account.DefaultMaxInflight))

	a.media = storage.NewLocalMediaCacheStore()
	for _, t := range []storage.MediaType{storage.MediaImage, storage.MediaVideo} {
		if err := a.media.Rebuild(t); err != nil {
			logger.Warnf("media cache rebuild failed: type=%s error=%v", t, err)
		}
	}

	a.server = api.NewServer(syncing, a.directory, a.refresh, a.transport, a.media)
	a.sched = NewScheduler(a.directory, a.refresh, env)
	a.http = &http.Server{
		Addr:              env.Addr(),
		Handler:           a.server.Router(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No Read/WriteTimeout: responses are long-lived SSE streams.
	}

	// Runtime reconfiguration: fires after every successful config reload.
	a.unsubscribe = config.Subscribe(a.onConfigReload)

	ok = true
	return a, nil
}

// onConfigReload applies the settings that may change while running. It reads
// the current configuration itself (not the snapshot it was notified with), so
// concurrent notifications cannot apply settings out of order, and it does
// nothing once the App is released.
func (a *App) onConfigReload(*config.Snapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return
	}
	cfg := config.Global()
	logger.Reload(a.env.LogLevel, a.env.FileLogLevel(cfg.GetStr("logging.file_level", "")), cfg.GetInt("logging.max_files", logRetentionFiles))
	account.ReconcileRuntime(a.directory,
		cfg.GetBool("account.refresh.enabled", false),
		cfg.GetInt("account.selection.max_inflight", account.DefaultMaxInflight))
	a.transport.ReloadConfig()
	a.sched.Reconcile(SettingsFrom(cfg))
}

// Run starts the schedule and the HTTP server and blocks until ctx is
// cancelled or the server fails, then shuts everything down in order. The
// returned error is the fatal server error, if any.
func (a *App) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	a.sched.Start(runCtx, SettingsFrom(config.Global()))

	serverErr := make(chan error, 1)
	go func() {
		logger.Infof("http server listening on %s", a.http.Addr)
		if err := a.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	var fatal error
	select {
	case <-ctx.Done():
		logger.Infof("application shutdown started")
	case fatal = <-serverErr:
		if fatal != nil {
			logger.Errorf("http server failed: error=%v", fatal)
		}
	}

	cancel() // stop the schedule
	shutdownCtx, stop := context.WithTimeout(context.Background(), shutdownTimeout)
	defer stop()
	if err := a.http.Shutdown(shutdownCtx); err != nil {
		logger.Warnf("http server shutdown timed out: error=%v", err)
	}
	a.sched.Wait()
	a.release()

	if fatal == nil {
		logger.Infof("application shutdown completed")
	}
	return fatal
}

// release closes every component that was created, in dependency order. It is
// idempotent for the components that matter and tolerates a partly built App.
func (a *App) release() {
	if a.unsubscribe != nil {
		a.unsubscribe()
		a.unsubscribe = nil
	}
	a.mu.Lock()
	a.released = true
	a.mu.Unlock()
	if a.server != nil {
		if err := a.server.Close(); err != nil {
			logger.Warnf("api server close: error=%v", err)
		}
		a.server = nil
	}
	if a.refresh != nil {
		a.refresh.Close()
		a.refresh = nil
	}
	if a.repo != nil {
		if err := a.repo.Close(context.Background()); err != nil {
			logger.Warnf("repository close failed: error=%v", err)
		}
		a.repo = nil
	}
	if a.media != nil {
		if err := a.media.Close(); err != nil {
			logger.Warnf("media cache close failed: error=%v", err)
		}
		a.media = nil
	}
	if a.transport != nil {
		a.transport.Close()
		a.transport = nil
	}
}

// --- path policy ---

func defaultsConfigPath(env Env) string {
	if env.DefaultsPath != "" {
		return env.DefaultsPath
	}
	return filepath.Join(platform.ProjectRoot(), "config.defaults.toml")
}

func userConfigPath(env Env) string {
	if env.UserPath != "" {
		return env.UserPath
	}
	return platform.DataPath("config.toml")
}

func accountPath(env Env, cfg interface {
	GetStr(key, def string) string
}) string {
	if env.AccountPath != "" {
		return env.AccountPath
	}
	if p := strings.TrimSpace(cfg.GetStr("account.local.path", "")); p != "" {
		return p
	}
	return platform.DataPath("accounts.jsonl")
}
