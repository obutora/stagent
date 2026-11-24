package task

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/fsutil"
)

// RelayPrep is what a relay target's task (`stagent task run --relay`)
// takes from the relay: where the package's working tree parts are and
// what its branch is made from (PROTOCOL.md "Relay"). relay.start writes
// it to RelayPrepFile in the relay's directory.
type RelayPrep struct {
	ID string `json:"relay_id"`
	// Tree holds the working tree parts: RelayBundle, RelayStaged,
	// RelayUnstaged and RelayUntracked/ (each absent when empty).
	Tree string `json:"tree"`
	// Remote is the checkout's remote whose URL is the package's (""
	// none): fetched first, for the commits the bundle builds on.
	Remote string `json:"remote,omitempty"`
	// Head is the source's HEAD commit, the branch's tip.
	Head string `json:"head"`
	// CwdRel is where in the worktree the agent starts ("/" separated).
	CwdRel string `json:"cwd_rel,omitempty"`

	// dir is the relay's directory (LoadRelayPrep).
	dir string
}

// The working tree parts of a relay, in RelayPrep.Tree.
const (
	RelayBundle    = "repo.bundle"
	RelayStaged    = "staged.patch"
	RelayUnstaged  = "unstaged.patch"
	RelayUntracked = "untracked"
	// RelayPrepFile is RelayPrep in the relay's directory.
	RelayPrepFile = "task.json"
	// relayApplied marks, in the relay's directory, the relay stage done.
	relayApplied = "applied"
)

// RelayRef is the ref a relay's bundle is fetched to until its branch is
// made.
func RelayRef(id string) string { return "refs/stagent-relay/" + id }

// SaveRelayPrep writes p to dir/RelayPrepFile.
func SaveRelayPrep(dir string, p RelayPrep) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(dir, RelayPrepFile), b)
}

// LoadRelayPrep reads dir/RelayPrepFile.
func LoadRelayPrep(dir string) (*RelayPrep, error) {
	b, err := os.ReadFile(filepath.Join(dir, RelayPrepFile))
	if err != nil {
		return nil, err
	}
	var p RelayPrep
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", RelayPrepFile, err)
	}
	p.dir = dir
	return &p, nil
}

// AgentDir is where in worktree the relay's agent starts: CwdRel, or the
// worktree's root when that is gone.
func (p *RelayPrep) AgentDir(worktree string) string {
	if p.CwdRel != "" {
		if d := filepath.Join(worktree, filepath.FromSlash(p.CwdRel)); IsDir(d) {
			return d
		}
	}
	return worktree
}

func (p *RelayPrep) part(name string) (string, bool) {
	path := filepath.Join(p.Tree, name)
	_, err := os.Lstat(path)
	return path, err == nil
}

// relayFetch fetches the remote whose URL matched, for the commits the
// bundle builds on. Failing to is only a line: the checkout may have them.
func (r *prep) relayFetch(ctx context.Context) error {
	if r.Relay.Remote == "" {
		r.logf("no remote of this checkout is the source's; nothing to fetch")
		return nil
	}
	err := r.runLimited(ctx, limitNetwork, r.Task.Repo, "git", "fetch", r.Relay.Remote)
	if err != nil {
		r.logf("fetch failed (%v); continuing with what the checkout has", err)
	}
	return nil
}

