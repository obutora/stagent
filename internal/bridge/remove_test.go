//go:build !windows

package bridge

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/gittest"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// watchers are the connections that called watch on a fake daemon.
type watchers struct {
	mu    sync.Mutex
	conns []*rpc.Conn
}

func (w *watchers) add(c *rpc.Conn) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.conns = append(w.conns, c)
}

func (w *watchers) push(method string, params any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.conns {
		c.Notify(method, params)
	}
}

// sessionEnded pushes the end of session id (exit code) to w.
func (w *watchers) sessionEnded(id string, code int) {
	w.push(wire.NotifySessionUpdated, wire.Session{ID: id, State: wire.StateExited, ExitCode: &code})
}

// runRemovals runs the sessions the bridge starts — `stagent task …`, a
// removal — in this process behind a fake holder, as their holder would,
// and tells ended each one's exit code once it is over. They outlive the
// bridge, as sessions do; the test waits for them (at most 10 s) before
// it ends.
func runRemovals(t *testing.T, ended func(id string, code int)) func(*Bridge) {
	var wg sync.WaitGroup
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Log("a removal session did not end")
		}
	})
	return func(b *Bridge) {
		l := b.l
		b.spawnProc = func(exe string, args []string, dir string, env []string, logPath string) (int, error) {
			id, cwd := args[slices.Index(args, "--id")+1], args[slices.Index(args, "--cwd")+1]
			command := args[slices.Index(args, "--")+1:]
			if len(command) < 3 || command[1] != "task" {
				return 0, fmt.Errorf("not a removal: %q", command)
			}
			serveFake(t, l.HolderAddr(id), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
				return wire.Session{ID: id, Cwd: cwd, Command: command}, nil
			})
			wg.Add(1)
			go func() {
				defer wg.Done()
				ended(id, task.Main(command[2:]))
			}()
			return 0, nil
		}
	}
}

