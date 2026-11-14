package task

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Changes lists what removing worktree would lose, as `git status
// --porcelain` lines: tracked files changed (staged or not) and untracked
// files git does not ignore. Ignored files (a copied .env) are not
// changes, nor are the shared-directory links named in links (paths
// relative to worktree, see ShareLinks). The user's git settings
// (status.showUntrackedFiles, submodule ignores) do not hide any: each
// untracked file is a line, and a submodule with changes inside is one.
// git status takes no optional lock (NoLocks): an agent may be working
// in worktree. A git status over its time limit (limitCheck) is an error
// wrapping a *LimitError.
func Changes(ctx context.Context, env []string, worktree string, links []string) ([]string, error) {
	ctx, cancel := withLimit(ctx, env, limitCheck)
	defer cancel()
	out, err := Git(ctx, NoLocks(env), worktree, "status", "--porcelain", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	var lines []string
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		xy, path := f[:2], f[3:]
		line := xy + " " + path
		if xy[0] == 'R' || xy[0] == 'C' {
			// -z gives a rename's source as the next field.
			if i+1 < len(fields) {
				i++
				line = xy + " " + fields[i] + " -> " + path
			}
		}
		if xy == "??" && slices.Contains(links, strings.TrimSuffix(path, "/")) {
			continue
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// ShareLinks lists the shared directories (worktree.sharedDirectories of
// the source checkout's orca.yaml) that are links in worktree — symbolic
// links, or junctions on Windows — as slash-separated paths relative to
// it. An orca.yaml that cannot be read has none.
func ShareLinks(repo, worktree string) []string {
	o, err := LoadOrca(repo)
	if err != nil || o == nil {
		return nil
	}
	var links []string
	for _, e := range o.SharedDirectories {
		rel := filepath.Clean(filepath.FromSlash(e))
		if e == "" || filepath.IsAbs(rel) || filepath.VolumeName(rel) != "" || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(worktree, rel)); err == nil && IsLink(fi) {
			links = append(links, filepath.ToSlash(rel))
		}
	}
	return links
}

// Unpushed counts the commits of the checkout repo's local branch that no
// remote-tracking branch has (`git rev-list <branch> --not --remotes
// --count`); 0 when there is no such branch.
func Unpushed(ctx context.Context, env []string, repo, branch string) (int, error) {
	ref := "refs/heads/" + branch
	if branch == "" || !HasRef(ctx, env, repo, ref) {
		return 0, nil
	}
	out, err := Git(ctx, env, repo, "rev-list", ref, "--not", "--remotes", "--count")
	if err != nil {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscan(string(out), &n); err != nil {
		return 0, fmt.Errorf("git rev-list: unexpected answer %q", bytes.TrimSpace(out))
	}
	return n, nil
}

// Refusals lists why worktree (canonical), of the repository whose
// worktrees are ws (ListWorktrees), is not removed whatever force says
// (確認に並ぶ断る理由): the one place task.remove, worktree.remove and
// worktree.list tell them. In this order: the other worktrees of ws inside
// it whose directory is there (nested_worktrees, in git's order) — git
// worktree remove would take them with it, their changes included, when
// the outer worktree ignores them; its submodules (submodules, see
// Submodules), which git worktree remove refuses; a lock on it (locked).
// The lock of a killed git worktree add (HalfMade) is no refusal when
// ours — worktree is a task's, whose add only its own preparation runs,
// so that add is gone and 片付け takes the worktree — but is for another
// tool's worktree, whose add may still be at work.
func Refusals(ctx context.Context, env []string, ws []Worktree, worktree string, ours bool) ([]wire.RemoveRefusal, error) {
	var refusals []wire.RemoveRefusal
	var nested []string
	for _, w := range ws {
		if !SamePath(w.Path, worktree) && Within(w.Path, worktree) && IsDir(w.Path) {
			nested = append(nested, w.Path)
		}
	}
	if len(nested) > 0 {
		refusals = append(refusals, wire.RemoveRefusal{Kind: wire.RefusalNestedWorktrees, Paths: nested})
	}
	self := FindWorktree(ws, worktree)
	if self != nil && IsDir(worktree) {
		r, err := Submodules(ctx, env, worktree)
		if err != nil {
			return nil, err
		}
		if r != nil {
			refusals = append(refusals, *r)
		}
	}
	if self != nil && self.Locked && !(ours && self.HalfMade()) {
		refusals = append(refusals, wire.RemoveRefusal{Kind: wire.RefusalLocked})
	}
	return refusals, nil
}

// Submodules is the submodules refusal of worktree (an existing
// directory), nil when it has none: as git worktree remove decides, its
// git directory has modules, or a submodule of its index is checked out.
// Paths are the submodules of its index (canonical). It takes no lock.
func Submodules(ctx context.Context, env []string, worktree string) (*wire.RemoveRefusal, error) {
	env = NoLocks(env)
	out, err := Git(ctx, env, worktree, "rev-parse", "--path-format=absolute", "--git-path", "modules")
	if err != nil {
		return nil, err
	}
	modules := IsDir(strings.TrimSpace(string(out)))
	if out, err = Git(ctx, env, worktree, "ls-files", "-z", "--stage"); err != nil {
		return nil, err
	}
	var paths []string
	checkedOut := false
	for _, e := range strings.Split(string(out), "\x00") {
		// "<mode> <object> <stage>\t<path>"; 160000 is a gitlink.
		meta, path, ok := strings.Cut(e, "\t")
		if !ok || !strings.HasPrefix(meta, "160000 ") {
			continue
		}
		p := filepath.Join(worktree, filepath.FromSlash(path))
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			checkedOut = true
		}
		paths = append(paths, CanonicalPath(p))
	}
	if !modules && !checkedOut {
		return nil, nil
	}
	return &wire.RemoveRefusal{Kind: wire.RefusalSubmodules, Paths: paths}, nil
}

// NoLocks is env (nil: this process's) with git's optional locks off, so
// git status never writes the index of a worktree an agent works in.
func NoLocks(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return append(slices.Clip(env), "GIT_OPTIONAL_LOCKS=0")
}

// RefusedError is a removal refused for Refusals of Worktree.
type RefusedError struct {
	Worktree string
	Refusals []wire.RemoveRefusal
}

func (e *RefusedError) Error() string {
	var why []string
	for _, r := range e.Refusals {
		switch r.Kind {
		case wire.RefusalNestedWorktrees:
			why = append(why, "it holds other worktrees, remove them first: "+strings.Join(r.Paths, ", "))
		case wire.RefusalSubmodules:
			why = append(why, "it has submodules, which git worktree remove refuses: "+strings.Join(r.Paths, ", "))
		case wire.RefusalLocked:
			why = append(why, "it is locked, unlock it first (git worktree unlock)")
		default:
			why = append(why, strings.TrimSpace(r.Kind+" "+strings.Join(r.Paths, ", ")))
		}
	}
	return fmt.Sprintf("%s is not removed: %s", e.Worktree, strings.Join(why, "; "))
}

// Removal is 片付け of a worktree task once its sessions have stopped:
// what `stagent task remove` runs in a session of the source checkout, and
// `stagent task remove-worktree` for a worktree no task has (ADR 0006).
type Removal struct {
	Task wire.Task
	// Archive runs scripts.archive of the source checkout's orca.yaml
	// first, in the worktree.
	Archive bool
	// Force removes a worktree with changes (git worktree remove --force).
	Force bool
	// Env is the environment of git, gh and the archive script; nil: this
	// process's.
	Env []string
	// Stdin feeds the archive script (nil: none); Out, the terminal (nil:
	// nowhere), gets the stage headers, the commands run and their output.
	Stdin io.Reader
	Out   io.Writer
	// Stage is told each stage as it starts (task.progress).
	Stage func(stage string)
}

// Remove runs the stages of r's removal in order — archive (when asked),
// worktree (unlink the shared-directory links, then git worktree remove),
// branch — stopping at the first that fails (*StageError). A worktree
// with Refusals (ours when r.Task is a task, with an ID) is not touched:
// the worktree stage fails with a *RefusedError before archive. A task's
// worktree a killed `git worktree add` left half made has no archive
// (nothing was checked out). git's worktree list is read once, and again
// after an archive script ran: the script may have removed or unregistered
// the worktree itself. It returns the task's branch when it is kept (see
// removeBranch). Remote branches are never deleted.
func Remove(ctx context.Context, r Removal) (keptBranch string, err error) {
	t := r.Task
	ws, err := ListWorktrees(ctx, r.Env, t.Repo)
	if err == nil {
		var refusals []wire.RemoveRefusal
		if refusals, err = Refusals(ctx, r.Env, ws, t.Worktree, t.ID != ""); err == nil && len(refusals) > 0 {
			err = &RefusedError{Worktree: t.Worktree, Refusals: refusals}
		}
	}
	if err != nil {
		return "", &StageError{Stage: wire.TaskStageWorktree, Err: err}
	}
	w := FindWorktree(ws, t.Worktree)
	p := r.prep()
	if r.Archive {
		if w != nil && w.HalfMade() {
			p.logf("no archive: %s was left half made by a git worktree add that was stopped", t.Worktree)
		} else {
			p.announce(wire.TaskStageArchive)
			ran, err := p.archive(ctx)
			if err != nil {
				return "", &StageError{Stage: wire.TaskStageArchive, Err: err}
			}
			if ran {
				if w, err = WorktreeAt(ctx, r.Env, t.Repo, t.Worktree); err != nil {
					return "", &StageError{Stage: wire.TaskStageWorktree, Err: err}
				}
			}
		}
	}
	p.announce(wire.TaskStageWorktree)
	if err := p.removeWorktree(ctx, w, r.Force); err != nil {
		return "", &StageError{Stage: wire.TaskStageWorktree, Err: err}
	}
	p.announce(wire.TaskStageBranch)
	return p.removeBranch(ctx), nil
}

// RemoveMissing is 片付け of a missing task, what `stagent task remove
// --missing` runs: its own worktree is unregistered when git still lists
// it without its directory (best effort — the record goes anyway — and
// never the repository's other worktrees, ADR 0016), then the branch stage
// as in Remove. A directory that is there is not touched. It returns the
// branch when it is kept.
func RemoveMissing(ctx context.Context, r Removal) (keptBranch string) {
	p := r.prep()
	t := r.Task
	p.announce(wire.TaskStageWorktree)
	if IsDir(t.Worktree) {
		p.logf("%s is there: left as it is", t.Worktree)
	} else if w, _ := WorktreeAt(ctx, r.Env, t.Repo, t.Worktree); w != nil {
		if err := p.run(ctx, t.Repo, "git", UnregisterArgs(ctx, r.Env, t.Worktree)...); err != nil {
			p.logf("left registered: %v", err)
		}
	}
	p.announce(wire.TaskStageBranch)
	return p.removeBranch(ctx)
}

// RemoveEmptyParent removes the directory path was in when that is
// <folder>/<repo name> — directly in the worktree folder — and empty now
// but for macOS Finder's .DS_Store, and returns it ("" otherwise, and
// without folder).
func RemoveEmptyParent(folder, path string) string {
	parent := filepath.Dir(path)
	if folder == "" || !SamePath(filepath.Dir(parent), folder) {
		return ""
	}
	if ents, err := os.ReadDir(parent); err == nil && len(ents) == 1 && ents[0].Name() == ".DS_Store" && ents[0].Type().IsRegular() {
		os.Remove(filepath.Join(parent, ".DS_Store"))
	}
	if os.Remove(parent) != nil {
		return "" // not empty, or gone already
	}
	return parent
}

func (r Removal) prep() *prep {
	if r.Out == nil {
		r.Out = io.Discard
	}
	return &prep{Prep: Prep{Task: r.Task, Env: r.Env, Stdin: r.Stdin, Out: r.Out, Stage: r.Stage}}
}

// archive runs scripts.archive of the source checkout's orca.yaml in the
// worktree, the way setup runs, and reports whether there was one to run.
func (r *prep) archive(ctx context.Context) (ran bool, err error) {
	t := r.Task
	o, err := LoadOrca(t.Repo)
	if err != nil {
		return false, err
	}
	if o == nil || o.Archive == "" {
		r.logf("no scripts.archive in %s", filepath.Join(t.Repo, OrcaFile))
		return false, nil
	}
	err = RunScript(ctx, Script{Text: o.Archive, Dir: t.Worktree, Root: t.Repo, Env: r.Env, Stdin: r.Stdin, Stdout: r.Out, Stderr: r.Out})
	if err != nil {
		return true, fmt.Errorf("scripts.archive: %w", err)
	}
	return true, nil
}

// removeWorktree unlinks the shared-directory links (only the links:
// nothing under them is touched) and removes the worktree, w its entry in
// git's worktree list. One whose directory is gone is unregistered alone
// (UnregisterArgs); one git does not list (w nil) is left to git, its
// directory removed only when empty. One a killed `git worktree add` left
// half made goes despite the add's lock (halfMadeRemoveArgs); a lock of
// any other reason stops git.
func (r *prep) removeWorktree(ctx context.Context, w *Worktree, force bool) error {
	t := r.Task
	for _, l := range ShareLinks(t.Repo, t.Worktree) {
		r.logf("unlink %s", l)
		if err := os.Remove(filepath.Join(t.Worktree, filepath.FromSlash(l))); err != nil {
			return err
		}
	}
	switch {
	case w == nil:
		r.logf("%s is not a worktree of %s", t.Worktree, t.Repo)
		removeIfEmpty(t.Worktree)
		return nil
	case w.HalfMade():
		return r.run(ctx, t.Repo, "git", halfMadeRemoveArgs(t.Worktree)...)
	case !IsDir(t.Worktree):
		return r.run(ctx, t.Repo, "git", UnregisterArgs(ctx, r.Env, t.Worktree)...)
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	return r.run(ctx, t.Repo, "git", append(args, t.Worktree)...)
}

// removeBranch deletes the task's branch in the source checkout when no
// commit would be lost: `git branch -d` takes it, or — a squash or rebase
// merge leaves it unmerged for that — git finds its changes in the task's
// base or the default branch (Merged), or gh says its pull request was
// merged at its commit; neither, and the bases are fetched from origin for
// git to look again. Only the commit found merged is deleted (git
// update-ref -d with it as the old value), never a branch checked out
// somewhere. It returns the branch when it is kept.
func (r *prep) removeBranch(ctx context.Context) string {
	t := r.Task
	ref := "refs/heads/" + t.Branch
	if t.Branch == "" || !r.has(ctx, ref) {
		r.logf("no branch to delete")
		return ""
	}
	if r.run(ctx, t.Repo, "git", "branch", "-d", t.Branch) == nil {
		return ""
	}
	head := objectOf(ctx, r.Env, t.Repo, ref+"^{commit}")
	if head == "" {
		r.logf("keeping branch %s: it is not a commit", t.Branch)
		return t.Branch
	}
	if err := r.branchMerged(ctx, head); err != nil {
		r.logf("keeping branch %s: %v", t.Branch, err)
		return t.Branch
	}
	if err := r.deleteBranch(ctx, head); err != nil {
		r.logf("keeping branch %s: %v", t.Branch, err)
		return t.Branch
	}
	return ""
}

// branchMerged is nil when the branch at head loses nothing: git finds its
// changes in a base (Merged), or gh says its pull request was merged at
// head (mergedPR); when neither does, the bases are fetched and git looks
// again.
func (r *prep) branchMerged(ctx context.Context, head string) error {
	t := r.Task
	var bases []string
	if t.Base != "" {
		bases = append(bases, t.Base)
	}
	if def, err := DefaultBranch(ctx, r.Env, t.Repo); err == nil && def != t.Base {
		bases = append(bases, def)
	}
	in := strings.Join(bases, " or ")
	ok, gitErr := Merged(ctx, r.Env, t.Repo, head, bases...)
	if !ok {
		prErr := r.mergedPR(ctx, head)
		if prErr == nil {
			return nil
		}
		if gitErr == nil && r.fetchBases(ctx, bases) {
			ok, gitErr = Merged(ctx, r.Env, t.Repo, head, bases...)
		}
		switch {
		case ok:
		case gitErr != nil:
			return fmt.Errorf("git cannot tell whether its changes are in %s (%v), and %v", in, gitErr, prErr)
		case len(bases) == 0:
			return fmt.Errorf("no base branch to look in, and %v", prErr)
		default:
			return fmt.Errorf("git does not find its changes in %s, and %v", in, prErr)
		}
	}
	r.logf("git finds the changes of %s in %s", t.Branch, in)
	return nil
}

// fetchBases fetches bases from origin (a failure — offline, without
// origin, over limitNetwork — is only logged) and reports whether one of
// origin's remote-tracking branches moved.
func (r *prep) fetchBases(ctx context.Context, bases []string) bool {
	moved := false
	for _, b := range bases {
		ref := "refs/remotes/origin/" + b + "^{commit}"
		was := objectOf(ctx, r.Env, r.Task.Repo, ref)
		if err := r.runLimited(ctx, limitNetwork, r.Task.Repo, "git", "fetch", "origin", b); err != nil {
			r.logf("fetch failed (%v)", err)
			continue
		}
		if objectOf(ctx, r.Env, r.Task.Repo, ref) != was {
			moved = true
		}
	}
	return moved
}

// deleteBranch deletes the task's branch if it is still at head (git
// update-ref -d compares and deletes at once) and checked out in no
// worktree, then its configuration.
func (r *prep) deleteBranch(ctx context.Context, head string) error {
	t := r.Task
	ws, err := ListWorktrees(ctx, r.Env, t.Repo)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if w.Branch == t.Branch {
			return fmt.Errorf("it is checked out in %s", w.Path)
		}
	}
	if err := r.run(ctx, t.Repo, "git", "update-ref", "-d", "refs/heads/"+t.Branch, head); err != nil {
		return fmt.Errorf("it is no longer at %s, the commit found merged (%v)", head, err)
	}
	Git(ctx, r.Env, t.Repo, "config", "--remove-section", "branch."+t.Branch) // none is fine
	return nil
}

// mergedPR is nil when gh reports the branch's pull request MERGED with
// headRefOid head. A PR task's pull request is looked up by its number: gh
// finds a fork's pull request by its number only, not by the branch name.
func (r *prep) mergedPR(ctx context.Context, head string) error {
	t := r.Task
	which := t.Branch
	if t.Kind == wire.TaskKindPR {
		which = strconv.Itoa(t.Number)
	}
	fmt.Fprintf(r.Out, "$ gh pr view %s --json state,headRefOid\n", which)
	ctx, cancel := withLimit(ctx, r.Env, limitNetwork)
	defer cancel()
	out, err := GH(ctx, r.Env, t.Repo, "pr", "view", which, "--json", "state,headRefOid")
	if err != nil {
		return err
	}
	var pr struct {
		State string `json:"state"`
		Head  string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &pr); err != nil {
		return errors.New("gh pr view: unexpected answer")
	}
	switch {
	case pr.State != "MERGED":
		return fmt.Errorf("its pull request is %s", strings.ToLower(cmp.Or(pr.State, "unknown")))
	case pr.Head != head:
		return errors.New("it has commits its merged pull request does not")
	}
	return nil
}
