package worktree

import (
	"cmp"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// Sizes measures inv's listed source checkouts, worktrees whose directory
// is there and folders (Measure, each of them excluded from the others),
// at most parallel at once, until ctx ends. Total adds the worktrees and
// folders.
func Sizes(ctx context.Context, inv *Inventory) wire.WorktreeSizesResult {
	type root struct {
		path      string
		countable bool // not a source checkout
	}
	var roots []root
	for _, r := range inv.Repos {
		if !r.listed() {
			continue
		}
		roots = append(roots, root{r.Path, false})
		for _, w := range r.Worktrees {
			if !w.Missing {
				roots = append(roots, root{w.Path, true})
			}
		}
	}
	for _, f := range inv.Folders {
		roots = append(roots, root{f.Path, true})
	}
	exclude := inv.Exclude()
	res := wire.WorktreeSizesResult{Entries: make([]wire.WorktreeSize, len(roots))}
	each(len(roots), func(i int) {
		bytes, mod := Measure(ctx, roots[i].path, exclude)
		res.Entries[i] = wire.WorktreeSize{Path: roots[i].path, Bytes: bytes, ModifiedAt: mod}
	})
	for i, r := range roots {
		if r.countable {
			res.Total += res.Entries[i].Bytes
		}
	}
	return res
}

// Exclude is what Measure and Top skip below the directory they measure:
// the source checkouts, worktrees and folders of inv.
func (inv *Inventory) Exclude() map[string]bool {
	exclude := map[string]bool{}
	for _, r := range inv.Repos {
		exclude[keyOf(r.Path)] = true
		for _, w := range r.Worktrees {
			exclude[keyOf(w.Path)] = true
		}
	}
	for _, f := range inv.Folders {
		exclude[keyOf(f.Path)] = true
	}
	return exclude
}

// Measure is the disk usage of dir (diskUsage: a file linked twice once)
// and the newest modification time in it, UTC to the second (zero when
// nothing could be read). Links (symbolic links, junctions) are counted
// as themselves, never followed; directories below dir in exclude (see
// Inventory.Exclude) are not counted; what cannot be read is skipped. The
// walk stops when ctx ends.
func Measure(ctx context.Context, dir string, exclude map[string]bool) (int64, time.Time) {
	var total int64
	var newest time.Time
	seen := map[fileID]bool{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return fs.SkipAll
		}
		if err != nil {
			if d != nil && d.IsDir() && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if p != dir && d.IsDir() && exclude[keyOf(p)] {
			return fs.SkipDir
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		n, id, shared := diskUsage(fi)
		if !shared || !seen[id] {
			total += n
			if shared {
				seen[id] = true
			}
		}
		if m := fi.ModTime(); m.After(newest) {
			newest = m
		}
		if p != dir && d.IsDir() && task.IsLink(fi) {
			return fs.SkipDir // a junction
		}
		return nil
	})
	if !newest.IsZero() {
		newest = newest.UTC().Truncate(time.Second)
	}
	return total, newest
}

// Top lists the entries directly in dir by disk usage (Measure), largest
// first (then by name), at most n.
func Top(ctx context.Context, dir string, n int, exclude map[string]bool) []wire.WorktreeFolderTop {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	top := make([]wire.WorktreeFolderTop, 0, len(ents))
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		t := wire.WorktreeFolderTop{Name: e.Name()}
		p := filepath.Join(dir, e.Name())
		switch {
		case task.IsLink(fi):
			t.Bytes, _, _ = diskUsage(fi)
		case e.IsDir():
			t.Dir = true
			if !exclude[keyOf(p)] {
				t.Bytes, _ = Measure(ctx, p, exclude)
			}
		default:
			t.Bytes, _, _ = diskUsage(fi)
		}
		top = append(top, t)
	}
	slices.SortFunc(top, func(a, b wire.WorktreeFolderTop) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), strings.Compare(a.Name, b.Name))
	})
	return top[:min(n, len(top))]
}

// RemoveFolder removes dir and everything in it. When that fails it makes
// what is read-only in it writable (Windows refuses to delete a read-only
// file; Unix, the entries of a directory without write permission),
// following no link, and tries again.
func RemoveFolder(dir string) error {
	if os.RemoveAll(dir) == nil {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if task.IsLink(fi) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		mode := fi.Mode().Perm()
		want := mode | 0o200
		if d.IsDir() {
			want |= 0o700
		}
		if want != mode {
			os.Chmod(p, want)
		}
		return nil
	})
	return os.RemoveAll(dir)
}
