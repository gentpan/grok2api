package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gentpan/grok2api/internal/config"
)

// H4: eviction used to deadlock (rows held the only SQLite connection while a
// DELETE was issued). It must now finish and evict the oldest file.
func TestEvictionDoesNotDeadlock(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)

	cfgDir := t.TempDir()
	userCfg := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(userCfg, []byte("[cache.local]\nvideo_max_mb = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config.SetPaths("", userCfg)
	t.Cleanup(func() { config.SetPaths("", "") })
	if err := config.Load(); err != nil {
		t.Fatal(err)
	}

	s := NewLocalMediaCacheStore()
	payload := bytes.Repeat([]byte("x"), 700*1024) // 2 x 700KB > 1MB limit

	done := make(chan error, 1)
	go func() {
		if _, err := s.SaveVideo(payload, "aaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
			done <- err
			return
		}
		time.Sleep(5 * time.Millisecond) // distinct created_at_ns
		_, err := s.SaveVideo(payload, "bbbbbbbbbbbbbbbbbbbbbbbb")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SaveVideo hung: eviction deadlocked")
	}

	dir := filepath.Join(dataDir, "files", "videos")
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaa.mp4")); !os.IsNotExist(err) {
		t.Fatalf("oldest file should have been evicted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbb.mp4")); err != nil {
		t.Fatalf("newest file must be kept: %v", err)
	}
}
