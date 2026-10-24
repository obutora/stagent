package worktree

import (
	"context"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const mib = 1 << 20

// writeRandom writes n bytes that no file system compresses.
func writeRandom(t *testing.T, p string, n int) {
	t.Helper()
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(1, uint64(n)))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Sizes measures every source checkout, worktree and folder by disk
// usage: a worktree inside the source checkout (.claude/worktrees/x) is
// not counted in it, nor twice in the total; a symbolic link to a shared
// directory is not followed; a file linked twice counts once. Total is
// the worktrees' and folders' bytes, not the source checkout's. A missing
// worktree is not measured. modified_at is the newest mtime inside.
func TestSizes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic and hard links need privileges on Windows")
	}
	f := newFixture(t)
	app := f.repo(t, "app")
	nested := filepath.Join(app, ".claude", "worktrees", "x")
	f.worktree(t, app, nested, "x")
	writeRandom(t, filepath.Join(nested, "big.bin"), 4*mib)
	wt := filepath.Join(f.base, "wt-y")
	f.worktree(t, app, wt, "y")
	writeRandom(t, filepath.Join(wt, "data.bin"), 4*mib)
	if err := os.Link(filepath.Join(wt, "data.bin"), filepath.Join(wt, "data-again.bin")); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(f.base, "shared")
	writeRandom(t, filepath.Join(shared, "lib.bin"), 4*mib)
	if err := os.Symlink(shared, filepath.Join(wt, "node_modules")); err != nil {
		t.Fatal(err)
	}
	newest := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(wt, "data.bin"), newest, newest.Add(500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(f.base, "wt-gone")
	f.worktree(t, app, gone, "gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(f.base, "worktrees")
	notes := filepath.Join(folder, "notes")
	writeRandom(t, filepath.Join(notes, "dump.bin"), mib)

	inv, err := Discover(context.Background(), Sources{Env: f.env, Folder: folder, Repos: []string{app}})
	if err != nil {
		t.Fatal(err)
	}
	res := Sizes(context.Background(), inv)
	got := map[string]wire.WorktreeSize{}
	var paths []string
	for _, e := range res.Entries {
		got[e.Path] = e
		paths = append(paths, e.Path)
	}
	if want := []string{app, nested, wt, notes}; !slices.Equal(paths, want) {
		t.Fatalf("measured %q, want %q", paths, want)
	}
	if b := got[app].Bytes; b <= 0 || b >= 4*mib {
		t.Errorf("source checkout %d bytes: the nested worktree counted in it", b)
	}
	if b := got[nested].Bytes; b < 4*mib || b >= 5*mib {
		t.Errorf("nested worktree %d bytes", b)
	}
	if b := got[wt].Bytes; b < 4*mib || b >= 5*mib {
		t.Errorf("worktree %d bytes: a link followed or a hard link counted twice", b)
	}
	if b := got[notes].Bytes; b < mib || b >= 2*mib {
		t.Errorf("folder %d bytes", b)
	}
	if want := got[nested].Bytes + got[wt].Bytes + got[notes].Bytes; res.Total != want {
		t.Errorf("total %d, want %d", res.Total, want)
	}
	if m := got[wt].ModifiedAt; !m.Equal(newest) || m.Location() != time.UTC {
		t.Errorf("modified_at %v, want %v", m, newest)
	}
}

// A folder's dry run shows its largest entries first, at most n, a
// directory measured as a whole.
func TestTop(t *testing.T) {
	dir := t.TempDir()
	writeRandom(t, filepath.Join(dir, "node_modules", "a.bin"), 2*mib)
	writeRandom(t, filepath.Join(dir, "node_modules", "b.bin"), mib)
	writeRandom(t, filepath.Join(dir, "dump.bin"), 2*mib+mib/2)
	writeRandom(t, filepath.Join(dir, "notes.md"), 10)
	writeRandom(t, filepath.Join(dir, "small.txt"), 1)
	top := Top(context.Background(), dir, 2, nil)
	if len(top) != 2 || top[0].Name != "node_modules" || !top[0].Dir || top[1].Name != "dump.bin" || top[1].Dir {
		t.Fatalf("top %+v", top)
	}
	if top[0].Bytes < 3*mib || top[1].Bytes < 2*mib+mib/2 || top[1].Bytes >= 3*mib {
		t.Fatalf("top bytes %+v", top)
	}
	if b, _ := Measure(context.Background(), dir, nil); b < 5*mib+mib/2 {
		t.Fatalf("measure %d", b)
	}
}

// RemoveFolder removes a folder whose files and directories are read-only.
func TestRemoveFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "notes")
	writeRandom(t, filepath.Join(dir, "ro", "f.txt"), 10)
	if err := os.Chmod(filepath.Join(dir, "ro", "f.txt"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := RemoveFolder(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("folder still there: %v", err)
	}
}
