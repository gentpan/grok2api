// Package jsonstore keeps one small value in a JSON file: loaded once, mutated
// in memory under a lock, and written back atomically (temp file, fsync,
// rename) either immediately or through a debounce timer.
//
// It replaces the hand-rolled "mutex + temp file + rename" stores of the HTTP
// layer (API keys, panel account, custom models, usage counters). It has no
// dependency on the rest of the project beyond internal/platform.
package jsonstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gentpan/grok2api/internal/platform"
)

// Config tunes a File. The zero value is valid: 0600 permissions, compact
// JSON, synchronous saves.
type Config[T any] struct {
	// Perm is the file mode of the persisted file (default 0600).
	Perm os.FileMode
	// Indent, when non-empty, pretty-prints with that indent string.
	Indent string
	// Debounce delays the write triggered by Mutate; the first Mutate arms the
	// timer and later ones within the window ride along. 0 saves synchronously.
	Debounce time.Duration
	// OnError receives write failures that have no caller to return to (the
	// debounce timer, Mutate).
	OnError func(error)
	// Normalize runs after a successful load to repair/default fields.
	Normalize func(*T)
}

// File is a JSON document bound to a path. All methods are safe for
// concurrent use.
type File[T any] struct {
	mu    sync.Mutex
	path  string
	cfg   Config[T]
	data  T
	dirty bool
	timer *time.Timer
	seq   uint64 // snapshots taken (guarded by mu)

	// wmu serialises disk writes. Debounced and flushed saves take their
	// snapshot under mu and write it under wmu only, so a slow fsync never
	// stalls readers/mutators of the in-memory value. written is the newest
	// snapshot on disk (guarded by wmu): an older snapshot that loses the race
	// for wmu is dropped instead of overwriting a newer one.
	wmu     sync.Mutex
	written uint64

	// pending counts snapshots that were taken (dirty cleared) but are not on
	// disk yet; guarded by mu. Flush waits on idle until it is zero, which is
	// what makes Close mean "everything mutated so far is persisted" even for a
	// timer write that has a snapshot but has not reached the disk lock yet.
	pending int
	idle    *sync.Cond

	beforeWrite   func() // test hook, called with wmu held just before a write
	afterSnapshot func() // test hook, called after a snapshot, before its write
}

// Open loads path into a File. An empty path yields a memory-only File that
// never touches disk. A missing file is not an error (the value
// starts as T's zero value). A file that exists but cannot be read or decoded
// yields a usable File over the zero value together with the error, so
// callers can log it and carry on; the next save replaces the bad file.
func Open[T any](path string, cfg Config[T]) (*File[T], error) {
	if cfg.Perm == 0 {
		cfg.Perm = 0o600
	}
	f := &File[T]{path: path, cfg: cfg}
	f.idle = sync.NewCond(&f.mu)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		var v T
		if uerr := json.Unmarshal(b, &v); uerr != nil {
			err = fmt.Errorf("decode %s: %w", path, uerr)
		} else {
			f.data = v
			err = nil
		}
	case errors.Is(err, os.ErrNotExist):
		err = nil
	}
	if cfg.Normalize != nil {
		cfg.Normalize(&f.data)
	}
	return f, err
}

// Path returns the backing file path.
func (f *File[T]) Path() string { return f.path }

// View runs fn with the current value under the lock. fn must not retain or
// mutate the value; copy what it needs.
func (f *File[T]) View(fn func(*T)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.data)
}

// Update runs fn under the lock and, when fn returns nil, writes the result
// to disk before returning. If fn or the write fails the in-memory value is
// restored, so a failed update leaves no trace.
func (f *File[T]) Update(fn func(*T) error) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, err := json.Marshal(&f.data)
	if err != nil {
		return err
	}
	if err := fn(&f.data); err != nil {
		f.restoreLocked(snap)
		return err
	}
	if err := f.saveLocked(); err != nil {
		f.restoreLocked(snap)
		return err
	}
	return nil
}

// Mutate runs fn under the lock and schedules a (debounced) save. Write
// failures are reported to Config.OnError.
func (f *File[T]) Mutate(fn func(*T)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.data)
	f.dirty = true
	if f.cfg.Debounce <= 0 {
		f.report(f.saveLocked())
		return
	}
	if f.timer == nil {
		f.timer = time.AfterFunc(f.cfg.Debounce, f.onTimer)
	}
}

