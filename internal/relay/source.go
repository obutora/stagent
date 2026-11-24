package relay

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/task"
)

// Checkout is the git state of a relay source's cwd (wire.RelaySource's
// git members).
type Checkout struct {
	// Root is the worktree cwd is in, canonical.
	Root      string
	Repo      string
	CwdRel    string
	Remote    string
	RemoteURL string
	Branch    string
	Head      string
	Base      string
	// Unpushed is how many commits of HEAD no remote-tracking ref has.
	Unpushed int
}

// git runs git in dir within the relay's per-command limit.
func git(ctx context.Context, env []string, dir string, args ...string) (string, error) {
	ctx, cancel := task.WithLimit(ctx, env, task.LimitRelayGit)
	defer cancel()
	out, err := task.Git(ctx, env, dir, args...)
	return strings.TrimSpace(string(out)), err
}

// Inspect reads the git state of cwd; nil when cwd is in no git checkout
// (a bare repository included). git not found is an error.
func Inspect(ctx context.Context, env []string, cwd string) (*Checkout, error) {
	top, err := git(ctx, env, cwd, "rev-parse", "--show-toplevel")
	if errors.Is(err, exec.ErrNotFound) {
		return nil, err
	}
	if err != nil || top == "" {
		return nil, nil
	}
	c := &Checkout{Root: task.CanonicalPath(filepath.FromSlash(top))}
	c.Repo = c.Root
	if ws, err := task.ListWorktrees(ctx, env, c.Root); err == nil && len(ws) > 0 {
		// A bare main repository has no .git of its own: the worktree is
		// the checkout then.
		if main := ws[0].Path; task.SamePath(main, c.Root) || exists(filepath.Join(main, ".git")) {
			c.Repo = main
		}
	}
	if rel, err := filepath.Rel(c.Root, task.CanonicalPath(cwd)); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		c.CwdRel = filepath.ToSlash(rel)
	}
	if c.Branch, err = task.CurrentBranch(ctx, env, c.Root); err != nil {
		return nil, gitErr(err)
	}
	c.Head, _ = git(ctx, env, c.Root, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	c.Remote = pickRemote(ctx, env, c.Root, c.Branch)
	if c.Remote != "" {
		c.RemoteURL, _ = git(ctx, env, c.Root, "remote", "get-url", c.Remote)
	}
	if c.Head != "" {
		if err := c.readBase(ctx, env); err != nil {
			return nil, gitErr(err)
		}
	}
	return c, nil
}

// pickRemote is the branch's upstream remote, else origin, else the first
// remote; "" without one.
func pickRemote(ctx context.Context, env []string, root, branch string) string {
	out, _ := git(ctx, env, root, "remote")
	remotes := strings.Fields(out)
	if branch != "" {
		if r, _ := git(ctx, env, root, "config", "--get", "branch."+branch+".remote"); r != "" && slices.Contains(remotes, r) {
			return r
		}
	}
	if slices.Contains(remotes, "origin") {
		return "origin"
	}
	if len(remotes) > 0 {
		return remotes[0]
	}
	return ""
}

// readBase counts the commits of HEAD no remote-tracking ref has and finds
// the newest one a ref has (Base).
func (c *Checkout) readBase(ctx context.Context, env []string) error {
	n, err := git(ctx, env, c.Root, "rev-list", "--count", "HEAD", "--not", "--remotes")
	if err != nil {
		return err
	}
	c.Unpushed, _ = strconv.Atoi(n)
	if c.Unpushed == 0 {
		c.Base = c.Head
		return nil
	}
	if ref, _ := git(ctx, env, c.Root, "for-each-ref", "--count=1", "--format=%(refname)", "refs/remotes"); ref == "" {
		return nil
	}
	out, err := git(ctx, env, c.Root, "rev-list", "--boundary", "HEAD", "--not", "--remotes")
	if err != nil {
		return err
	}
	var bounds []string
	for line := range strings.Lines(out) {
		if b, ok := strings.CutPrefix(strings.TrimSpace(line), "-"); ok {
			bounds = append(bounds, b)
		}
	}
	if len(bounds) == 0 {
		return nil
	}
	newest, err := git(ctx, env, c.Root, append([]string{"rev-list", "-1", "--no-walk"}, bounds...)...)
	if err != nil {
		return err
	}
	c.Base = newest
	return nil
}