// relayBranch makes the task's branch at the source's HEAD: from the
// bundle when there is one (fetched to RelayRef, removed once the branch
// is made), else from the commits the checkout has. A branch there already
// (prepared again) is kept.
func (r *prep) relayBranch(ctx context.Context) error {
	t, rp := r.Task, r.Relay
	if r.has(ctx, "refs/heads/"+t.Branch) {
		r.logf("%s exists already", t.Branch)
		return nil
	}
	bundle, ok := rp.part(RelayBundle)
	if !ok {
		if !r.has(ctx, rp.Head) {
			return fmt.Errorf("the source's HEAD %s is not in this checkout (not pushed, or not fetched)", rp.Head)
		}
		return r.run(ctx, t.Repo, "git", "branch", "--no-track", t.Branch, rp.Head)
	}
	prereqs, ref, err := bundleHeader(bundle)
	if err != nil {
		return err
	}
	var missing []string
	for _, c := range prereqs {
		if !r.has(ctx, c) {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the bundle builds on commits this checkout lacks: %s", strings.Join(missing, ", "))
	}
	if err := r.run(ctx, t.Repo, "git", "bundle", "verify", bundle); err != nil {
		return err
	}
	tmp := RelayRef(rp.ID)
	if err := r.run(ctx, t.Repo, "git", "fetch", bundle, "+"+ref+":"+tmp); err != nil {
		return err
	}
	if err := r.run(ctx, t.Repo, "git", "branch", "--no-track", t.Branch, tmp); err != nil {
		return err
	}
	return r.run(ctx, t.Repo, "git", "update-ref", "-d", tmp)
}

// bundleHeader reads a git bundle's prerequisite commits and its first
// ref.
func bundleHeader(path string) (prereqs []string, ref string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	br := bufio.NewReader(f)
	first, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "# v") || !strings.Contains(first, "git bundle") {
		return nil, "", errors.New("the bundle is no git bundle")
	}
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if ref == "" {
				return nil, "", errors.New("the bundle names no ref")
			}
			return prereqs, ref, nil
		case strings.HasPrefix(line, "@"): // v3 capability
		case strings.HasPrefix(line, "-"):
			c, _, _ := strings.Cut(line[1:], " ")
			prereqs = append(prereqs, c)
		case ref == "":
			_, name, _ := strings.Cut(line, " ")
			ref = name
		}
		if err != nil {
			return nil, "", errors.New("the bundle's header is cut off")
		}
	}
}

// relayApply applies the package's uncommitted changes in the worktree —
// the staged patch to the index and the files, the unstaged one to the
// files, each tried again once with --3way — and writes its untracked
// files, refusing to replace one there. Once done it is not done again.
func (r *prep) relayApply(ctx context.Context) error {
	t, rp := r.Task, r.Relay
	marker := filepath.Join(rp.dir, relayApplied)
	if _, err := os.Stat(marker); err == nil {
		r.logf("the relay's changes are applied already")
		return nil
	}
	if p, ok := rp.part(RelayStaged); ok {
		if err := r.applyPatch(ctx, t.Worktree, p, "--index"); err != nil {
			return err
		}
	}
	if p, ok := rp.part(RelayUnstaged); ok {
		if err := r.applyPatch(ctx, t.Worktree, p); err != nil {
			return err
		}
	}
	if dir, ok := rp.part(RelayUntracked); ok {
		n, err := r.writeUntracked(dir, t.Worktree)
		if err != nil {
			return err
		}
		r.logf("%d untracked file(s) written", n)
	}
	return os.WriteFile(marker, nil, 0o600)
}

// applyPatch runs git apply --binary <flags> patch in dir, again with
// --3way when that fails.
func (r *prep) applyPatch(ctx context.Context, dir, patch string, flags ...string) error {
	args := append([]string{"apply", "--binary"}, flags...)
	err := r.run(ctx, dir, "git", append(args, patch)...)
	if err == nil {
		return nil
	}
	r.logf("%s did not apply (%v); trying again with --3way", filepath.Base(patch), err)
	return r.run(ctx, dir, "git", append(args, "--3way", patch)...)
}

// writeUntracked copies the files under src into dest at the same paths:
// regular files with their executable bit, symbolic links as links (as a
// file holding the target when no link can be made: Windows without the
// privilege). A path there already fails, naming it.
func (r *prep) writeUntracked(src, dest string) (int, error) {
	n := 0
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		to := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(to, 0o755)
		}
		if _, err := os.Lstat(to); err == nil {
			return fmt.Errorf("untracked file %s is in the worktree already", filepath.ToSlash(rel))
		}
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			return err
		}
		n++
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, to); err != nil {
				r.logf("cannot make the symbolic link %s (%v): written as a file holding its target", filepath.ToSlash(rel), err)
				return os.WriteFile(to, []byte(target), 0o644)
			}
			return nil
		}
		return copyFileMode(p, to)
	})
	return n, err
}

// copyFileMode copies the regular file from to to, keeping its executable
// bit.
func copyFileMode(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if st.Mode()&0o111 != 0 {
		mode = 0o755
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
