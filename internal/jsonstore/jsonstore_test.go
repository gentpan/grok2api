package jsonstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type doc struct {
	Name  string         `json:"name"`
	Items []string       `json:"items"`
	Count map[string]int `json:"count"`
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenMissingFileIsEmptyAndUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, err := Open(path, Config[doc]{})
	if err != nil {
		t.Fatal(err)
	}
	var got doc
	f.View(func(d *doc) { got = *d })
	if got.Name != "" || got.Items != nil {
		t.Fatalf("zero value expected, got %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("Open must not create the file")
	}
}

func TestUpdatePersistsAtomicallyWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "d.json") // directory is created on demand
	f, _ := Open(path, Config[doc]{})
	if err := f.Update(func(d *doc) error { d.Name = "a"; d.Items = append(d.Items, "x"); return nil }); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != `{"name":"a","items":["x"],"count":null}` {
		t.Fatalf("file = %s", got)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", st.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftover temp files: %v", entries)
	}
	// Reload sees it.
	g, err := Open(path, Config[doc]{})
	if err != nil {
		t.Fatal(err)
	}
	g.View(func(d *doc) {
		if d.Name != "a" || len(d.Items) != 1 {
			t.Fatalf("reloaded = %+v", *d)
		}
	})
}

func TestIndentOption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, _ := Open(path, Config[doc]{Indent: "  "})
	_ = f.Update(func(d *doc) error { d.Name = "a"; return nil })
	if !strings.Contains(readFile(t, path), "\n  \"name\": \"a\"") {
		t.Fatalf("not indented: %s", readFile(t, path))
	}
}

func TestUpdateRollsBackWhenFnFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, _ := Open(path, Config[doc]{})
	_ = f.Update(func(d *doc) error { d.Name = "keep"; return nil })
	boom := errors.New("boom")
	err := f.Update(func(d *doc) error { d.Name = "dropped"; d.Items = []string{"z"}; return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	f.View(func(d *doc) {
		if d.Name != "keep" || d.Items != nil {
			t.Fatalf("state not restored: %+v", *d)
		}
	})
	if !strings.Contains(readFile(t, path), `"keep"`) {
		t.Fatal("file changed by failed update")
	}
}

func TestUpdateRollsBackWhenWriteFails(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The parent "directory" is a regular file, so every write fails.
	f, _ := Open(filepath.Join(blocker, "d.json"), Config[doc]{})
	err := f.Update(func(d *doc) error { d.Name = "never"; return nil })
	if err == nil {
		t.Fatal("expected a write error")
	}
	f.View(func(d *doc) {
		if d.Name != "" {
			t.Fatalf("in-memory value kept a change that was not persisted: %+v", *d)
		}
	})
}

func TestCorruptFileYieldsErrorButUsableStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	_ = os.WriteFile(path, []byte("{not json"), 0o600)
	f, err := Open(path, Config[doc]{})
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if err := f.Update(func(d *doc) error { d.Name = "fresh"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), "fresh") {
		t.Fatal("corrupt file was not replaced")
	}
}

func TestNormalizeRunsAfterLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	_ = os.WriteFile(path, []byte(`{"name":"","items":["","a",""]}`), 0o600)
	f, _ := Open(path, Config[doc]{Normalize: func(d *doc) {
		var kept []string
		for _, it := range d.Items {
			if it != "" {
				kept = append(kept, it)
			}
		}
		d.Items = kept
		if d.Count == nil {
			d.Count = map[string]int{}
		}
	}})
	f.View(func(d *doc) {
		if len(d.Items) != 1 || d.Count == nil {
			t.Fatalf("normalize not applied: %+v", *d)
		}
	})
}

func TestMutateIsDebouncedAndFlushWritesNow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	var errs []error
	var mu sync.Mutex
	f, _ := Open(path, Config[doc]{Debounce: 50 * time.Millisecond, OnError: func(err error) {
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}})
	f.Mutate(func(d *doc) { d.Name = "one" })
	f.Mutate(func(d *doc) { d.Name = "two" })
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("debounced mutate wrote synchronously")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("debounce timer never wrote the file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(readFile(t, path), `"two"`) {
		t.Fatalf("file = %s", readFile(t, path))
	}

	f.Mutate(func(d *doc) { d.Name = "three" })
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), `"three"`) {
		t.Fatalf("Flush did not write: %s", readFile(t, path))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 0 {
		t.Fatalf("unexpected write errors: %v", errs)
	}
}

func TestCloseFlushesPendingMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, _ := Open(path, Config[doc]{Debounce: time.Hour})
	f.Mutate(func(d *doc) { d.Name = "pending" })
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, path), `"pending"`) {
		t.Fatal("Close did not flush")
	}
}

func TestConcurrentUpdateAndMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, _ := Open(path, Config[doc]{Debounce: 5 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = f.Update(func(d *doc) error { d.Items = append(d.Items, "u"); return nil })
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				f.Mutate(func(d *doc) {
					if d.Count == nil {
						d.Count = map[string]int{}
					}
					d.Count["m"]++
				})
			}
		}()
	}
	wg.Wait()
	_ = f.Close()
	g, err := Open(path, Config[doc]{})
	if err != nil {
		t.Fatal(err)
	}
	g.View(func(d *doc) {
		if len(d.Items) != 160 || d.Count["m"] != 160 {
			t.Fatalf("lost writes: items=%d m=%d", len(d.Items), d.Count["m"])
		}
	})
}

func TestEmptyPathIsMemoryOnly(t *testing.T) {
	f, err := Open("", Config[doc]{})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Update(func(d *doc) error { d.Name = "mem"; return nil }); err != nil {
		t.Fatal(err)
	}
	f.Mutate(func(d *doc) { d.Items = []string{"x"} })
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f.View(func(d *doc) {
		if d.Name != "mem" || len(d.Items) != 1 {
			t.Fatalf("value = %+v", *d)
		}
	})
}

// A debounced save used to run marshal + write + fsync under the data lock, so
// every reader and mutator (each API request) stalled for the fsync. While a
// write is in flight the value must stay readable and mutable.
func TestDebouncedWriteDoesNotBlockReadersOrMutators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, err := Open(path, Config[doc]{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.beforeWrite = func() {
		once.Do(func() { close(entered) })
		<-release
	}

	f.Mutate(func(d *doc) { d.Name = "first" })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("debounced write never started")
	}

	done := make(chan struct{})
	go func() {
		f.View(func(d *doc) { _ = d.Name })
		f.Mutate(func(d *doc) { d.Name = "second" })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("reader/mutator blocked behind an in-flight write")
	}
	close(release)

	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, `"second"`) {
		t.Fatalf("latest value not on disk after Flush: %s", got)
	}
}

// Flush/Close must be a barrier: when it returns, a write that the debounce
// timer had already started is on disk too.
func TestFlushWaitsForAnInFlightTimerWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, err := Open(path, Config[doc]{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var once sync.Once
	f.beforeWrite = func() {
		once.Do(func() { close(entered) })
		time.Sleep(150 * time.Millisecond) // a slow fsync
	}
	f.Mutate(func(d *doc) { d.Name = "slow" })
	<-entered

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, `"slow"`) {
		t.Fatalf("Close returned before the in-flight write landed: %q", got)
	}
}

// Writes are numbered: a snapshot that loses the race for the disk must not
// replace a newer one.
func TestOlderSnapshotNeverOverwritesNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, err := Open(path, Config[doc]{})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.data.Name = "old"
	oldB, oldSeq, _ := f.encodeLocked()
	f.data.Name = "new"
	newB, newSeq, _ := f.encodeLocked()
	f.mu.Unlock()

	if err := f.writeOut(newB, newSeq); err != nil {
		t.Fatal(err)
	}
	if err := f.writeOut(oldB, oldSeq); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, `"new"`) {
		t.Fatalf("stale snapshot won: %s", got)
	}
}

// The exact window of the bug: the timer goroutine has taken its snapshot
// (the value is no longer "dirty") but has not reached the disk yet. Close must
// still wait for that write; it used to return with the old file content.
func TestCloseWaitsForASnapshotThatHasNotReachedTheDiskYet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	f, err := Open(path, Config[doc]{Debounce: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	inWindow := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	f.afterSnapshot = func() {
		once.Do(func() { close(inWindow) })
		<-release
	}
	f.Mutate(func(d *doc) { d.Name = "captured" })
	<-inWindow // snapshot taken, write not started

	closed := make(chan error, 1)
	go func() { closed <- f.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned before the captured snapshot was written")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); !strings.Contains(got, `"captured"`) {
		t.Fatalf("snapshot missing after Close: %q", got)
	}
}