// holdGit puts a git first on PATH that, while the file hold exists,
// waits in `git worktree remove` (creating the file held once there),
// and is git otherwise. The hold goes when the test ends.
func holdGit(t *testing.T) (hold, held string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	bin := t.TempDir()
	hold, held = filepath.Join(bin, "hold"), filepath.Join(bin, "held")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1 $2" = "worktree remove" ] && [ -f '%s' ]; then
  : > '%s'
  while [ -f '%s' ]; do sleep 0.05; done
fi
exec '%s' "$@"
`, hold, held, hold, real)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hold, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(hold) })
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return hold, held
}

// eventually waits up to 10 s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// removeDaemon is a fake daemon for task.remove: one task, its sessions
// (each with a fake holder whose hangup ends it, then runs onHangup), the
// stages reported and whether the record went (and the branch it kept;
// task.removed to its watchers). The bridge's removal sessions run in the
// test (runRemovals).
type removeDaemon struct {
	t        *testing.T
	l        *paths.Layout
	mu       sync.Mutex
	task     wire.Task
	sessions []wire.Session
	signaled []string
	stages   []wire.TaskProgressParams
	forgot   bool
	kept     string // task.forget's kept_branch
	stopErr  error  // the answer to task.progress stop
	onHangup func(d *removeDaemon, id string)
	watchers watchers
}

func startRemoveDaemon(t *testing.T, tk wire.Task) (*removeDaemon, *app) {
	t.Helper()
	d := &removeDaemon{t: t, task: tk}
	l, a := startBridge(t, runRemovals(t, d.watchers.sessionEnded))
	d.l = l
	serveFake(t, l.DaemonAddr, d.handle)
	return d, a
}

func (d *removeDaemon) gone() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.forgot
}

// addSession adds a live session of the task, its holder answering
// session.signal (locked by the caller or before the bridge runs).
func (d *removeDaemon) addSession(id string) {
	d.sessions = append(d.sessions, wire.Session{ID: id, TaskID: d.task.ID, State: wire.StateIdle})
	serveFake(d.t, d.l.HolderAddr(id), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodSessionSignal {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.signaled = append(d.signaled, id)
			for i := range d.sessions {
				if d.sessions[i].ID == id {
					code := 0
					d.sessions[i].ExitCode, d.sessions[i].State = &code, wire.StateExited
				}
			}
			if d.onHangup != nil {
				d.onHangup(d, id)
			}
		}
		return struct{}{}, nil
	})
}

func (d *removeDaemon) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch m.Method {
	case wire.MethodWatch:
		d.watchers.add(c)
	case wire.MethodTaskList:
		if d.forgot {
			return wire.TaskListResult{Tasks: []wire.Task{}}, nil
		}
		return wire.TaskListResult{Tasks: []wire.Task{d.task}}, nil
	case wire.MethodSessionsList:
		return wire.SessionsListResult{Sessions: slices.Clone(d.sessions)}, nil
	case wire.MethodTaskProgress:
		var p wire.TaskProgressParams
		rpc.Decode(m, &p)
		if p.Stage == wire.TaskStageStop && p.Error == "" && d.stopErr != nil {
			return nil, d.stopErr
		}
		d.stages = append(d.stages, p)
	case wire.MethodTaskForget:
		var r wire.TaskRemoved
		rpc.Decode(m, &r)
		d.forgot, d.kept = true, r.KeptBranch
		d.watchers.push(wire.NotifyTaskRemoved, r)
	}
	return struct{}{}, nil
}

// Closing the bridge (the app's SSH connection going) in the middle of a
// removal does not stop it: the worktree goes, the branch is decided and
// the record forgotten after the bridge has exited.
func TestTaskRemoveOutlivesTheBridge(t *testing.T) {
	repo := checkout(t)
	wt := filepath.Join(filepath.Dir(repo), "app-1")
	addWorktree(t, repo, wt, "1-x")
	gitRun(t, wt, "commit", "-q", "--allow-empty", "-m", "work")
	stubGH(t, stubcmd.Rule{Args: "pr view 1-x *", Stderr: "no pull requests found\n", Exit: 1})
	hold, held := holdGit(t)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Kind: wire.TaskKindIssue, Number: 1, Branch: "1-x", Worktree: wt, State: wire.TaskReady})

	a.goCall(wire.MethodTaskRemove, "remove", wire.TaskRemoveParams{ID: "t1", SkipArchive: true})
	eventually(t, "git worktree remove", func() bool { _, err := os.Stat(held); return err == nil })
	a.hangUp(t)
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the record forgotten", d.gone)
	if listed(t, repo, wt) || !gone(t, wt) {
		t.Fatal("worktree left")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.kept != "1-x" || gitOut(t, repo, "rev-parse", "--verify", "-q", "refs/heads/1-x") == "" {
		t.Fatalf("kept branch %q, want the unmerged 1-x kept", d.kept)
	}
}

// A preparing in_place task is busy unless its sessions may be stopped;
// with stop_sessions they are — one bound to the task meanwhile too —
// and the record goes once the task failed.
func TestTaskRemovePreparingInPlace(t *testing.T) {
	const prep, late = "00000000000000a1", "00000000000000a2"
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: "/w/app", Worktree: "/w/app", InPlace: true, State: wire.TaskPreparing, SessionID: prep})
	d.addSession(prep)
	d.onHangup = func(d *removeDaemon, id string) {
		if id == prep {
			d.task.State, d.task.Error = wire.TaskFailed, wire.TaskErrInterrupted
			d.addSession(late)
		}
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("task.remove of a preparing task: %v, want %s", e, wire.ErrBusy)
	}
	if d.forgot || len(d.signaled) != 0 {
		t.Fatalf("busy removal forgot %v, signaled %v", d.forgot, d.signaled)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1", StopSessions: true}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || !slices.Equal(d.signaled, []string{prep, late}) {
		t.Fatalf("forgot %v, signaled %v", d.forgot, d.signaled)
	}
}

// Another removal that got to the task first (the daemon answers stop
// busy) makes task.remove busy, and the record stays.
func TestTaskRemoveRacedIsBusy(t *testing.T) {
	repo := checkout(t)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: repo + "-1", Branch: "1-x", State: wire.TaskReady})
	d.stopErr = wire.Errorf(wire.ErrBusy, "task t1 is being removed")
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("task.remove raced: %v, want %s", e, wire.ErrBusy)
	}
	if d.forgot || len(d.stages) != 0 {
		t.Fatalf("forgot %v, stages %+v", d.forgot, d.stages)
	}
}

// A task whose source checkout and worktree were both removed by hand (a
// failed one here) only loses its record: no stage runs, as git cannot
// run in a checkout that is gone, and its branch — not checked, not
// deleted — is named as kept. With the worktree still there the record
// stays and the worktree stage fails.
func TestTaskRemoveGoneCheckout(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "app")
	tk := wire.Task{ID: "t1", Repo: repo, Worktree: repo + "-7", Branch: "7-x", State: wire.TaskFailed, Stage: wire.TaskStageWorktree}
	d, a := startRemoveDaemon(t, tk)
	var res wire.TaskRemoveResult
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1", DryRun: true}, &res); e != nil || len(res.Changes)+len(res.Sessions)+res.Unpushed != 0 {
		t.Fatalf("dry run: %+v, %v", res, e)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, &res); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	if !d.forgot || len(d.stages) != 0 || d.kept != "7-x" || res.KeptBranch != "7-x" {
		t.Fatalf("forgot %v (kept %q), stages %+v, result %+v", d.forgot, d.kept, d.stages, res)
	}
	d.mu.Unlock()

	if err := os.Mkdir(tk.Worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	d, a = startRemoveDaemon(t, tk)
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil {
		t.Fatal("removed a task whose worktree is still there without its checkout")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forgot {
		t.Fatal("forgot a task whose worktree is still there")
	}
}

// A missing task's removal handles its branch as every removal does: with
// all its commits' changes in the base (squash-merged here, gh knowing no
// pull request) it is deleted; with a commit found nowhere else it is
// kept and named, in the reply and in task.forget.
func TestTaskRemoveMissingBranch(t *testing.T) {
	for _, merged := range []bool{true, false} {
		repo := checkout(t)
		stubGH(t, stubcmd.Rule{Args: "pr view 7-x *", Stderr: "no pull requests found\n", Exit: 1})
		wt := filepath.Join(filepath.Dir(repo), "app-7")
		addWorktree(t, repo, wt, "7-x")
		writeTree(t, wt, map[string]string{"b.txt": "b\n"})
		gitRun(t, wt, "add", "b.txt")
		gitRun(t, wt, "commit", "-q", "-m", "work")
		if merged {
			gitRun(t, repo, "merge", "-q", "--squash", "7-x")
			gitRun(t, repo, "commit", "-q", "-m", "squash")
		}
		if err := os.RemoveAll(wt); err != nil {
			t.Fatal(err)
		}
		d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: wt, Branch: "7-x", Base: "trunk", State: wire.TaskMissing})
		var res wire.TaskRemoveResult
		if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, &res); e != nil {
			t.Fatal(e)
		}
		has := gitOut(t, repo, "branch", "--list", "7-x") != ""
		d.mu.Lock()
		forgot, kept := d.forgot, d.kept
		d.mu.Unlock()
		want := "7-x"
		if merged {
			want = ""
		}
		if !forgot || kept != want || res.KeptBranch != want || has != !merged || listed(t, repo, wt) {
			t.Fatalf("merged %v: forgot %v (kept %q), result %+v, branch there %v, worktree listed %v", merged, forgot, kept, res, has, listed(t, repo, wt))
		}
	}
}

// halfMade leaves at wt a worktree of repo on a new branch whose `git
// worktree add` was killed: no files, locked "initializing".
func halfMade(t *testing.T, repo, wt, branch string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "a.txt")
	gitRun(t, repo, "commit", "-q", "-m", "a file")
	gittest.KilledAdd(t, repo, "-b", branch, wt)
}

