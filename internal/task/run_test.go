//go:build !windows

package task

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// stubGH stands in for gh: `issue develop` creates the branch on origin
// from base and fetches it (or fails like a missing permission with
// GH_STUB_FAIL set), `pr checkout` fetches refs/pull/<n>/head (a fork's PR)
// into the given branch and checks it out. Every call is logged.
const stubGH = `#!/bin/sh
echo "gh $*" >> "$GH_STUB_LOG"
case "$1 $2" in
"issue develop")
  if [ -n "$GH_STUB_FAIL" ]; then
    echo "GraphQL: Resource not accessible by integration (createLinkedBranch)" >&2
    exit 1
  fi
  shift 3
  while [ $# -gt 0 ]; do
    case "$1" in --base) base=$2; shift 2;; --name) name=$2; shift 2;; *) shift;; esac
  done
  git push -q origin "refs/remotes/origin/$base:refs/heads/$name" || exit 1
  git fetch -q origin "+refs/heads/$name:refs/remotes/origin/$name" || exit 1
  echo "github.com/o/r/tree/$name"
  ;;
"pr checkout")
  n=$3; shift 3
  while [ $# -gt 0 ]; do
    case "$1" in --branch) b=$2; shift 2;; *) shift;; esac
  done
  git fetch -q origin "refs/pull/$n/head:$b" && git checkout -q "$b"
  ;;
*)
  echo "stub gh: unexpected $*" >&2
  exit 1
  ;;
esac
`

type fixture struct {
	base, repo string
	env        []string
	ghLog      string
}

// newFixture makes origin (a bare repository whose main holds files and
// whose refs/pull/7/head is a fork's PR), a clone of it (the source
// checkout) and a gh stub first on PATH.
func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	base, seed := newRepo(t)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(seed, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, seed, "add", name)
	}
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "files")
	git(t, seed, "switch", "-q", "-c", "fork-work")
	git(t, seed, "commit", "-q", "--allow-empty", "-m", "fork change")
	origin := filepath.Join(base, "origin.git")
	git(t, base, "init", "-q", "--bare", "-b", "main", origin)
	git(t, seed, "push", "-q", origin, "main", "fork-work:refs/pull/7/head")
	git(t, base, "clone", "-q", origin, "repo-src")
	f := &fixture{base: base, repo: filepath.Join(base, "repo-src"), ghLog: filepath.Join(base, "gh.log")}
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stubGH), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GH_STUB_LOG="+f.ghLog, "SHELL=/bin/sh")
	return f
}

// task is a task of the fixture's checkout, its worktree directory made
// empty as the bridge does.
func (f *fixture) task(t *testing.T, kind string, number int, branch string) wire.Task {
	t.Helper()
	wt := filepath.Join(f.base, "repo-src-"+strconv.Itoa(number))
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	return wire.Task{ID: "t", Repo: f.repo, Kind: kind, Number: number, Branch: branch, Base: "main", Worktree: wt}
}

// prepare runs Prepare and returns the stages it announced, its terminal
// output and its error.
func (f *fixture) prepare(t *testing.T, p Prep) ([]string, string, error) {
	t.Helper()
	var stages []string
	var out bytes.Buffer
	p.Env, p.Out = f.env, &out
	p.Stage = func(s string) { stages = append(stages, s) }
	err := Prepare(context.Background(), p)
	return stages, out.String(), err
}

