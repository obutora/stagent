package install

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// readOptional returns the file's content, or nil (no error) when it does
// not exist.
func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fileSHA256 hashes a file; "" when it cannot be read.
func fileSHA256(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return sha256Hex(b)
}

// resolveTarget follows a symlinked config file (dotfile managers) so an
// atomic replace rewrites the target instead of replacing the link.
func resolveTarget(path string) string {
	if st, err := os.Lstat(path); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			return r
		}
	}
	return path
}

// atomicWrite replaces path with data through a temporary file in the same
// directory and a rename. An existing file keeps its permission bits; a new
// one gets mode. Missing parent directories are created owner-only.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	path = resolveTarget(path)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// missingDirs lists dir and those of its ancestors that do not exist yet:
// the directories a write into dir creates.
func missingDirs(dir string) []string {
	var out []string
	for !exists(dir) {
		out = append(out, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return out
}

// removeEmptyDirs deletes dirs that are empty, deepest first; a directory
// that holds anything stays.
func removeEmptyDirs(dirs []string) {
	dirs = slices.Clone(dirs)
	slices.SortFunc(dirs, func(a, b string) int { return len(b) - len(a) })
	for _, d := range dirs {
		os.Remove(d)
	}
}

// backupName is `<file>.sshterm-bak-<UTC timestamp>`, made unique if a backup
// of the same second already exists.
func backupName(path string, now time.Time) string {
	base := path + ".sshterm-bak-" + now.UTC().Format("20060102T150405Z")
	name := base
	for i := 1; exists(name); i++ {
		name = base + "-" + strconv.Itoa(i)
	}
	return name
}