// Bundle writes the commits of HEAD no remote has to dest (`git bundle
// create dest <branch or HEAD> --not --remotes`); false when there are
// none.
func (c *Checkout) Bundle(ctx context.Context, env []string, dest string) (bool, error) {
	if c.Head == "" || c.Unpushed == 0 {
		return false, nil
	}
	ctx, cancel := task.WithLimit(ctx, env, task.LimitRelayBundle)
	defer cancel()
	ref := cmp.Or(c.Branch, "HEAD")
	if _, err := task.Git(ctx, env, c.Root, "bundle", "create", "--quiet", dest, ref, "--not", "--remotes"); err != nil {
		os.Remove(dest)
		return false, gitErr(err)
	}
	if chmodOK {
		os.Chmod(dest, 0o600)
	}
	return true, nil
}

// diffArgs are the options of the patches.
var diffArgs = []string{"-c", "core.quotepath=off", "diff", "--binary", "--no-ext-diff", "--no-color", "--ignore-submodules=all"}

// Diff writes the staged and the unstaged changes to the patches staged
// and unstaged, each left out when empty, and answers their bytes.
func (c *Checkout) Diff(ctx context.Context, env []string, staged, unstaged string) (int64, error) {
	var total int64
	for _, p := range []struct {
		path string
		args []string
	}{
		{staged, append(slices.Clone(diffArgs), "--cached")},
		{unstaged, diffArgs},
	} {
		out, err := gitRaw(ctx, env, c.Root, p.args...)
		if err != nil {
			return 0, gitErr(err)
		}
		if len(out) == 0 {
			continue
		}
		if err := writePrivate(p.path, out); err != nil {
			return 0, err
		}
		total += int64(len(out))
	}
	return total, nil
}

// gitRaw is git's untrimmed output.
func gitRaw(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
	ctx, cancel := task.WithLimit(ctx, env, task.LimitRelayGit)
	defer cancel()
	return task.Git(ctx, env, dir, args...)
}

// UntrackedFile is a carried untracked file.
type UntrackedFile struct {
	Path string // "/" separated, relative to the worktree's root
	Size int64
}

// Untracked is what Untracked carried and skipped.
type Untracked struct {
	Files   []UntrackedFile
	Bytes   int64
	Skipped []SkippedEntry
}

// Untracked copies every untracked file of the worktree (`git ls-files
// --others --exclude-standard`) to dest at the same path: regular files
// owner-only with their executable bit, symbolic links as links. Sockets,
// FIFOs, devices and nested git checkouts are skipped. progress is told
// the bytes copied.
func (c *Checkout) Untracked(ctx context.Context, env []string, dest string, progress func(done int64)) (*Untracked, error) {
	out, err := gitRaw(ctx, env, c.Root, "-c", "core.quotepath=off", "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, gitErr(err)
	}
	u := &Untracked{}
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasSuffix(rel, "/") {
			u.Skipped = append(u.Skipped, SkippedEntry{Path: strings.TrimSuffix(rel, "/"), Why: "nested git checkout"})
			continue
		}
		src := filepath.Join(c.Root, filepath.FromSlash(rel))
		st, err := os.Lstat(src)
		if err != nil {
			continue // gone meanwhile
		}
		to := filepath.Join(dest, filepath.FromSlash(rel))
		switch m := st.Mode(); {
		case m.IsRegular():
			if err := MkdirPrivate(filepath.Dir(to)); err != nil {
				return nil, err
			}
			if err := copyPrivate(src, to, m&0o111 != 0); err != nil {
				return nil, err
			}
			u.Files = append(u.Files, UntrackedFile{Path: rel, Size: st.Size()})
			u.Bytes += st.Size()
			if progress != nil {
				progress(u.Bytes)
			}
		case m&fs.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return nil, err
			}
			if err := MkdirPrivate(filepath.Dir(to)); err != nil {
				return nil, err
			}
			if err := os.Symlink(target, to); err != nil {
				// Windows without the privilege to make links.
				u.Skipped = append(u.Skipped, SkippedEntry{Path: rel, Why: "symbolic link this host cannot copy"})
				continue
			}
			u.Files = append(u.Files, UntrackedFile{Path: rel})
		default:
			u.Skipped = append(u.Skipped, SkippedEntry{Path: rel, Why: kindOf(m)})
		}
	}
	return u, nil
}

