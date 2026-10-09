package task

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Prep is a task to prepare: what `stagent task run` does in the task's
// first session before it starts the agent (ADR 0006).
type Prep struct {
	Task wire.Task
	// SwitchBranch (in_place only): switch the checkout to Task.Branch.
	SwitchBranch bool
	// Setup (in_place only): run scripts.setup. A worktree task always
	// runs it.
	Setup bool
	// WorktreeInclude are the app's patterns of files to copy into a
	// worktree task's worktree (config.json repos.<repo>.worktree_include).
	WorktreeInclude []string
	// Env is the environment of git, gh and the setup script; nil: this
	// process's.
	Env []string
	// Stdin feeds the setup script (nil: none); Out, the terminal, gets
	// the stage headers, the commands run and their output.
	Stdin io.Reader
	Out   io.Writer
	// Stage is told each stage as it starts (task.progress).
	Stage func(stage string)
}

// StageError is the stage that failed and why.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }

func (e *StageError) Unwrap() error { return e.Err }

// Prepare runs the stages of p's task in order — fetch, branch, worktree,
// copy, setup — stopping at the first that fails (*StageError). A
// worktree task whose directory is left empty then has it removed (after
// leaving it when it is the working directory). Every stage can run
// twice: a task that failed is prepared again with the same task (ADR
// 0006).
func Prepare(ctx context.Context, p Prep) error {
	if p.Out == nil {
		p.Out = io.Discard
	}
	r := &prep{Prep: p}
	for _, s := range r.stages() {
		if p.Stage != nil {
			p.Stage(s.name)
		}
		fmt.Fprintf(p.Out, "\n[stagent] %s\n", s.name)
		if err := s.run(ctx); err != nil {
			if !p.Task.InPlace {
				removeIfEmpty(p.Task.Worktree)
			}
			return &StageError{Stage: s.name, Err: err}
		}
	}
	return nil
}

type stage struct {
	name string
	run  func(context.Context) error
}

type prep struct {
	Prep
}

// stages lists what preparing the task runs. An in_place task has no
// worktree and copy stages and runs setup only when asked.
func (r *prep) stages() []stage {
	s := []stage{
		{wire.TaskStageFetch, r.fetch},
		{wire.TaskStageBranch, r.branch},
	}
	if !r.Task.InPlace {
		s = append(s, stage{wire.TaskStageWorktree, r.worktree}, stage{wire.TaskStageCopy, r.copy})
	}
	if !r.Task.InPlace || r.Setup {
		s = append(s, stage{wire.TaskStageSetup, r.setup})
	}
	return s
}

func (r *prep) logf(format string, args ...any) {
	fmt.Fprintf(r.Out, "[stagent] "+format+"\n", args...)
}

// run runs a git or gh command for the terminal to see. Its error is the
// command's last stderr line.
func (r *prep) run(ctx context.Context, dir, name string, args ...string) error {
	bin, err := lookProgram(r.Env, name)
	if err != nil {
		return fmt.Errorf("%s not found", name)
	}
	if name == "git" {
		args = gitArgs(args)
	}
	fmt.Fprintf(r.Out, "$ %s %s\n", name, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, r.Env
	var tail tailWriter
	cmd.Stdout = r.Out
	cmd.Stderr = io.MultiWriter(r.Out, &tail)
	if err := cmd.Run(); err != nil {
		sub := ""
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && a != "core.longpaths=true" {
				sub = " " + a
				break
			}
		}
		if msg := lastLine(string(tail.b)); msg != "" {
			return fmt.Errorf("%s%s: %s", name, sub, msg)
		}
		return fmt.Errorf("%s%s: %v", name, sub, err)
	}
	return nil
}

// tailWriter keeps the last 4 KiB written to it.
type tailWriter struct{ b []byte }

func (w *tailWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	if len(w.b) > 4096 {
		w.b = w.b[len(w.b)-4096:]
	}
	return len(p), nil
}

func (r *prep) has(ctx context.Context, ref string) bool {
	return HasRef(ctx, r.Env, r.Task.Repo, ref)
}

