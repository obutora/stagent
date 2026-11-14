//go:build !windows

package task

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

// hangGit puts a git first on the fixture's PATH that hangs (until it is
// killed) on the calls matching each pattern and is the real git
// otherwise, and scales every limit to a hundredth.
func (f *fixture) hangGit(t *testing.T, patterns ...string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	never := filepath.Join(t.TempDir(), "never")
	var rules []stubcmd.Rule
	for _, p := range patterns {
		rules = append(rules, stubcmd.Rule{Args: p, Await: never})
	}
	bin := t.TempDir()
	stubcmd.Install(t, bin, "git", append(rules, stubcmd.Rule{Args: "*", Exec: real})...)
	path, _ := lookupEnv(f.env, "PATH")
	f.env = append(slices.Clone(f.env), "PATH="+bin+string(os.PathListSeparator)+path, EnvGitTimeoutScale+"=0.01")
}

// A fetch that does not answer is given up at its limit: preparing goes
// on from what the checkout has, saying so on the terminal.
func TestPrepareFetchTimeout(t *testing.T) {
	f := newFixture(t, nil)
	f.hangGit(t, "fetch *")
	tk := f.task(t, wire.TaskKindName, 0, "brave-otter")
	start := time.Now()
	_, out, err := f.prepare(t, Prep{Task: tk})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fetch timed out after 1.2s") || !strings.Contains(out, "skipped") {
		t.Fatalf("terminal:\n%s", out)
	}
	if got := f.gitOut(t, tk.Worktree, "symbolic-ref", "--short", "HEAD"); got != "brave-otter" {
		t.Fatalf("worktree on %q", got)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("took %v", d)
	}
}

// A git worktree add over its limit fails the worktree stage, saying it
// timed out, and leaves nothing in the way: preparing the task again
// (task.create on a failed task) makes the worktree.
func TestPrepareWorktreeAddTimeout(t *testing.T) {
	f := newFixture(t, nil)
	plain := f.env
	f.hangGit(t, "*worktree add *")
	tk := f.task(t, wire.TaskKindName, 0, "brave-otter")
	_, out, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != wire.TaskStageWorktree || !strings.Contains(err.Error(), "git worktree: timed out after 1.8s") {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if IsDir(tk.Worktree) {
		t.Fatalf("%s left behind", tk.Worktree)
	}
	f.env = plain
	if _, out, err := f.prepare(t, Prep{Task: tk}); err != nil {
		t.Fatalf("again: %v\n%s", err, out)
	}
	if got := f.gitOut(t, tk.Worktree, "symbolic-ref", "--short", "HEAD"); got != "brave-otter" {
		t.Fatalf("worktree on %q", got)
	}
}

// The git status before a removal that does not answer fails at its
// limit, saying it timed out.
func TestChangesTimeout(t *testing.T) {
	f := newFixture(t, nil)
	f.hangGit(t, "status *")
	_, err := Changes(context.Background(), f.env, f.repo, nil)
	var le *LimitError
	if !errors.As(err, &le) || le.Limit != 300*time.Millisecond || !strings.Contains(err.Error(), "git status --porcelain -z --untracked-files=all --ignore-submodules=none: timed out after 300ms") {
		t.Fatalf("Changes: %v", err)
	}
}

// unlocker is a terminal that removes the lock file once a command's
// output names it: another git finishing.
type unlocker struct {
	out  bytes.Buffer
	lock string
}

func (u *unlocker) Write(p []byte) (int, error) {
	u.out.Write(p)
	if u.lock != "" && strings.Contains(u.out.String(), filepath.Base(u.lock)+"'") {
		os.Remove(u.lock)
		u.lock = ""
	}
	return len(p), nil
}

// A git command that fails on a lock another git holds (index.lock) runs
// again after a wait and succeeds once the lock is gone. A lock that
// stays fails it after the last retry.
func TestPrepareLockRetry(t *testing.T) {
	f := newFixture(t, nil)
	tk := wire.Task{ID: "t", Repo: f.repo, Kind: wire.TaskKindIssue, Number: 2, Branch: "2-here", Base: "main", Worktree: f.repo, InPlace: true}
	lock := filepath.Join(f.repo, ".git", "index.lock")
	prepare := func(remove bool) (string, error) {
		if err := os.WriteFile(lock, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		u := &unlocker{}
		if remove {
			u.lock = lock
		}
		err := Prepare(context.Background(), Prep{Task: tk, SwitchBranch: true, Env: f.env, Out: u})
		return u.out.String(), err
	}

	out, err := prepare(false)
	if stageOf(err) != wire.TaskStageBranch || !strings.Contains(err.Error(), "index.lock'") || strings.Count(out, "$ git switch 2-here") != 1+len(lockRetries) {
		t.Fatalf("Prepare with the lock staying: %v\n%s", err, out)
	}
	os.Remove(lock)
	out, err = prepare(true)
	if err != nil || strings.Count(out, "$ git switch 2-here") != 2 || !strings.Contains(out, "trying again in 50ms") {
		t.Fatalf("Prepare with the lock going: %v\n%s", err, out)
	}
	if got := f.gitOut(t, f.repo, "symbolic-ref", "--short", "HEAD"); got != "2-here" {
		t.Fatalf("checkout on %q", got)
	}
}

// scripts.setup and scripts.archive run on a terminal the user sees and
// can ^C: they have no limit, though they outlast every git one.
func TestScriptsHaveNoLimit(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": "scripts:\n  setup: |\n    sleep 2\n    touch setup-ran\n  archive: |\n    sleep 2\n    touch \"$ORCA_ROOT_PATH/archive-ran\"\n"})
	f.env = append(slices.Clone(f.env), EnvGitTimeoutScale+"=0.01")
	tk := f.task(t, wire.TaskKindName, 0, "brave-otter")
	if _, out, err := f.prepare(t, Prep{Task: tk}); err != nil || !present(filepath.Join(tk.Worktree, "setup-ran")) {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	var out bytes.Buffer
	if _, err := Remove(context.Background(), Removal{Task: tk, Archive: true, Force: true, Env: f.env, Out: &out}); err != nil || !present(filepath.Join(f.repo, "archive-ran")) {
		t.Fatalf("Remove: %v\n%s", err, out.String())
	}
}

func present(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
