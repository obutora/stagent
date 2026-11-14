//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// stubGHPRView answers `gh pr view <branch> --json state,headRefOid` with
// the file pr-<branch>.json next to it, failing like gh without a PR.
const stubGHPRView = `#!/bin/sh
[ "$1 $2" = "pr view" ] || { echo "stub gh: unexpected $*" >&2; exit 1; }
f="$(dirname "$0")/pr-$3.json"
[ -f "$f" ] || { echo "no pull requests found for branch \"$3\"" >&2; exit 1; }
cat "$f"
`

// removeEnv is a bridge with a source checkout (repo) whose orca.yaml
// shares node_modules and archives with a script that fails while
// $ORCA_ROOT_PATH/fail-archive exists, and the gh stub on the login PATH.
// bin and home start more bridges for it (startBridgeFor).
type removeEnv struct {
	a         *app
	d         *rpc.Client
	repo      string
	ghBin     string
	gitIn     func(dir string, args ...string) string
	bin, home string
}

const removeOrca = `worktree:
  sharedDirectories:
    - node_modules
scripts:
  archive: |
    test ! -f "$ORCA_ROOT_PATH/fail-archive" || exit 4
    pwd > "$ORCA_ROOT_PATH/archive-ran"
`

func newRemoveEnv(t *testing.T) *removeEnv {
	t.Helper()
	a, l, bin, home := startPersistBridge(t)
	e := &removeEnv{a: a, bin: bin, home: home}
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	e.repo, e.ghBin = filepath.Join(home, "app"), filepath.Join(home, "stub-bin")
	e.gitIn = func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed := filepath.Join(home, "seed")
	e.gitIn(home, "init", "-q", "-b", "main", seed)
	for name, content := range map[string]string{".gitignore": "node_modules/\n.env\n", "a.txt": "a\n", "orca.yaml": removeOrca} {
		writeFile(t, filepath.Join(seed, name), content)
	}
	e.gitIn(seed, "add", ".")
	e.gitIn(seed, "commit", "-q", "-m", "init")
	e.gitIn(home, "clone", "-q", "--bare", seed, "origin.git")
	e.gitIn(home, "clone", "-q", "origin.git", "app")
	writeFile(t, filepath.Join(e.repo, "node_modules", "keep.js"), "kept")

	if err := os.MkdirAll(e.ghBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.ghBin, "gh"), []byte(stubGHPRView), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := os.OpenFile(filepath.Join(home, ".bash_profile"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(profile, "export PATH='%s':\"$PATH\"\n", e.ghBin)
	profile.Close()

	e.d = dialDaemon(t, l)
	return e
}

func dialDaemon(t *testing.T, l *paths.Layout) *rpc.Client {
	t.Helper()
	conn, err := daemonclient.Dial(l, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d := rpc.NewClient(conn, nil)
	t.Cleanup(func() { d.Close() })
	return d
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (e *removeEnv) daemonCall(t *testing.T, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := e.d.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// task registers a ready task of number. A worktree task gets its
// worktree on branch <number>-fix (from origin/main, no upstream) with
// node_modules linked to the checkout's.
func (e *removeEnv) task(t *testing.T, number int, inPlace bool) wire.Task {
	t.Helper()
	place := e.repo + "-" + fmt.Sprint(number)
	branch := fmt.Sprintf("%d-fix", number)
	if !inPlace {
		e.gitIn(e.repo, "worktree", "add", "-q", "--no-track", "-b", branch, place, "origin/main")
		if err := os.Symlink(filepath.Join(e.repo, "node_modules"), filepath.Join(place, "node_modules")); err != nil {
			t.Fatal(err)
		}
	}
	var reg wire.TaskRegisterResult
	e.daemonCall(t, wire.MethodTaskRegister, wire.TaskRegisterParams{
		Repo: e.repo, Kind: wire.TaskKindIssue, Number: number, Title: "Fix", Branch: branch, Base: "main",
		Worktree: place, InPlace: inPlace, SessionID: wire.NewSessionID(),
	}, &reg)
	e.daemonCall(t, wire.MethodTaskProgress, wire.TaskProgressParams{ID: reg.Task.ID, Stage: wire.TaskStageAgent}, nil)
	return reg.Task
}

// shell starts a kept shell in cwd (tied to taskID when given) and waits
// until the daemon tied it to want.
func (e *removeEnv) shell(t *testing.T, cwd, taskID, want string) string {
	t.Helper()
	var res wire.SpawnResult
	e.a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cwd: cwd, TaskID: taskID, Cols: 80, Rows: 24}, &res)
	id := res.Session.ID
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.a.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil)
	})
	e.a.waitFor(t, "session tied to its task", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return s.TaskID == want })
	})
	return id
}