// fetch updates origin's base branch. Failing to (offline, no origin) is
// only logged: the branch is then made from what the checkout has.
func (r *prep) fetch(ctx context.Context) error {
	args := []string{"fetch", "origin"}
	if r.Task.Base != "" {
		args = append(args, r.Task.Base)
	}
	if err := r.run(ctx, r.Task.Repo, "git", args...); err != nil {
		r.logf("fetch failed (%v); continuing with what the checkout has", err)
	}
	return nil
}

// baseRef is the commit a new branch starts from: origin's base branch,
// else a local one.
func (r *prep) baseRef(ctx context.Context) (string, error) {
	b := r.Task.Base
	for _, ref := range []string{"refs/remotes/origin/" + b, "refs/heads/" + b} {
		if r.has(ctx, ref) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("base branch %s not found (neither origin/%s nor a local %s)", b, b, b)
}

// branch makes the task's branch. Issue: `gh issue develop` links it to
// the issue on GitHub (or reuses the linked branch of that name), then a
// local branch tracks it; when gh fails it is made locally from base only.
// PR: `gh pr checkout` in a detached worktree (fork PRs get their push
// remote from gh). in_place: the checkout switches to it only with
// SwitchBranch.
func (r *prep) branch(ctx context.Context) error {
	t := r.Task
	switch {
	case t.InPlace && !r.SwitchBranch:
		r.logf("staying on the current branch")
		return nil
	case t.Kind == wire.TaskKindIssue:
		if err := r.issueBranch(ctx); err != nil {
			return err
		}
		if t.InPlace {
			return r.switchTo(ctx)
		}
		return nil
	case t.InPlace:
		if cur, err := CurrentBranch(ctx, r.Env, t.Repo); err == nil && cur == t.Branch {
			r.logf("%s is checked out already", t.Branch)
			return nil
		}
		return r.run(ctx, t.Repo, "gh", "pr", "checkout", strconv.Itoa(t.Number), "--branch", t.Branch)
	}
	return r.prCheckout(ctx)
}

func (r *prep) issueBranch(ctx context.Context) error {
	t := r.Task
	err := r.run(ctx, t.Repo, "gh", "issue", "develop", strconv.Itoa(t.Number), "--base", t.Base, "--name", t.Branch)
	if err != nil {
		r.logf("gh issue develop failed (%v): %s is made locally from %s, not on GitHub", err, t.Branch, t.Base)
	} else if !r.has(ctx, "refs/remotes/origin/"+t.Branch) {
		// gh fetches the branch only where the checkout's remote is the
		// issue's repository.
		r.run(ctx, t.Repo, "git", "fetch", "origin", "+refs/heads/"+t.Branch+":refs/remotes/origin/"+t.Branch)
	}
	if r.has(ctx, "refs/heads/"+t.Branch) {
		return nil
	}
	if r.has(ctx, "refs/remotes/origin/"+t.Branch) {
		return r.run(ctx, t.Repo, "git", "branch", "--track", t.Branch, "origin/"+t.Branch)
	}
	base, err := r.baseRef(ctx)
	if err != nil {
		return err
	}
	return r.run(ctx, t.Repo, "git", "branch", "--no-track", t.Branch, base)
}

// switchTo switches the in_place checkout to the task's branch.
func (r *prep) switchTo(ctx context.Context) error {
	t := r.Task
	if cur, err := CurrentBranch(ctx, r.Env, t.Repo); err == nil && cur == t.Branch {
		r.logf("%s is checked out already", t.Branch)
		return nil
	}
	return r.run(ctx, t.Repo, "git", "switch", t.Branch)
}

// prCheckout makes a worktree task's PR branch: in a detached worktree
// (made unless an earlier attempt left it) `gh pr checkout` checks out the
// head branch under its own name. A local branch of that name checked out
// elsewhere fails rather than being renamed.
func (r *prep) prCheckout(ctx context.Context) error {
	t := r.Task
	w, err := r.place(ctx)
	if err != nil {
		return err
	}
	switch {
	case w == nil:
		if err := r.run(ctx, t.Repo, "git", "worktree", "add", "--detach", t.Worktree); err != nil {
			return err
		}
	case w.Branch == t.Branch:
		r.logf("the worktree is on %s already", t.Branch)
		return nil
	case w.Branch != "":
		return fmt.Errorf("%s is a worktree on branch %s, not %s", t.Worktree, w.Branch, t.Branch)
	}
	return r.run(ctx, t.Worktree, "gh", "pr", "checkout", strconv.Itoa(t.Number), "--branch", t.Branch)
}

// worktree adds the task's worktree on its branch in the (empty) task
// directory, or takes the worktree of the repository already there on
// that branch.
func (r *prep) worktree(ctx context.Context) error {
	t := r.Task
	w, err := r.place(ctx)
	if err != nil {
		return err
	}
	if w != nil {
		if w.Branch != t.Branch {
			return fmt.Errorf("%s is a worktree on %s, not %s", t.Worktree, cmpOrDetached(w.Branch), t.Branch)
		}
		r.logf("using the worktree %s on %s", t.Worktree, t.Branch)
		return nil
	}
	return r.run(ctx, t.Repo, "git", "worktree", "add", t.Worktree, t.Branch)
}

func cmpOrDetached(branch string) string {
	if branch == "" {
		return "a detached HEAD"
	}
	return "branch " + branch
}

// place looks at the task's directory: the repository's worktree there,
// if any. Otherwise the directory must be empty (it is made when absent)
// and the task's branch not checked out in another worktree. A worktree
// git still lists without its directory is pruned first.
func (r *prep) place(ctx context.Context) (*Worktree, error) {
	t := r.Task
	dir := CanonicalPath(t.Worktree)
	ws, err := ListWorktrees(ctx, r.Env, t.Repo)
	if err != nil {
		return nil, err
	}
	for _, w := range ws {
		if !SamePath(w.Path, dir) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return &w, nil
		}
		if err := r.run(ctx, t.Repo, "git", "worktree", "prune"); err != nil {
			return nil, err
		}
	}
	for _, w := range ws {
		if w.Branch == t.Branch && !SamePath(w.Path, dir) {
			return nil, fmt.Errorf("branch %s is checked out in another worktree: %s", t.Branch, w.Path)
		}
	}
	if err := os.MkdirAll(t.Worktree, 0o755); err != nil {
		return nil, err
	}
	if empty, err := isEmptyDir(t.Worktree); err != nil {
		return nil, err
	} else if !empty {
		return nil, fmt.Errorf("%s is not empty and not a worktree of %s", t.Worktree, t.Repo)
	}
	return nil, nil
}

