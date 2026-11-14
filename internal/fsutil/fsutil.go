// Package fsutil holds the file helpers the daemon's state files share.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes b to path via a temporary file in the same
// directory and a rename, mode 0600, creating missing parent directories
// owner-only. A reader sees the old content or the new, never a part.
func WriteFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
