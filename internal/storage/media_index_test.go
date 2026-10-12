package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

const mb = 1024 * 1024

// newIndexTestStore builds a store on a fresh DATA_DIR whose limit is
// controlled by the returned atomic (bytes, applied to every media type).
func newIndexTestStore(t *testing.T, limit int64) (*LocalMediaCacheStore, *atomic.Int64, string) {
	t.Helper()
	dataDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)
	var lim atomic.Int64
	lim.Store(limit)
	s := NewLocalMediaCacheStoreWithLimits(func(MediaType) int64 { return lim.Load() })
	t.Cleanup(func() { _ = s.Close() })
	return s, &lim, dataDir
}

func writeMedia(t *testing.T, dir, name string, size int, mtime time.Time) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("v"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// Rebuild used to stamp every file with created_at=now, which degraded the
// eviction order to file-name order after each restart. It must use the file
// mtime instead.
func TestRebuildOrdersByFileModTime(t *testing.T) {
	s, _, dataDir := newIndexTestStore(t, 1*mb)
	dir := filepath.Join(dataDir, "files", "videos")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Name order is the OPPOSITE of age order: "zzz" is the oldest file.
	writeMedia(t, dir, "zzz.mp4", 700*1024, now.Add(-2*time.Hour))
	writeMedia(t, dir, "aaa.mp4", 700*1024, now.Add(-1*time.Hour))

	if err := s.Rebuild(MediaVideo); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "zzz.mp4")) {
		t.Error("oldest file (by mtime) should have been evicted")
	}
	if !exists(filepath.Join(dir, "aaa.mp4")) {
		t.Error("newest file (by mtime) must be kept")
	}
}

// With no limit nothing was indexed, so enabling a limit at runtime used to
// leave every pre-existing file untracked (and never evicted).
func TestEnablingLimitAtRuntimeIndexesExistingFiles(t *testing.T) {
	s, lim, dataDir := newIndexTestStore(t, 0)
	dir := filepath.Join(dataDir, "files", "videos")
	payload := bytes.Repeat([]byte("x"), 700*1024)

	if _, err := s.SaveVideo(payload, "old1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVideo(payload, "old2"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := os.Chtimes(filepath.Join(dir, "old1.mp4"), now.Add(-3*time.Hour), now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "old2.mp4"), now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Limit disabled so far: the index database was never needed.
	if exists(filepath.Join(dataDir, "cache", "local_media_cache.db")) {
		t.Fatal("index database created although no limit is configured")
	}

	lim.Store(1 * mb) // admin enables a 1 MB limit at runtime
	if _, err := s.SaveVideo(payload, "new"); err != nil {
		t.Fatal(err)
	}
	var left int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		left += info.Size()
	}
	if left > 1*mb {
		t.Fatalf("cache holds %d bytes after the limit was enabled, want <= %d (pre-existing files untracked)", left, 1*mb)
	}
	if exists(filepath.Join(dir, "old1.mp4")) {
		t.Error("oldest pre-existing file should have been evicted first")
	}
	if !exists(filepath.Join(dir, "new.mp4")) {
		t.Error("the file just saved must be kept")
	}
}

// Disabling and re-enabling the limit within one process must rescan too:
// files written while untracked would otherwise be invisible to eviction.
func TestReenablingLimitRescansFilesWrittenWhileDisabled(t *testing.T) {
	s, lim, dataDir := newIndexTestStore(t, 1*mb)
	dir := filepath.Join(dataDir, "files", "videos")
	payload := bytes.Repeat([]byte("x"), 400*1024)

	if _, err := s.SaveVideo(payload, "tracked"); err != nil {
		t.Fatal(err)
	}
	lim.Store(0)
	for _, id := range []string{"u1", "u2", "u3"} { // 1.2 MB written untracked
		if _, err := s.SaveVideo(payload, id); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	for _, id := range []string{"tracked", "u1", "u2"} {
		_ = os.Chtimes(filepath.Join(dir, id+".mp4"), past, past)
	}
	lim.Store(1 * mb)
	if _, err := s.SaveVideo(payload, "fresh"); err != nil {
		t.Fatal(err)
	}
	var left int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, _ := e.Info()
		left += info.Size()
	}
	if left > 1*mb {
		t.Fatalf("cache holds %d bytes, want <= %d", left, 1*mb)
	}
}

// The SQLite handle is opened once per store, not once per operation.
func TestIndexDatabaseOpenedOncePerStore(t *testing.T) {
	s, _, _ := newIndexTestStore(t, 10*mb)
	payload := []byte("data")
	if _, err := s.SaveVideo(payload, "a"); err != nil {
		t.Fatal(err)
	}
	s.dbMu.Lock()
	first := s.db
	s.dbMu.Unlock()
	if first == nil {
		t.Fatal("index database not opened by a limited save")
	}
	if _, err := s.SaveVideo(payload, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(MediaVideo, "a.mp4"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Clear(MediaImage); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(MediaVideo); err != nil {
		t.Fatal(err)
	}
	s.dbMu.Lock()
	again := s.db
	s.dbMu.Unlock()
	if again != first {
		t.Fatal("index database handle was reopened between operations")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close must be a no-op: %v", err)
	}
	// Still usable after Close: reopens lazily.
	if _, err := s.SaveVideo(payload, "c"); err != nil {
		t.Fatal(err)
	}
}

// Delete/Clear/Rebuild without a limit must not create the index database.
func TestUnlimitedStoreNeverCreatesIndex(t *testing.T) {
	s, _, dataDir := newIndexTestStore(t, 0)
	if _, err := s.SaveImage([]byte("img"), "image/png", "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveVideo([]byte("vid"), "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(MediaImage); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Delete(MediaVideo, "v1.mp4"); err != nil || !ok {
		t.Fatalf("Delete = %v, %v", ok, err)
	}
	if n, err := s.Clear(MediaImage); err != nil || n != 1 {
		t.Fatalf("Clear = %d, %v", n, err)
	}
	if exists(filepath.Join(dataDir, "cache", "local_media_cache.db")) {
		t.Fatal("index database created for an unlimited store")
	}
}

func TestSaveImageExtensionFromMime(t *testing.T) {
	s, _, dataDir := newIndexTestStore(t, 0)
	if _, err := s.SaveImage([]byte("a"), "image/PNG", "x1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveImage([]byte("b"), "image/jpeg", "x2"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(dataDir, "files", "images")
	if !exists(filepath.Join(dir, "x1.png")) || !exists(filepath.Join(dir, "x2.jpg")) {
		t.Fatal("unexpected image extensions")
	}
}