// A task whose worktree a killed `git worktree add` left half made: while
// its preparing session lives the worktree stays (busy without
// stop_sessions); then it goes without force or archive — nothing was
// checked out, so the dry run has no changes — and so does the record.
func TestTaskRemoveKilledAdd(t *testing.T) {
	const prep = "00000000000000a1"
	repo := checkout(t)
	if err := os.WriteFile(filepath.Join(repo, "orca.yaml"), []byte("scripts:\n  archive: exit 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := repo + "-3"
	halfMade(t, repo, wt, "3-x")
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: wt, Branch: "3-x", State: wire.TaskPreparing, Stage: wire.TaskStageWorktree, SessionID: prep})
	d.addSession(prep)
	d.onHangup = func(d *removeDaemon, id string) {
		d.task.State, d.task.Error = wire.TaskFailed, wire.TaskErrInterrupted
	}
	var dry wire.TaskRemoveResult
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1", DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	if len(dry.Changes) != 0 || !slices.Equal(dry.Sessions, []string{prep}) {
		t.Fatalf("dry run %+v", dry)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("task.remove while preparing: %v, want %s", e, wire.ErrBusy)
	}
	d.mu.Lock()
	if d.forgot || len(d.signaled) != 0 || !listed(t, repo, wt) {
		t.Fatalf("busy removal forgot %v, signaled %v, or removed the worktree", d.forgot, d.signaled)
	}
	d.mu.Unlock()
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1", StopSessions: true}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || !slices.Equal(d.signaled, []string{prep}) || listed(t, repo, wt) || !gone(t, wt) {
		t.Fatalf("forgot %v, signaled %v, listed %v", d.forgot, d.signaled, listed(t, repo, wt))
	}
}