// remove calls task.remove; a failure is returned as its wire code and
// message.
func (e *removeEnv) remove(t *testing.T, p wire.TaskRemoveParams) (wire.TaskRemoveResult, *wire.Error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res wire.TaskRemoveResult
	err := e.a.Call(ctx, wire.MethodTaskRemove, p, &res)
	var we *wire.Error
	if err != nil && !errors.As(err, &we) {
		t.Fatalf("task.remove: %v", err)
	}
	return res, we
}

func (e *removeEnv) tasks(t *testing.T) []wire.Task {
	t.Helper()
	var list wire.TaskListResult
	e.daemonCall(t, wire.MethodTaskList, struct{}{}, &list)
	return list.Tasks
}

func taskRemoved(id string, kept *string) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		var r wire.TaskRemoved
		if m.Method != wire.NotifyTaskRemoved || json.Unmarshal(m.Params, &r) != nil || r.ID != id {
			return false
		}
		*kept = r.KeptBranch
		return true
	}
}

func code(we *wire.Error) string {
	if we == nil {
		return ""
	}
	return we.Code
}

// holdGit puts a git first on the login PATH that, while the file hold
// exists, waits in `git worktree remove` (creating the file held once
// there) and then gives up when the removal that ran it is gone; it is
// git otherwise.
func (e *removeEnv) holdGit(t *testing.T) (hold, held string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	hold, held = filepath.Join(e.ghBin, "hold"), filepath.Join(e.ghBin, "held")
	writeFile(t, filepath.Join(e.ghBin, "git"), fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "worktree remove" ] && [ -f '%s' ]; then
  : > '%s'
  while [ -f '%s' ]; do sleep 0.05; done
  kill -0 $PPID 2>/dev/null || exit 1
fi
exec '%s' "$@"
`, hold, held, hold, real))
	if err := os.Chmod(filepath.Join(e.ghBin, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, hold, "")
	t.Cleanup(func() { os.Remove(hold) })
	return hold, held
}

// goCall calls method on the bridge without waiting for the answer.
func (e *removeEnv) goCall(method string, params any) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), waitLong)
		defer cancel()
		e.a.Call(ctx, method, params, nil)
	}()
}

// waitFile waits until the file p exists.
func waitFile(t *testing.T, p string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		if _, err := os.Stat(p); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", p)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Closing the bridge (the app's SSH connection) in the middle of a
// removal does not stop it: the worktree goes, the unpushed branch is
// kept and the record forgotten after the bridge has exited, and the app
// connecting again is told so (task.removed naming the kept branch).
func TestTaskRemoveOutlivesTheBridge(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 5, false)
	e.gitIn(tk.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	hold, held := e.holdGit(t)

	e.goCall(wire.MethodTaskRemove, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true})
	waitFile(t, held)
	e.a.hangUp()
	again := startBridgeFor(t, e.bin, e.home)
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	var kept string
	again.waitFor(t, "task.removed", taskRemoved(tk.ID, &kept))
	if kept != "5-fix" {
		t.Fatalf("task.removed kept_branch %q, want 5-fix", kept)
	}
	if _, err := os.Lstat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if list := e.gitIn(e.repo, "worktree", "list", "--porcelain"); strings.Contains(list, tk.Worktree) {
		t.Fatalf("worktree still listed:\n%s", list)
	}
	e.gitIn(e.repo, "rev-parse", "--verify", "refs/heads/5-fix")
	if ts := e.tasks(t); len(ts) != 0 {
		t.Fatalf("tasks after removal: %+v", ts)
	}
}

// The session a removal goes on in dying in the middle of it ends the
// removal without a daemon restart: the task is ready again (error
// interrupted, at the stage it got to) and can be removed again.
func TestTaskRemoveInterruptedWhenItsSessionDies(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 6, false)
	hold, held := e.holdGit(t)

	e.goCall(wire.MethodTaskRemove, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true})
	waitFile(t, held)
	var removal wire.Session
	e.a.waitFor(t, "the removal's session", func(m *wire.Msg) bool {
		return m.Method == wire.NotifySessionUpdated && json.Unmarshal(m.Params, &removal) == nil &&
			slices.Contains(removal.Command, "remove") && removal.Cwd == e.repo
	})
	e.a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: removal.ID, Signal: wire.SignalKill}, nil)
	got := e.a.waitFor(t, "task interrupted", func(m *wire.Msg) bool {
		var u wire.Task
		return m.Method == wire.NotifyTaskUpdated && json.Unmarshal(m.Params, &u) == nil && u.ID == tk.ID &&
			u.State == wire.TaskReady && u.Error == wire.TaskErrInterrupted
	})
	var u wire.Task
	json.Unmarshal(got.Params, &u)
	if u.Stage != wire.TaskStageWorktree {
		t.Fatalf("interrupted task %+v, want stage worktree", u)
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if _, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true}); we != nil {
		t.Fatalf("task.remove again: %v", we)
	}
	if _, err := os.Lstat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if ts := e.tasks(t); len(ts) != 0 {
		t.Fatalf("tasks after removal: %+v", ts)
	}
}

// Removing a worktree no task has from the worktree screen goes on when
// the bridge goes in the middle of it: the worktree goes and its merged
// branch is deleted after the bridge has exited.
func TestWorktreeRemoveOutlivesTheBridge(t *testing.T) {
	e := newRemoveEnv(t)
	wt := e.repo + "-loose"
	e.gitIn(e.repo, "worktree", "add", "-q", "--no-track", "-b", "loose", wt, "origin/main")
	e.daemonCall(t, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"worktree_repos":["` + e.repo + `"]}`)}, nil)
	hold, held := e.holdGit(t)

	e.goCall(wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt})
	waitFile(t, held)
	e.a.hangUp()
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(waitLong)
	for {
		_, err := os.Lstat(wt)
		list := e.gitIn(e.repo, "worktree", "list", "--porcelain")
		branch := e.gitIn(e.repo, "branch", "--list", "loose")
		if os.IsNotExist(err) && !strings.Contains(list, wt) && branch == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("worktree there (%v), listed:\n%s\nbranch %q", err, list, branch)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// task.remove without archive: the dry run reports the changes (not the
// ignored .env nor the node_modules link), the unpushed commits and the
// live session; the real call refuses changes (not_clean) and live
// sessions (busy) until forced and told to stop them; then the session
// ends but stays listed without its task, only the link goes, the
// worktree is removed and the unpushed branch kept.
func TestTaskRemoveEndToEnd(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 1, false)
	wt := tk.Worktree
	writeFile(t, filepath.Join(wt, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(wt, "new.txt"), "n\n")
	e.gitIn(wt, "commit", "-q", "--allow-empty", "-m", "one")
	e.gitIn(wt, "commit", "-q", "--allow-empty", "-m", "two")
	writeFile(t, filepath.Join(wt, ".env"), "SECRET=1\n")
	sid := e.shell(t, wt, "", tk.ID)

	dry, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID, DryRun: true})
	if we != nil {
		t.Fatalf("dry run: %v", we)
	}
	if !slices.Equal(dry.Changes, []string{" M a.txt", "?? new.txt"}) || dry.ChangesTruncated || dry.Unpushed != 2 || !slices.Equal(dry.Sessions, []string{sid}) {
		t.Fatalf("dry run = %+v", dry)
	}
	if _, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true}); code(we) != wire.ErrNotClean {
		t.Fatalf("with changes: %v, want %s", we, wire.ErrNotClean)
	}
	if _, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true, Force: true}); code(we) != wire.ErrBusy {
		t.Fatalf("with a live session: %v, want %s", we, wire.ErrBusy)
	}
	if ts := e.tasks(t); len(ts) != 1 || ts[0].State != wire.TaskReady {
		t.Fatalf("refused removals changed the task: %+v", ts)
	}

	res, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID, SkipArchive: true, Force: true, StopSessions: true})
	if we != nil {
		t.Fatalf("task.remove: %v", we)
	}
	if res.KeptBranch != "1-fix" {
		t.Fatalf("result %+v, want kept_branch 1-fix", res)
	}
	var kept string
	e.a.waitFor(t, "task.removed", taskRemoved(tk.ID, &kept))
	if kept != "1-fix" {
		t.Fatalf("task.removed kept_branch %q", kept)
	}
	e.a.waitFor(t, "stopped session listed without its task", func(m *wire.Msg) bool {
		return sessionUpdated(m, sid, func(s wire.Session) bool { return s.ExitCode != nil && s.TaskID == "" })
	})
	if _, err := os.Lstat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(e.repo, "node_modules", "keep.js")); err != nil || string(b) != "kept" {
		t.Fatalf("shared directory content: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "archive-ran")); err == nil {
		t.Fatal("archive ran despite skip_archive")
	}
	e.gitIn(e.repo, "rev-parse", "--verify", "refs/heads/1-fix")
	if ts := e.tasks(t); len(ts) != 0 {
		t.Fatalf("tasks after removal: %+v", ts)
	}
}

