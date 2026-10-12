package grok

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gentpan/grok2api/internal/config"
)

// useConfig points the process-global config at a temp TOML file for the
// duration of the test and restores an empty config afterwards.
func useConfig(t *testing.T, toml string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	defaults := filepath.Join(dir, "defaults.toml")
	if err := os.WriteFile(defaults, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	config.SetPaths(defaults, p)
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		config.SetPaths("", "")
		_ = config.Load()
	})
}