// missingWorktrees adds three worktrees of repo — one, two and a locked
// one — and removes their directories (a volume taken away).
func missingWorktrees(t *testing.T, repo string) (one, two, locked string) {
	t.Helper()
	base := filepath.Dir(repo)
	one, two, locked = filepath.Join(base, "app-1"), filepath.Join(base, "app-2"), filepath.Join(base, "app-3")
	addWorktree(t, repo, one, "1-one")
	addWorktree(t, repo, two, "2-two")
	addWorktree(t, repo, locked, "3-locked")
	gitRun(t, repo, "worktree", "lock", "--reason", "on a volume", locked)
	for _, p := range []string{one, two, locked} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	return one, two, locked
}

// Removing a missing task unregisters its own worktree only: another
// worktree whose directory is gone too, and a locked one, stay registered.
func TestTaskRemoveMissingUnregistersItAlone(t *testing.T) {
	repo := checkout(t)
	one, two, locked := missingWorktrees(t, repo)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: one, Branch: "1-one", State: wire.TaskMissing})
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || listed(t, repo, one) || !listed(t, repo, two) || !listed(t, repo, locked) {
		t.Fatalf("forgot %v:\n%s", d.forgot, gitOut(t, repo, "worktree", "list"))
	}
}

// A missing task whose worktree is back (the volume reconnected) keeps
// the worktree: only the record goes.
func TestTaskRemoveMissingKeepsWorktreeThatIsBack(t *testing.T) {
	repo := checkout(t)
	wt := filepath.Join(filepath.Dir(repo), "app-1")
	addWorktree(t, repo, wt, "1-one")
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: wt, Branch: "1-one", State: wire.TaskMissing})
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || !listed(t, repo, wt) || gone(t, wt) {
		t.Fatalf("forgot %v:\n%s", d.forgot, gitOut(t, repo, "worktree", "list"))
	}
}

