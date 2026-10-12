package platform

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteBytesCreatesPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := AtomicWriteBytes(path, 0o600, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("content = %q, err = %v", got, err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o, want 600", st.Mode().Perm())
	}
}

func TestAtomicWriteFileFailureKeepsOldContentAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := AtomicWriteBytes(path, 0o600, []byte("old")); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("encode failed")
	err := AtomicWriteFile(path, 0o600, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "old" {
		t.Fatalf("content = %q, want old", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

func TestAtomicWriteFileMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope", "state.json")
	if err := AtomicWriteBytes(path, 0o600, []byte("x")); err == nil {
		t.Fatal("expected error for missing directory")
	}
}