// copy links the shared directories (共有ディレクトリ) and copies the
// files to copy (コピーするファイル) from the source checkout into the
// worktree (CopyFiles). It never fails: setup and the agent start anyway.
// A broken orca.yaml in the source checkout only loses its
// sharedDirectories (setup then fails on it in the worktree).
func (r *prep) copy(ctx context.Context) error {
	t := r.Task
	spec := CopySpec{Repo: t.Repo, Env: r.Env, Patterns: r.WorktreeInclude}
	o, err := LoadOrca(t.Repo)
	switch {
	case err != nil:
		r.logf("warning: %v; worktree.sharedDirectories ignored", oneLine(err.Error()))
	case o != nil:
		spec.Shared = o.SharedDirectories
	}
	CopyFiles(ctx, CopyJob{CopySpec: spec, Dest: t.Worktree, Out: r.Out})
	return nil
}

// setup runs scripts.setup of the orca.yaml in the task's place. No file
// or no script is nothing to do; a broken orca.yaml fails.
func (r *prep) setup(ctx context.Context) error {
	t := r.Task
	o, err := LoadOrca(t.Worktree)
	if err != nil {
		return err
	}
	if o == nil || o.Setup == "" {
		r.logf("no scripts.setup in %s", OrcaFile)
		return nil
	}
	err = RunScript(ctx, Script{Text: o.Setup, Dir: t.Worktree, Root: t.Repo, Env: r.Env, Stdin: r.Stdin, Stdout: r.Out, Stderr: r.Out})
	if err != nil {
		return fmt.Errorf("scripts.setup: %w", err)
	}
	return nil
}

func isEmptyDir(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	return false, err
}

// removeIfEmpty removes dir when it is an empty directory, leaving it
// first when it is the working directory (Windows cannot remove that).
func removeIfEmpty(dir string) {
	if empty, err := isEmptyDir(dir); err != nil || !empty {
		return
	}
	if wd, err := os.Getwd(); err == nil && SamePath(CanonicalPath(wd), CanonicalPath(dir)) {
		os.Chdir(filepath.Dir(dir))
	}
	os.Remove(dir)
}