// With scripts.archive the removal runs in a session of the source
// checkout and task.remove answers once it is done: an archive failing
// takes the task back to ready with the error; once it passes, it ran in
// the worktree, and a squash-merged PR's branch is deleted.
func TestTaskRemoveArchive(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 2, false)
	e.gitIn(tk.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	head := e.gitIn(tk.Worktree, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(e.ghBin, "pr-2-fix.json"), `{"state":"MERGED","headRefOid":"`+head+`"}`)

	writeFile(t, filepath.Join(e.repo, "fail-archive"), "")
	_, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID})
	if code(we) != wire.ErrInternal || !strings.Contains(we.Message, "exited with code 4") {
		t.Fatalf("failing archive: %v", we)
	}
	ts := e.tasks(t)
	if len(ts) != 1 || ts[0].State != wire.TaskReady || !strings.Contains(ts[0].Error, "exited with code 4") {
		t.Fatalf("task after a failed archive: %+v", ts)
	}
	if _, err := os.Stat(tk.Worktree); err != nil {
		t.Fatalf("worktree after a failed archive: %v", err)
	}
	// It ran in its own session, in the source checkout.
	e.a.waitFor(t, "removal session in the source checkout", func(m *wire.Msg) bool {
		var s wire.Session
		return m.Method == wire.NotifySessionUpdated && json.Unmarshal(m.Params, &s) == nil &&
			slices.Contains(s.Command, "remove") && s.Cwd == e.repo && s.TaskID == ""
	})

	if err := os.Remove(filepath.Join(e.repo, "fail-archive")); err != nil {
		t.Fatal(err)
	}
	res, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID})
	if we != nil || res.KeptBranch != "" {
		t.Fatalf("task.remove = %+v, %v", res, we)
	}
	if b, err := os.ReadFile(filepath.Join(e.repo, "archive-ran")); err != nil || string(b) != tk.Worktree+"\n" {
		t.Fatalf("archive ran in %q, %v", b, err)
	}
	if _, err := os.Lstat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if out, err := exec.Command("git", "-C", e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/2-fix").CombinedOutput(); err == nil {
		t.Fatalf("squash-merged branch kept: %s", out)
	}
	if ts := e.tasks(t); len(ts) != 0 {
		t.Fatalf("tasks after removal: %+v", ts)
	}
}