func (f *fixture) gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := Git(context.Background(), f.env, dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stageOf(err error) string {
	var se *StageError
	if errors.As(err, &se) {
		return se.Stage
	}
	return ""
}

const setupWritesRoot = "scripts:\n  setup: |\n    echo \"$ORCA_ROOT_PATH $ORCA_WORKSPACE_NAME\" > setup-ran\n"

// An issue task: gh links a branch <N>-<slug> made from base on GitHub,
// the worktree checks it out tracking origin's, and setup runs in it.
func TestPrepareIssue(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	tk := f.task(t, wire.TaskKindIssue, 3, IssueBranch(3, "Fix the login redirect"))
	stages, out, err := f.prepare(t, Prep{Task: tk})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if want := []string{"fetch", "branch", "worktree", "copy", "setup"}; !slices.Equal(stages, want) {
		t.Fatalf("stages %q, want %q", stages, want)
	}
	if got := f.gitOut(t, tk.Worktree, "symbolic-ref", "--short", "HEAD"); got != "3-fix-the-login-redirect" {
		t.Fatalf("worktree on %q", got)
	}
	if got := f.gitOut(t, tk.Worktree, "rev-parse", "--abbrev-ref", "@{upstream}"); got != "origin/3-fix-the-login-redirect" {
		t.Fatalf("upstream %q", got)
	}
	if !strings.Contains(readFile(t, f.ghLog), "gh issue develop 3 --base main --name 3-fix-the-login-redirect") {
		t.Fatalf("gh calls:\n%s", readFile(t, f.ghLog))
	}
	if got := readFile(t, filepath.Join(tk.Worktree, "setup-ran")); got != f.repo+" repo-src-3\n" {
		t.Fatalf("setup saw %q", got)
	}
}

// gh failing to link the branch (permissions, network) makes it locally
// from base only, with one line saying so, and goes on.
func TestPrepareIssueWithoutGitHub(t *testing.T) {
	f := newFixture(t, nil)
	f.env = append(f.env, "GH_STUB_FAIL=1")
	tk := f.task(t, wire.TaskKindIssue, 4, "4-offline")
	_, out, err := f.prepare(t, Prep{Task: tk})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if got := f.gitOut(t, tk.Worktree, "symbolic-ref", "--short", "HEAD"); got != "4-offline" {
		t.Fatalf("worktree on %q", got)
	}
	if HasRef(context.Background(), f.env, f.repo, "refs/remotes/origin/4-offline") {
		t.Fatal("the branch reached origin")
	}
	if n := strings.Count(out, "[stagent] gh issue develop failed"); n != 1 {
		t.Fatalf("%d fallback lines in:\n%s", n, out)
	}
}

// A PR task (a fork's: only refs/pull/N/head) is checked out by gh in a
// detached worktree under the head branch's name.
func TestPreparePR(t *testing.T) {
	f := newFixture(t, nil)
	tk := f.task(t, wire.TaskKindPR, 7, "fork-work")
	_, out, err := f.prepare(t, Prep{Task: tk})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if got := f.gitOut(t, tk.Worktree, "symbolic-ref", "--short", "HEAD"); got != "fork-work" {
		t.Fatalf("worktree on %q", got)
	}
	if got := f.gitOut(t, tk.Worktree, "log", "-1", "--format=%s"); got != "fork change" {
		t.Fatalf("worktree at %q", got)
	}
}

// A PR whose head branch name is checked out elsewhere fails instead of
// being renamed, naming where; the directory left empty is removed.
func TestPreparePRBranchClash(t *testing.T) {
	f := newFixture(t, nil)
	git(t, f.repo, "switch", "-q", "-c", "fork-work")
	tk := f.task(t, wire.TaskKindPR, 7, "fork-work")
	_, _, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != wire.TaskStageBranch || !strings.Contains(err.Error(), f.repo) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("empty task directory left: %v", err)
	}
}