// A coding agent's process tree removes nothing, not even a dry run: the
// daemon is not asked and nothing starts.
func TestTaskRemoveRefusedToAgents(t *testing.T) {
	d := &taskDaemon{tasks: []wire.Task{{ID: "t1", Repo: "/w/app", Worktree: "/w/app-1", State: wire.TaskReady}}}
	a, starts := startTaskBridge(t, d, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	for _, p := range []wire.TaskRemoveParams{{ID: "t1", DryRun: true}, {ID: "t1", Force: true, StopSessions: true}} {
		if e := a.call(t, wire.MethodTaskRemove, p, nil); e == nil || e.Code != wire.ErrAgentRefused {
			t.Fatalf("task.remove %+v from an agent: %v", p, e)
		}
	}
	if len(d.calls) != 0 || len(starts()) != 0 {
		t.Fatalf("daemon calls %v, starts %v", d.calls, starts())
	}
}

// nestedWorktree adds `claude --worktree`'s worktree x of repo inside
// outer, in .claude/worktrees, which the checkout ignores, with an
// uncommitted file in it; it returns its path.
func nestedWorktree(t *testing.T, repo, outer string) string {
	t.Helper()
	exclude := filepath.Join(gitOut(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir"), "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte(".claude/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, ".claude", "worktrees", "x")
	addWorktree(t, outer, inner, "x")
	writeTree(t, inner, map[string]string{"wip.txt": "unsaved"})
	return inner
}

// A task whose worktree holds another worktree lists it in the dry run
// and is refused (refused) whatever the flags, also when no dry run came
// first (an app that does not know the reason): nothing is stopped, no
// stage runs and both worktrees stay. With the inner one removed the
// task is cleaned up as usual.
func TestTaskRemoveRefusesNestedWorktree(t *testing.T) {
	const sid = "00000000000000c1"
	repo := checkout(t)
	wt := repo + "-5"
	addWorktree(t, repo, wt, "5-fix")
	inner := nestedWorktree(t, repo, wt)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady})
	d.addSession(sid)

	var dry wire.TaskRemoveResult
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	want := []wire.RemoveRefusal{{Kind: wire.RefusalNestedWorktrees, Paths: []string{inner}}}
	if !reflect.DeepEqual(dry.Refusals, want) || !slices.Equal(dry.Sessions, []string{sid}) {
		t.Fatalf("dry run %+v, want refusals %+v", dry, want)
	}
	e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", Force: true, StopSessions: true}, nil)
	if e == nil || e.Code != wire.ErrRefused || !strings.Contains(e.Message, inner) {
		t.Fatalf("task.remove of a worktree holding another: %v, want %s", e, wire.ErrRefused)
	}
	d.mu.Lock()
	if d.forgot || len(d.signaled) != 0 || len(d.stages) != 0 {
		t.Fatalf("refused removal forgot %v, signaled %v, stages %+v", d.forgot, d.signaled, d.stages)
	}
	d.mu.Unlock()
	if !listed(t, repo, wt) || !listed(t, repo, inner) || gone(t, filepath.Join(inner, "wip.txt")) {
		t.Fatal("a refused removal removed a worktree")
	}

	gitRun(t, repo, "worktree", "remove", "--force", inner)
	var again wire.TaskRemoveResult
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", DryRun: true}, &again); e != nil || len(again.Refusals) != 0 {
		t.Fatalf("dry run without the inner worktree: %+v, %v", again, e)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", StopSessions: true}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || listed(t, repo, wt) || !gone(t, wt) {
		t.Fatalf("forgot %v, worktree listed %v", d.forgot, listed(t, repo, wt))
	}
}

// A task whose worktree has a submodule (git worktree remove refuses it)
// or that the user locked lists why in the dry run and is refused
// (refused) whatever the flags, before its sessions are stopped.
func TestTaskRemoveRefusesSubmodulesAndLock(t *testing.T) {
	const sid = "00000000000000c2"
	repo := checkout(t)
	wt := repo + "-5"
	addWorktree(t, repo, wt, "5-fix")
	src := filepath.Join(filepath.Dir(repo), "seed")
	gitRun(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "sub")
	gitRun(t, wt, "commit", "-q", "-m", "submodule")
	gitRun(t, repo, "worktree", "lock", "--reason", "mine", wt)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady})
	d.addSession(sid)

	var dry wire.TaskRemoveResult
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	want := []wire.RemoveRefusal{{Kind: wire.RefusalSubmodules, Paths: []string{filepath.Join(wt, "sub")}}, {Kind: wire.RefusalLocked}}
	if !reflect.DeepEqual(dry.Refusals, want) {
		t.Fatalf("dry run %+v, want refusals %+v", dry, want)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", Force: true, StopSessions: true}, nil); e == nil || e.Code != wire.ErrRefused {
		t.Fatalf("task.remove: %v, want %s", e, wire.ErrRefused)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forgot || len(d.signaled) != 0 || len(d.stages) != 0 || !listed(t, repo, wt) {
		t.Fatalf("refused removal forgot %v, signaled %v, stages %+v", d.forgot, d.signaled, d.stages)
	}
}

// A refusal that comes after the check — the user locks the worktree while
// the task's sessions stop — is found by the removal's own check in its
// session and answered refused, as the check would: nothing is removed.
func TestTaskRemoveRefusedInItsSession(t *testing.T) {
	const sid = "00000000000000c3"
	repo := checkout(t)
	wt := repo + "-5"
	addWorktree(t, repo, wt, "5-fix")
	d, a := startRemoveDaemon(t, wire.Task{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady})
	d.addSession(sid)
	d.onHangup = func(d *removeDaemon, id string) { gitRun(t, repo, "worktree", "lock", "--reason", "mine", wt) }
	e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t5", StopSessions: true}, nil)
	if e == nil || e.Code != wire.ErrRefused || !strings.Contains(e.Message, "locked") {
		t.Fatalf("task.remove: %v, want %s", e, wire.ErrRefused)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.forgot || !listed(t, repo, wt) || gone(t, wt) {
		t.Fatalf("refused removal forgot %v or removed the worktree", d.forgot)
	}
}