func (f *File[T]) onTimer() {
	f.mu.Lock()
	f.timer = nil
	b, seq, ok := f.snapshotDirtyLocked()
	f.mu.Unlock()
	if !ok {
		return
	}
	f.persistSnapshot(b, seq)
}

// persistSnapshot writes a snapshot taken by snapshotDirtyLocked and settles
// the in-flight count (re-arming dirty and reporting when the write failed).
func (f *File[T]) persistSnapshot(b []byte, seq uint64) error {
	if f.afterSnapshot != nil {
		f.afterSnapshot()
	}
	err := f.writeOut(b, seq)
	f.mu.Lock()
	if err != nil {
		f.dirty = true
	}
	f.pending--
	f.idle.Broadcast()
	f.mu.Unlock()
	if err != nil {
		f.report(err)
	}
	return err
}

// Flush writes pending Mutate changes now and returns once every snapshot that
// was already in flight (for instance from the debounce timer) is on disk too.
func (f *File[T]) Flush() error {
	f.mu.Lock()
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	b, seq, ok := f.snapshotDirtyLocked()
	f.mu.Unlock()

	var err error
	if ok {
		// persistSnapshot reports through OnError; Flush also returns it.
		err = f.persistSnapshot(b, seq)
	}
	f.mu.Lock()
	for f.pending > 0 {
		f.idle.Wait()
	}
	f.mu.Unlock()
	return err
}

// snapshotDirtyLocked encodes the value when there are unsaved Mutate changes
// and marks it clean. ok is false when there is nothing to write (or the value
// cannot be encoded, which is reported).
func (f *File[T]) snapshotDirtyLocked() (b []byte, seq uint64, ok bool) {
	if !f.dirty {
		return nil, 0, false
	}
	if f.path == "" {
		f.dirty = false
		return nil, 0, false
	}
	b, seq, err := f.encodeLocked()
	if err != nil {
		f.report(err)
		return nil, 0, false
	}
	f.dirty = false
	f.pending++
	return b, seq, true
}

// Close flushes pending changes and disarms the timer. The File stays usable;
// later Mutate calls simply schedule a new timer.
func (f *File[T]) Close() error { return f.Flush() }

func (f *File[T]) report(err error) {
	if err != nil && f.cfg.OnError != nil {
		f.cfg.OnError(err)
	}
}

func (f *File[T]) restoreLocked(snap []byte) {
	var v T
	if json.Unmarshal(snap, &v) == nil {
		f.data = v
	}
}

// encodeLocked marshals the current value and numbers the snapshot.
func (f *File[T]) encodeLocked() ([]byte, uint64, error) {
	var (
		b   []byte
		err error
	)
	if f.cfg.Indent != "" {
		b, err = json.MarshalIndent(&f.data, "", f.cfg.Indent)
	} else {
		b, err = json.Marshal(&f.data)
	}
	if err != nil {
		return nil, 0, err
	}
	f.seq++
	return b, f.seq, nil
}

// writeOut persists a snapshot unless a newer one is already on disk. It must
// not be called with mu held by a path that other goroutines wait on for long;
// the synchronous paths (saveLocked) call it under mu on purpose.
func (f *File[T]) writeOut(b []byte, seq uint64) error {
	f.wmu.Lock()
	defer f.wmu.Unlock()
	if seq <= f.written {
		return nil
	}
	if f.beforeWrite != nil {
		f.beforeWrite()
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
		return err
	}
	if err := platform.AtomicWriteBytes(f.path, f.cfg.Perm, b); err != nil {
		return err
	}
	f.written = seq
	return nil
}

// saveLocked writes synchronously under mu: used by Update (the caller needs
// the result and rollback needs exclusion) and by Mutate without a debounce.
func (f *File[T]) saveLocked() error {
	if f.path == "" {
		// Memory-only store (tests, unconfigured data dir): nothing to write.
		f.dirty = false
		return nil
	}
	b, seq, err := f.encodeLocked()
	if err != nil {
		return err
	}
	if err := f.writeOut(b, seq); err != nil {
		return err
	}
	f.dirty = false
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
	return nil
}