// Setup failing fails the task before the agent; preparing it again takes
// the worktree as it is and runs setup again.
func TestPrepareSetupFailsThenRetry(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": "scripts:\n  setup: |\n    echo run >> \"$ORCA_ROOT_PATH/../setup-runs\"\n    test -f \"$ORCA_ROOT_PATH/ok\"\n    touch done\n"})
	tk := f.task(t, wire.TaskKindIssue, 5, "5-retry")
	stages, out, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != wire.TaskStageSetup || !strings.Contains(err.Error(), "exited with code 1") {
		t.Fatalf("err = %v\n%s", err, out)
	}
	if stages[len(stages)-1] != wire.TaskStageSetup {
		t.Fatalf("stages %q", stages)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "done")); err == nil {
		t.Fatal("setup went on after a failing command")
	}
	mine := filepath.Join(tk.Worktree, "work-in-progress")
	if err := os.WriteFile(mine, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.repo, "ok"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, out, err := f.prepare(t, Prep{Task: tk}); err != nil {
		t.Fatalf("again: %v\n%s", err, out)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("the worktree was made again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "done")); err != nil {
		t.Fatal("setup did not finish")
	}
	if got := readFile(t, filepath.Join(f.base, "setup-runs")); got != "run\nrun\n" {
		t.Fatalf("setup runs %q", got)
	}
}

// A non-empty directory that is no worktree of the checkout fails and is
// kept as it is.
func TestPrepareForeignDirectory(t *testing.T) {
	f := newFixture(t, nil)
	tk := f.task(t, wire.TaskKindIssue, 6, "6-x")
	if err := os.WriteFile(filepath.Join(tk.Worktree, "notes"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != wire.TaskStageWorktree || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "notes")); err != nil {
		t.Fatal(err)
	}
}

// The task's branch checked out in another worktree fails, naming it.
func TestPrepareBranchInAnotherWorktree(t *testing.T) {
	f := newFixture(t, nil)
	other := filepath.Join(f.base, "elsewhere")
	git(t, f.repo, "worktree", "add", "-q", "-b", "8-taken", other)
	tk := f.task(t, wire.TaskKindIssue, 8, "8-taken")
	_, _, err := f.prepare(t, Prep{Task: tk})
	if stageOf(err) != wire.TaskStageWorktree || !strings.Contains(err.Error(), other) {
		t.Fatalf("err = %v", err)
	}
}

// A broken orca.yaml fails setup.
func TestPrepareBrokenOrcaYaml(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": "scripts:\n  setup: a\n  setup: b\n"})
	tk := f.task(t, wire.TaskKindIssue, 9, "9-x")
	_, _, err := f.prepare(t, Prep{Task: tk})
	var oe *OrcaError
	if stageOf(err) != wire.TaskStageSetup || !errors.As(err, &oe) {
		t.Fatalf("err = %v", err)
	}
}

// in_place: without switch_branch the checkout stays as it is and setup
// runs only when asked; with both it switches and runs setup.
func TestPrepareInPlace(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	tk := wire.Task{ID: "t", Repo: f.repo, Kind: wire.TaskKindIssue, Number: 2, Branch: "2-here", Base: "main", Worktree: f.repo, InPlace: true}
	stages, out, err := f.prepare(t, Prep{Task: tk})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if want := []string{"fetch", "branch"}; !slices.Equal(stages, want) {
		t.Fatalf("stages %q, want %q", stages, want)
	}
	if got := f.gitOut(t, f.repo, "symbolic-ref", "--short", "HEAD"); got != "main" {
		t.Fatalf("checkout on %q", got)
	}
	stages, out, err = f.prepare(t, Prep{Task: tk, SwitchBranch: true, Setup: true})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if want := []string{"fetch", "branch", "setup"}; !slices.Equal(stages, want) {
		t.Fatalf("stages %q, want %q", stages, want)
	}
	if got := f.gitOut(t, f.repo, "symbolic-ref", "--short", "HEAD"); got != "2-here" {
		t.Fatalf("checkout on %q", got)
	}
	if _, err := os.Stat(filepath.Join(f.repo, "setup-ran")); err != nil {
		t.Fatal("setup did not run")
	}
}

// in_place PR with switch_branch: gh checks the PR out in the checkout;
// again, it is already there.
func TestPrepareInPlacePR(t *testing.T) {
	f := newFixture(t, nil)
	tk := wire.Task{ID: "t", Repo: f.repo, Kind: wire.TaskKindPR, Number: 7, Branch: "fork-work", Base: "main", Worktree: f.repo, InPlace: true}
	for range 2 {
		if _, out, err := f.prepare(t, Prep{Task: tk, SwitchBranch: true}); err != nil {
			t.Fatalf("Prepare: %v\n%s", err, out)
		}
		if got := f.gitOut(t, f.repo, "log", "-1", "--format=%D %s"); got != "HEAD -> fork-work fork change" {
			t.Fatalf("checkout at %q", got)
		}
	}
	if n := strings.Count(readFile(t, f.ghLog), "gh pr checkout 7 --branch fork-work"); n != 1 {
		t.Fatalf("gh calls:\n%s", readFile(t, f.ghLog))
	}
}

func TestIssueBranch(t *testing.T) {
	for _, c := range []struct {
		number int
		title  string
		want   string
	}{
		{493, "Windows GitHub icon", "493-windows-github-icon"},
		{12, "Fix: login redirect (#12)!", "12-fix-login-redirect-12"},
		{5, "Windows の GitHub 画面", "5-windows-github"},
		{6, "  --Already__Hyphenated--  ", "6-already-hyphenated"},
		{7, "日本語だけのタイトル", "issue-7"},
		{8, "", "issue-8"},
		{9, "abcdefghij abcdefghij abcdefghij abcdef ghij", "9-abcdefghij-abcdefghij-abcdefghij-abcdef"},
	} {
		if got := IssueBranch(c.number, c.title); got != c.want {
			t.Errorf("IssueBranch(%d, %q) = %q, want %q", c.number, c.title, got, c.want)
		}
	}
}

func TestRunScriptStopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := RunScript(context.Background(), Script{
		Text: "echo one\nfalse\necho two\n", Dir: dir, Root: dir,
		Env: append(os.Environ(), "SHELL=/bin/sh"), Stdout: &out, Stderr: &out,
	})
	if err == nil || err.Error() != "exited with code 1" || out.String() != "one\n" {
		t.Fatalf("err = %v, output %q", err, out.String())
	}
}