// A branch squash-merged on origin goes though gh knows no pull request
// for it (another forge, or no gh): git finds its changes in origin's main
// once fetched. Origin's branch stays.
func TestTaskRemoveSquashMergedWithoutGH(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 6, false)
	writeFile(t, filepath.Join(tk.Worktree, "b.txt"), "b\n")
	e.gitIn(tk.Worktree, "add", "b.txt")
	e.gitIn(tk.Worktree, "commit", "-q", "-m", "work")
	e.gitIn(tk.Worktree, "push", "-q", "origin", tk.Branch)
	pushed := e.gitIn(tk.Worktree, "rev-parse", "HEAD")
	home := filepath.Dir(e.repo)
	merger := filepath.Join(home, "merger")
	e.gitIn(home, "clone", "-q", "origin.git", merger)
	e.gitIn(merger, "merge", "-q", "--squash", "origin/"+tk.Branch)
	e.gitIn(merger, "commit", "-q", "-m", "squash")
	e.gitIn(merger, "push", "-q", "origin", "main")
	if res, we := e.remove(t, wire.TaskRemoveParams{ID: tk.ID}); we != nil || res.KeptBranch != "" {
		t.Fatalf("task.remove = %+v, %v", res, we)
	}
	if out, err := exec.Command("git", "-C", e.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+tk.Branch).CombinedOutput(); err == nil {
		t.Fatalf("squash-merged branch kept: %s", out)
	}
	if got := e.gitIn(filepath.Join(home, "origin.git"), "rev-parse", "refs/heads/"+tk.Branch); got != pushed {
		t.Fatalf("origin's %s is %s, want %s", tk.Branch, got, pushed)
	}
}