func kindOf(m fs.FileMode) string {
	switch {
	case m&fs.ModeSocket != 0:
		return "socket"
	case m&fs.ModeNamedPipe != 0:
		return "fifo"
	case m&fs.ModeDevice != 0, m&fs.ModeCharDevice != 0:
		return "device"
	}
	return "not a regular file"
}

// copyPrivate copies the regular file from to to, owner-only, keeping its
// executable bit (0700).
func copyPrivate(from, to string, exe bool) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	mode := fs.FileMode(0o600)
	if exe {
		mode = 0o700
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if chmodOK {
		return os.Chmod(to, mode)
	}
	return nil
}

// TreeState is what the document tells of the working tree.
func (c *Checkout) TreeState(ctx context.Context, env []string, bundleCommits int, staged, unstaged int64, u *Untracked) *TreeState {
	ts := &TreeState{BundleCommits: bundleCommits, StagedPatchBytes: staged, UnstagedPatchBytes: unstaged}
	if c.Head != "" {
		args := []string{"log", "--oneline", "--no-decorate", "-n", "51", c.Head}
		if c.Base != "" {
			args = append(args, "^"+c.Base)
		}
		if c.Base != c.Head {
			out, _ := git(ctx, env, c.Root, args...)
			ts.Commits = nonEmptyLines(out)
			if len(ts.Commits) > 50 {
				ts.Commits = ts.Commits[:50]
				ts.CommitsMore = max(c.Unpushed-50, 1)
			}
		}
	}
	if out, err := gitRaw(ctx, env, c.Root, "-c", "core.quotepath=off", "status", "--porcelain=v1"); err == nil {
		ts.Status = nonEmptyLines(string(out))
		if len(ts.Status) > 200 {
			ts.StatusMore = len(ts.Status) - 200
			ts.Status = ts.Status[:200]
		}
	}
	if u != nil {
		for _, f := range u.Files {
			ts.Untracked = append(ts.Untracked, f.Path)
		}
		ts.Skipped = u.Skipped
	}
	if exists(filepath.Join(c.Root, ".gitmodules")) {
		out, _ := git(ctx, env, c.Root, "submodule", "status")
		ts.Submodules = nonEmptyLines(out)
	}
	if b, err := os.ReadFile(filepath.Join(c.Root, ".gitattributes")); err == nil && strings.Contains(string(b), "filter=lfs") {
		ts.LFS = true
	}
	return ts
}

func nonEmptyLines(s string) []string {
	var out []string
	for line := range strings.Lines(s) {
		if line = strings.TrimRight(line, "\r\n"); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TooLarge is a package over wire.RelayPackageMax: the bundle's bytes and
// the largest untracked files, for the user to commit, ignore or remove.
func TooLarge(total, bundle int64, u *Untracked) error {
	msg := fmt.Sprintf("the package would be %s, over %s: bundle %s", size(total), size(maxPackage), size(bundle))
	if u != nil && len(u.Files) > 0 {
		files := slices.Clone(u.Files)
		slices.SortFunc(files, func(a, b UntrackedFile) int { return cmp.Compare(b.Size, a.Size) })
		files = files[:min(5, len(files))]
		parts := make([]string, len(files))
		for i, f := range files {
			parts[i] = fmt.Sprintf("%s (%s)", f.Path, size(f.Size))
		}
		msg += "; largest untracked files: " + strings.Join(parts, ", ")
	}
	return errors.New(msg)
}

// size is n bytes for people.
func size(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
