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
// relative to worktree, see ShareLinks).
func Changes(ctx context.Context, env []string, worktree string, links []string) ([]string, error) {
	out, err := Git(ctx, env, worktree, "status", "--porcelain", "-z", "--untracked-files=normal")
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
		if fi, err := os.Lstat(filepath.Join(worktree, rel)); err == nil && isLink(fi) {
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

// Removal is 片付け of a worktree task once its sessions have stopped:
// what `stagent task remove` runs, or the bridge itself without archive
// (ADR 0006).
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
// branch — stopping at the first that fails (*StageError). It returns the
// task's branch when it is kept: `git branch -d` refused it and gh does not
// say its pull request was merged at the very commit the branch is on.
// Remote branches are never deleted.
func Remove(ctx context.Context, r Removal) (keptBranch string, err error) {
	if r.Out == nil {
		r.Out = io.Discard
	}
	p := &prep{Prep{Task: r.Task, Env: r.Env, Stdin: r.Stdin, Out: r.Out}}
	stage := func(name string) {
		if r.Stage != nil {
			r.Stage(name)
		}
		fmt.Fprintf(r.Out, "\n[stagent] %s\n", name)
	}
	if r.Archive {
		stage(wire.TaskStageArchive)
		if err := p.archive(ctx); err != nil {
			return "", &StageError{Stage: wire.TaskStageArchive, Err: err}
		}
	}
	stage(wire.TaskStageWorktree)
	if err := p.removeWorktree(ctx, r.Force); err != nil {
		return "", &StageError{Stage: wire.TaskStageWorktree, Err: err}
	}
	stage(wire.TaskStageBranch)
	return p.removeBranch(ctx), nil
}

// archive runs scripts.archive of the source checkout's orca.yaml in the
// worktree, the way setup runs.
func (r *prep) archive(ctx context.Context) error {
	t := r.Task
	o, err := LoadOrca(t.Repo)
	if err != nil {
		return err
	}
	if o == nil || o.Archive == "" {
		r.logf("no scripts.archive in %s", filepath.Join(t.Repo, OrcaFile))
		return nil
	}
	err = RunScript(ctx, Script{Text: o.Archive, Dir: t.Worktree, Root: t.Repo, Env: r.Env, Stdin: r.Stdin, Stdout: r.Out, Stderr: r.Out})
	if err != nil {
		return fmt.Errorf("scripts.archive: %w", err)
	}
	return nil
}

// removeWorktree unlinks the shared-directory links (only the links:
// nothing under them is touched) and removes the worktree. A worktree git
// no longer lists is pruned; its directory goes only when empty.
func (r *prep) removeWorktree(ctx context.Context, force bool) error {
	t := r.Task
	for _, l := range ShareLinks(t.Repo, t.Worktree) {
		r.logf("unlink %s", l)
		if err := os.Remove(filepath.Join(t.Worktree, filepath.FromSlash(l))); err != nil {
			return err
		}
	}
	listed, err := Listed(ctx, r.Env, t.Repo, t.Worktree)
	if err != nil {
		return err
	}
	if !listed {
		r.logf("%s is not a worktree of %s", t.Worktree, t.Repo)
		if err := r.run(ctx, t.Repo, "git", "worktree", "prune"); err != nil {
			return err
		}
		removeIfEmpty(t.Worktree)
		return nil
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	return r.run(ctx, t.Repo, "git", append(args, t.Worktree)...)
}

// removeBranch deletes the task's branch in the source checkout when it is
// merged (git branch -d), or when gh says its pull request was merged with
// the branch's own head commit (a squash merge leaves it unmerged for
// git). It returns the branch when it stays.
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
	if err := r.mergedPR(ctx, ref); err != nil {
		r.logf("keeping branch %s: %v", t.Branch, err)
		return t.Branch
	}
	if err := r.run(ctx, t.Repo, "git", "branch", "-D", t.Branch); err != nil {
		r.logf("keeping branch %s: %v", t.Branch, err)
		return t.Branch
	}
	return ""
}

// mergedPR is nil when gh reports the branch's pull request MERGED with
// headRefOid the commit ref (the local branch) is on. A PR task's pull
// request is looked up by its number: gh finds a fork's pull request by
// its number only, not by the branch name.
func (r *prep) mergedPR(ctx context.Context, ref string) error {
	t := r.Task
	which := t.Branch
	if t.Kind == wire.TaskKindPR {
		which = strconv.Itoa(t.Number)
	}
	fmt.Fprintf(r.Out, "$ gh pr view %s --json state,headRefOid\n", which)
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
	head, err := Git(ctx, r.Env, t.Repo, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return err
	}
	switch {
	case pr.State != "MERGED":
		return fmt.Errorf("its pull request is %s", strings.ToLower(cmp.Or(pr.State, "unknown")))
	case strings.TrimSpace(string(head)) != pr.Head:
		return errors.New("it has commits its merged pull request does not")
	}
	return nil
}
