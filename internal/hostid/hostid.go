// Package hostid keeps the host's random identifier (host_id): 128 random
// bits as 32 lowercase hex chars, created the first time stagent needs it
// and never changed afterwards. It lives in its own file under Root, apart
// from config.json, so config.set and removing `notify` leave it alone. The
// app maps it to a saved connection to open notification links.
package hostid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Read returns the stored id, or "" when there is none (or it is invalid).
func Read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(b))
	if !Valid(id) {
		return ""
	}
	return id
}

// Ensure returns the stored id, creating it first if there is none. Racing
// processes agree on one id: the file appears complete or not at all
// (written aside, then linked into place without replacing).
func Ensure(path string) (string, error) {
	if id := Read(path); id != "" {
		return id, nil
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.WriteString(id + "\n")
	if err := errors.Join(werr, f.Close()); err != nil {
		return "", err
	}
	// A file that exists but holds no valid id is replaced; anything else
	// (another process won the race) is read back.
	if Read(path) == "" && fileExists(path) {
		if err := os.Rename(tmp, path); err != nil {
			return "", err
		}
		return id, nil
	}
	if err := os.Link(tmp, path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if got := Read(path); got != "" {
		return got, nil
	}
	return "", fmt.Errorf("host id: %s holds no valid id", path)
}

// Valid reports whether id has the form Ensure creates.
func Valid(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
