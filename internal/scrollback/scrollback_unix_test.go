//go:build !windows

package scrollback

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSegmentSymlinkIsNotFollowed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sess")
	s, err := open(dir, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.segPath(0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte("output")); err == nil {
		t.Fatal("wrote a segment through a symlink")
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep me" {
		t.Errorf("symlink target now %q (%v), want unchanged", b, err)
	}
}
