package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Update must not report success when nothing can be persisted.
func TestUpdateWithoutUserPathReturnsError(t *testing.T) {
	s := &Snapshot{}
	err := s.Update(map[string]any{"a": map[string]any{"b": int64(1)}})
	if !errors.Is(err, ErrNoUserPath) {
		t.Fatalf("Update with empty userPath = %v, want ErrNoUserPath", err)
	}
}

// blockingValue is a TOML text marshaler that parks the encoder until
// released, so a test can observe the snapshot while Update is mid-write.
type blockingValue struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingValue) MarshalText() ([]byte, error) {
	close(b.entered)
	<-b.release
	return []byte("blocked"), nil
}

// Disk IO in Update must happen outside the data lock: readers stay
// responsive for the whole duration of the write.
func TestUpdateDoesNotBlockReadersDuringWrite(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath: filepath.Join(dir, "defaults.toml"),
		userPath:     filepath.Join(dir, "config.toml"),
	}
	writeFile(t, s.defaultsPath, "[a]\nb = 1\n", time.Now().Add(-time.Hour))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	bv := &blockingValue{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- s.Update(map[string]any{"a": map[string]any{"c": bv}})
	}()
	select {
	case <-bv.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Update never reached the encoder")
	}

	read := make(chan int, 1)
	go func() { read <- s.GetInt("a.b", -1) }()
	select {
	case v := <-read:
		if v != 1 {
			t.Fatalf("a.b = %d, want 1", v)
		}
	case <-time.After(2 * time.Second):
		close(bv.release)
		t.Fatal("GetInt blocked while Update was writing to disk")
	}
	close(bv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Concurrent Updates are serialised, none of the merges is lost.
func TestUpdateConcurrentMergesAreNotLost(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{userPath: filepath.Join(dir, "config.toml")}
	const n = 16
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			errs <- s.Update(map[string]any{"k": map[string]any{string(rune('a' + i)): int64(i)}})
		}(i)
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got, err := loadTOML(s.userPath)
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := got["k"].(map[string]any); len(k) != n {
		t.Fatalf("expected %d merged keys, got %v", n, got)
	}
}

// Raw/Get/GetList must not hand out the live slices/maps.
func TestAccessorsDoNotExposeLiveInternals(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{defaultsPath: filepath.Join(dir, "defaults.toml")}
	writeFile(t, s.defaultsPath,
		"[retry]\ncodes = [403, 429]\n[tbl]\nnames = [\"a\", \"b\"]\n[tbl.sub]\nx = 1\n", time.Now())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	// Raw: mutate a nested slice and a nested map.
	raw := s.Raw()
	raw["retry"].(map[string]any)["codes"].([]any)[0] = int64(999)
	raw["tbl"].(map[string]any)["sub"].(map[string]any)["x"] = int64(999)
	if got := s.GetList("retry.codes", nil); got[0] != "403" {
		t.Fatalf("Raw() exposed the live slice: %v", got)
	}
	if got := s.GetInt("tbl.sub.x", 0); got != 1 {
		t.Fatalf("Raw() exposed the live map: %d", got)
	}

	// Get: slices and maps are copies too.
	s.Get("retry.codes", nil).([]any)[0] = int64(999)
	s.Get("tbl.sub", nil).(map[string]any)["x"] = int64(999)
	if got := s.GetList("retry.codes", nil); got[0] != "403" {
		t.Fatalf("Get() exposed the live slice: %v", got)
	}
	if got := s.GetInt("tbl.sub.x", 0); got != 1 {
		t.Fatalf("Get() exposed the live map: %d", got)
	}

	// GetList on a []string value returns a copy.
	s.mu.Lock()
	s.data["tbl"].(map[string]any)["strs"] = []string{"p", "q"}
	s.mu.Unlock()
	l := s.GetList("tbl.strs", nil)
	l[0] = "mutated"
	if got := s.GetList("tbl.strs", nil); got[0] != "p" {
		t.Fatalf("GetList exposed the live []string: %v", got)
	}
}

// mtimes are int64 nanoseconds: a 1ns change must still be noticed (float64
// cannot represent UnixNano values exactly).
func TestFileMtimeIsExactNanoseconds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.toml")
	writeFile(t, p, "a = 1\n", time.Now())
	mt := time.Unix(1_700_000_000, 123_456_789)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	if fi.ModTime().UnixNano()%1000 == 0 {
		t.Skip("filesystem does not keep sub-microsecond mtimes")
	}
	var got int64 = fileMtime(p)
	if got != fi.ModTime().UnixNano() {
		t.Fatalf("fileMtime = %d, want %d", got, fi.ModTime().UnixNano())
	}
}

func TestLoadDetectsOneNanosecondMtimeChange(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{userPath: filepath.Join(dir, "config.toml")}
	base := time.Unix(1_700_000_000, 123_456_789)
	writeFile(t, s.userPath, "[a]\nb = 1\n", base)
	fi, _ := os.Stat(s.userPath)
	if fi.ModTime().UnixNano() != base.UnixNano() {
		t.Skip("filesystem does not keep nanosecond mtimes")
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	// Same size, different content, mtime +1ns.
	writeFile(t, s.userPath, "[a]\nb = 2\n", base.Add(1))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetInt("a.b", 0); got != 2 {
		t.Fatalf("a.b = %d, want 2 (1ns mtime change not detected)", got)
	}
}

func TestSubscribeNotifiedAfterSuccessfulReload(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath: filepath.Join(dir, "defaults.toml"),
		userPath:     filepath.Join(dir, "config.toml"),
	}
	t0 := time.Now().Add(-time.Hour)
	writeFile(t, s.defaultsPath, "[a]\nb = 1\n", t0)

	var calls atomic.Int32
	var seen atomic.Int64
	cancel := s.Subscribe(func(snap *Snapshot) {
		calls.Add(1)
		seen.Store(int64(snap.GetInt("a.b", -1))) // must be able to read: no locks held
	})
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || seen.Load() != 1 {
		t.Fatalf("calls=%d seen=%d after first load", calls.Load(), seen.Load())
	}

	// Unchanged files: no notification.
	_ = s.Load()
	if calls.Load() != 1 {
		t.Fatalf("notified without a reload: %d", calls.Load())
	}

	// Update + Load -> exactly one more, with the new value visible.
	if err := s.Update(map[string]any{"a": map[string]any{"b": int64(7)}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || seen.Load() != 7 {
		t.Fatalf("calls=%d seen=%d after update", calls.Load(), seen.Load())
	}

	// A broken file does not notify.
	writeFile(t, s.userPath, "[a\nb = ", t0.Add(time.Minute))
	if err := s.Load(); err == nil {
		t.Fatal("expected parse error")
	}
	if calls.Load() != 2 {
		t.Fatalf("notified for a failed reload: %d", calls.Load())
	}

	// Cancel is idempotent and stops notifications.
	cancel()
	cancel()
	writeFile(t, s.userPath, "[a]\nb = 9\n", t0.Add(2*time.Minute))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("notified after cancel: %d", calls.Load())
	}
}

func TestSubscribePanicDoesNotBreakLoad(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{defaultsPath: filepath.Join(dir, "defaults.toml")}
	writeFile(t, s.defaultsPath, "[a]\nb = 1\n", time.Now())
	var second atomic.Bool
	s.Subscribe(func(*Snapshot) { panic("boom") })
	s.Subscribe(func(*Snapshot) { second.Store(true) })
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if !second.Load() {
		t.Fatal("later subscribers must still run after one panics")
	}
}
