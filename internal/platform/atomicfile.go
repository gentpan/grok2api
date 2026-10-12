package platform

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
)

// AtomicWriteFile replaces path with whatever write streams into it.
//
// The content goes to a same-directory temp file (created with perm) which is
// fsynced and renamed over path, then the directory is fsynced on a best-effort
// basis. A failure at any point leaves the previous file untouched and removes
// the temp file, so readers never observe a truncated file.
func AtomicWriteFile(path string, perm os.FileMode, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(perm); err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 256*1024)
	if err = write(bw); err != nil {
		return err
	}
	if err = bw.Flush(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	closed = true
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync() // best effort: not supported on every platform
		_ = d.Close()
	}
	return nil
}

// AtomicWriteBytes is AtomicWriteFile for an in-memory payload.
func AtomicWriteBytes(path string, perm os.FileMode, data []byte) error {
	return AtomicWriteFile(path, perm, func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
}
