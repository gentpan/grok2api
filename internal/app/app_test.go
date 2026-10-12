package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/config"
)

// newTestApp builds a full App on temp dirs with the shipped defaults. The
// config singleton is process-global, so it is reset afterwards.
func newTestApp(t *testing.T, userToml string) *App {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("LOG_DIR", filepath.Join(dir, "logs"))
	t.Setenv("SERVER_PORT", "0")
	t.Setenv("PROXY_HTTP", "")
	t.Cleanup(func() {
		config.SetPaths("", "")
		config.SetEmbeddedDefaults(nil)
	})

	defaults, err := os.ReadFile(filepath.Join("..", "..", "config.defaults.toml"))
	if err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(user, []byte(userToml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", user)
	t.Setenv("CONFIG_DEFAULTS_PATH", filepath.Join(dir, "missing.defaults.toml"))

	a, err := New(context.Background(), Options{Version: "test", EmbeddedDefaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.release)
	return a
}

// main.go used to be untestable. This pins that the whole service can be
// assembled from config and serves its health endpoint.
func TestNewAssemblesServiceAndServesHealth(t *testing.T) {
	a := newTestApp(t, "")
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	a.http.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("/health = %d: %s", w.Code, w.Body.String())
	}
}

func TestNewFailsOnBrokenConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DATA_DIR", filepath.Join(dir, "data"))
	t.Setenv("LOG_DIR", filepath.Join(dir, "logs"))
	t.Cleanup(func() {
		config.SetPaths("", "")
		config.SetEmbeddedDefaults(nil)
	})
	bad := filepath.Join(dir, "defaults.toml")
	if err := os.WriteFile(bad, []byte("[app\nbroken"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_DEFAULTS_PATH", bad)
	t.Setenv("CONFIG_PATH", filepath.Join(dir, "config.toml"))
	if _, err := New(context.Background(), Options{Version: "test"}); err == nil {
		t.Fatal("expected a config error")
	}
}

// Run must come up, serve, and shut down cleanly when its context ends.
func TestRunStopsOnContextCancel(t *testing.T) {
	a := newTestApp(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// A config reload must reconcile the refresh schedule: enabling refresh in the
// user config starts the quota loops without a restart.
func TestConfigReloadReconcilesSchedule(t *testing.T) {
	a := newTestApp(t, "[account.refresh]\nenabled = false\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.sched.Start(ctx, SettingsFrom(config.Global()))
	if got := a.sched.ActivePools(); len(got) != 0 {
		t.Fatalf("refresh disabled in user config but pools run: %v", got)
	}

	if err := config.Global().Update(map[string]any{"account": map[string]any{"refresh": map[string]any{"enabled": true}}}); err != nil {
		t.Fatal(err)
	}
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "refresh loops after reload", func() bool { return len(a.sched.ActivePools()) == 3 })

	cancel()
	a.sched.Wait()
}

// A config reload that fires while (or after) the App shuts down must be a
// no-op, not a nil dereference on a released component.
func TestConfigReloadAfterReleaseIsANoop(t *testing.T) {
	a := newTestApp(t, "")
	a.release()
	a.onConfigReload(config.Global()) // panicked on the nil transport before
}