// A missing task is forgotten and its worktree unregistered, its branch
// deleted when no commit would be lost and else kept and named; an
// in_place task only forgotten, its sessions untied. Neither needs a dry
// run.
func TestTaskRemoveMissingAndInPlace(t *testing.T) {
	e := newRemoveEnv(t)
	gone := e.task(t, 3, false)
	if err := os.RemoveAll(gone.Worktree); err != nil {
		t.Fatal(err)
	}
	if ts := e.tasks(t); len(ts) != 1 || ts[0].State != wire.TaskMissing {
		t.Fatalf("tasks %+v, want missing", ts)
	}
	if res, we := e.remove(t, wire.TaskRemoveParams{ID: gone.ID, DryRun: true}); we != nil || len(res.Changes)+len(res.Sessions)+res.Unpushed != 0 {
		t.Fatalf("dry run of a missing task = %+v, %v", res, we)
	}
	if res, we := e.remove(t, wire.TaskRemoveParams{ID: gone.ID}); we != nil || res.KeptBranch != "" {
		t.Fatalf("task.remove missing = %+v, %v", res, we)
	}
	if list := e.gitIn(e.repo, "worktree", "list", "--porcelain"); strings.Contains(list, gone.Worktree) {
		t.Fatalf("not unregistered:\n%s", list)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "archive-ran")); err == nil {
		t.Fatal("archive ran for a missing task")
	}
	if e.gitIn(e.repo, "branch", "--list", gone.Branch) != "" {
		t.Fatalf("branch %s of the missing task kept", gone.Branch)
	}

	worked := e.task(t, 5, false)
	e.gitIn(worked.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	if err := os.RemoveAll(worked.Worktree); err != nil {
		t.Fatal(err)
	}
	if ts := e.tasks(t); len(ts) != 1 || ts[0].State != wire.TaskMissing {
		t.Fatalf("tasks %+v, want missing", ts)
	}
	if res, we := e.remove(t, wire.TaskRemoveParams{ID: worked.ID}); we != nil || res.KeptBranch != worked.Branch {
		t.Fatalf("task.remove missing with a commit = %+v, %v; want kept_branch %s", res, we, worked.Branch)
	}
	var kept string
	e.a.waitFor(t, "missing task.removed", taskRemoved(worked.ID, &kept))
	if kept != worked.Branch {
		t.Fatalf("task.removed kept_branch %q, want %q", kept, worked.Branch)
	}
	e.gitIn(e.repo, "rev-parse", "--verify", "refs/heads/"+worked.Branch)

	ip := e.task(t, 4, true)
	sid := e.shell(t, e.repo, ip.ID, ip.ID)
	if _, we := e.remove(t, wire.TaskRemoveParams{ID: ip.ID}); we != nil {
		t.Fatalf("task.remove in_place: %v", we)
	}
	e.a.waitFor(t, "in_place task.removed", taskRemoved(ip.ID, &kept))
	e.a.waitFor(t, "session untied, still live", func(m *wire.Msg) bool {
		return sessionUpdated(m, sid, func(s wire.Session) bool { return s.TaskID == "" && s.ExitCode == nil })
	})
	if ts := e.tasks(t); len(ts) != 0 {
		t.Fatalf("tasks after removal: %+v", ts)
	}
}

// A task whose worktree folder was moved away (a volume unplugged) is
// missing; task.create of it answers that, starting nothing. With the
// folder back task.create finds it ready again and opens it as usual.
func TestTaskMissingComesBack(t *testing.T) {
	e := newRemoveEnv(t)
	tk := e.task(t, 5, false)
	if err := os.Rename(tk.Worktree, tk.Worktree+"-away"); err != nil {
		t.Fatal(err)
	}
	if ts := e.tasks(t); len(ts) != 1 || ts[0].State != wire.TaskMissing {
		t.Fatalf("tasks %+v, want missing", ts)
	}
	create := func() wire.TaskCreateResult {
		var res wire.TaskCreateResult
		e.a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{Repo: e.repo, Kind: wire.TaskKindIssue, Number: 5, Command: []string{"claude"}}, &res)
		return res
	}
	if res := create(); !res.Existed || res.Task.State != wire.TaskMissing || res.Session != nil {
		t.Fatalf("task.create while away = %+v", res)
	}
	if err := os.Rename(tk.Worktree+"-away", tk.Worktree); err != nil {
		t.Fatal(err)
	}
	if res := create(); !res.Existed || res.Task.ID != tk.ID || res.Task.State != wire.TaskReady {
		t.Fatalf("task.create with the folder back = %+v", res)
	}
	if ts := e.tasks(t); len(ts) != 1 || ts[0].State != wire.TaskReady {
		t.Fatalf("tasks %+v, want ready", ts)
	}
}
