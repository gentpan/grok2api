package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// M7: unchanged files must be served via the read-lock fast path.
func TestLoadFastPathUsesReadLock(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath: filepath.Join(dir, "defaults.toml"),
		userPath:     filepath.Join(dir, "config.toml"),
	}
	writeFile(t, s.defaultsPath, "[a]\nb = 1\n", time.Now())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}

	s.mu.RLock() // a writer would block forever behind this reader
	done := make(chan error, 1)
	go func() { done <- s.Load() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Load() took the write lock although nothing changed")
	}
	s.mu.RUnlock()
}

// M7: a broken file is parsed/reported once, previous config stays active.
func TestLoadBrokenFileIsNotReparsed(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath: filepath.Join(dir, "defaults.toml"),
		userPath:     filepath.Join(dir, "config.toml"),
	}
	t0 := time.Now().Add(-time.Hour)
	writeFile(t, s.defaultsPath, "[a]\nb = 1\n", t0)
	writeFile(t, s.userPath, "[a]\nb = 2\n", t0)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetInt("a.b", 0); got != 2 {
		t.Fatalf("a.b = %d, want 2", got)
	}

	writeFile(t, s.userPath, "[a\nb = ", t0.Add(time.Minute))
	err1 := s.Load()
	if err1 == nil {
		t.Fatal("expected parse error")
	}
	err2 := s.Load()
	if err2 != err1 {
		t.Fatalf("broken file was parsed again (got a new error value)")
	}
	if got := s.GetInt("a.b", 0); got != 2 {
		t.Fatalf("previous config must stay active, a.b = %d", got)
	}

	// Fixing the file (new mtime) reloads.
	writeFile(t, s.userPath, "[a]\nb = 3\n", t0.Add(2*time.Minute))
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetInt("a.b", 0); got != 3 {
		t.Fatalf("a.b = %d, want 3", got)
	}
}

// M10: embedded defaults are used when the defaults file is missing.
func TestEmbeddedDefaultsFallback(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{
		defaultsPath:     filepath.Join(dir, "missing.toml"),
		userPath:         filepath.Join(dir, "config.toml"),
		embeddedDefaults: []byte("[app]\napp_key = \"embedded\"\n[x]\ny = 1\n"),
	}
	writeFile(t, s.userPath, "[x]\ny = 5\n", time.Now())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetStr("app.app_key", ""); got != "embedded" {
		t.Fatalf("app_key = %q", got)
	}
	if got := s.GetInt("x.y", 0); got != 5 {
		t.Fatalf("user config must override embedded defaults, x.y = %d", got)
	}

	// An existing defaults file wins over the embedded copy.
	writeFile(t, s.defaultsPath, "[app]\napp_key = \"file\"\n", time.Now())
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.GetStr("app.app_key", ""); got != "file" {
		t.Fatalf("app_key = %q, want file", got)
	}
}

func TestSetEmbeddedDefaultsGlobal(t *testing.T) {
	SetEmbeddedDefaults([]byte("[z]\nv = 7\n"))
	t.Cleanup(func() {
		SetEmbeddedDefaults(nil)
		SetPaths("", "")
	})
	dir := t.TempDir()
	SetPaths(filepath.Join(dir, "nope.toml"), "")
	if err := Load(); err != nil {
		t.Fatal(err)
	}
	if got := Global().GetInt("z.v", 0); got != 7 {
		t.Fatalf("z.v = %d", got)
	}
}

// M8: Update writes atomically with 0600 and merges into the existing file.
func TestUpdateAtomic0600(t *testing.T) {
	dir := t.TempDir()
	s := &Snapshot{userPath: filepath.Join(dir, "config.toml")}
	writeFile(t, s.userPath, "[a]\nb = 1\nc = 2\n", time.Now().Add(-time.Hour))
	if err := s.Update(map[string]any{"a": map[string]any{"b": int64(9)}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.userPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
	got, err := loadTOML(s.userPath)
	if err != nil {
		t.Fatal(err)
	}
	a := got["a"].(map[string]any)
	if a["b"] != int64(9) || a["c"] != int64(2) {
		t.Fatalf("merged content wrong: %v", got)
	}
}

// M9: nested patches are expanded to dotted paths and checked on those.
func TestFlattenKeysAndStartupOnly(t *testing.T) {
	patch := map[string]any{
		"app":     map[string]any{"app_key": "x"},
		"account": map[string]any{"local": map[string]any{"path": "/tmp/x"}},
	}
	keys := FlattenKeys(patch)
	want := []string{"account.local.path", "app.app_key"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("FlattenKeys = %v, want %v", keys, want)
	}

	// Checking the flattened paths catches the nested form.
	var blocked []string
	for _, k := range keys {
		if IsStartupOnlyConfigKey(k) {
			blocked = append(blocked, k)
		}
	}
	if !reflect.DeepEqual(blocked, []string{"account.local.path"}) {
		t.Fatalf("blocked = %v", blocked)
	}

	cases := map[string]bool{
		"account.storage":                true,
		"account.storage.type":           true,
		"account.redis.url":              true,
		"account":                        true, // replacing the table would clobber reserved keys
		"account.refresh.enabled":        false,
		"account.localfoo":               false, // boundary-aware, no longer a raw prefix match
		"app.app_key":                    false,
		"features.stream":                false,
		"account.selection.max_inflight": false,
	}
	for k, want := range cases {
		if got := IsStartupOnlyConfigKey(k); got != want {
			t.Errorf("IsStartupOnlyConfigKey(%q) = %v, want %v", k, got, want)
		}
	}
}
