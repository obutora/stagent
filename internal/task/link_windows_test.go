package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A shared directory of a checkout on a network share: an administrator
// can make a junction there, but the share's server resolves it as its
// own path and it does not work. LinkDir then makes a directory symbolic
// link that works, or fails leaving nothing (copying takes over).
func TestLinkDirOnShare(t *testing.T) {
	local, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.VolumeName(local)
	if len(vol) != 2 || vol[1] != ':' {
		t.Skipf("temporary directory %s is not on a drive letter", local)
	}
	share := `\\localhost\` + vol[:1] + `$` + strings.TrimPrefix(local, vol)
	if _, err := os.Stat(share); err != nil {
		t.Skipf("administrative share unreachable: %v", err)
	}
	target := filepath.Join(share, "app", "node_modules")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "index.js"), []byte("js"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(share, "app-1", "node_modules")
	if err := os.Mkdir(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := LinkDir(target, link); err != nil {
		if _, lerr := os.Lstat(link); !errors.Is(lerr, os.ErrNotExist) {
			t.Fatalf("LinkDir failed (%v) and left %s: %v", err, link, lerr)
		}
		t.Logf("no link on the share (not elevated): %v", err)
		return
	}
	if b, err := os.ReadFile(filepath.Join(link, "index.js")); err != nil || string(b) != "js" {
		t.Fatalf("through the link: %q, %v", b, err)
	}
}
