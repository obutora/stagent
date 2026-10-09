//go:build !windows

package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const fakeExe = "/opt/stagent/stagent"

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// checkout clones a repository whose default branch is trunk into
// <tmp>/app (symlinks resolved) and returns its path.
func checkout(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(base, "seed")
	gitRun(t, base, "init", "-q", "-b", "trunk", seed)
	gitRun(t, seed, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, base, "clone", "-q", "--bare", seed, "origin.git")
	gitRun(t, base, "clone", "-q", "origin.git", "app")
	return filepath.Join(base, "app")
}

// taskDaemon is a fake daemon answering task.list with tasks and
// task.register with reg (its params recorded).
type taskDaemon struct {
	mu       sync.Mutex
	tasks    []wire.Task
	reg      wire.TaskRegisterResult
	calls    []string
	register wire.TaskRegisterParams
	progress []wire.TaskProgressParams
}

func (d *taskDaemon) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, m.Method)
	switch m.Method {
	case wire.MethodTaskList:
		return wire.TaskListResult{Tasks: d.tasks}, nil
	case wire.MethodTaskRegister:
		if err := rpc.Decode(m, &d.register); err != nil {
			return nil, err
		}
		res := d.reg
		if !res.Existed {
			res.Task.Repo, res.Task.Worktree, res.Task.InPlace = d.register.Repo, d.register.Worktree, d.register.InPlace
			if res.Task.InPlace {
				res.Task.Worktree = res.Task.Repo
			}
		}
		return res, nil
	case wire.MethodTaskProgress:
		var p wire.TaskProgressParams
		rpc.Decode(m, &p)
		d.progress = append(d.progress, p)
	}
	return struct{}{}, nil
}

// spawned is a holder start the bridge asked for.
type spawned struct {
	args []string
	dir  string
}

// startTaskBridge runs a bridge whose holder starts are recorded and
// answered by fake holders, against a fake daemon d.
func startTaskBridge(t *testing.T, d *taskDaemon, opts ...func(*Bridge)) (*app, func() []spawned) {
	t.Helper()
	t.Setenv(daemonclient.EnvExe, fakeExe)
	var mu sync.Mutex
	var starts []spawned
	var l interface{ HolderAddr(string) string }
	opts = append(opts, func(b *Bridge) {
		l = b.l
		b.spawnProc = func(exe string, args []string, dir string, env []string, logPath string) (int, error) {
			id, cwd := args[slices.Index(args, "--id")+1], args[slices.Index(args, "--cwd")+1]
			command := args[slices.Index(args, "--")+1:]
			mu.Lock()
			starts = append(starts, spawned{args: append([]string{exe}, args...), dir: dir})
			mu.Unlock()
			serveFake(t, l.HolderAddr(id), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
				return wire.Session{ID: id, Cwd: cwd, Command: command, Harness: wire.DetectHarness(command)}, nil
			})
			return 0, nil
		}
	})
	lay, a := startBridge(t, opts...)
	serveFake(t, lay.DaemonAddr, d.handle)
	return a, func() []spawned {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(starts)
	}
}

// A new issue task: registered with the canonical checkout, the default
// branch <N>-<slug> from the default branch, and its place next to the
// checkout, made empty before the prepared session starts there running
// `stagent task run`.
func TestTaskCreateStartsPreparedSession(t *testing.T) {
	repo := checkout(t)
	d := &taskDaemon{reg: wire.TaskRegisterResult{Task: wire.Task{ID: "task1", State: wire.TaskPreparing}}}
	a, starts := startTaskBridge(t, d)

	var res wire.TaskCreateResult
	if e := a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
		Repo: repo + "/", Kind: wire.TaskKindIssue, Number: 493, Title: "Windows GitHub icon", URL: "https://github.com/o/r/issues/493",
		Command: []string{"claude", "Issue #493"}, Cols: 100, Rows: 30,
	}, &res); e != nil {
		t.Fatal(e)
	}
	place := filepath.Join(filepath.Dir(repo), "app-493")
	reg := d.register
	if reg.Repo != repo || reg.Branch != "493-windows-github-icon" || reg.Base != "trunk" || reg.Worktree != place ||
		reg.InPlace || reg.SessionID == "" || reg.Title != "Windows GitHub icon" {
		t.Fatalf("task.register %+v", reg)
	}
	if empty, err := os.ReadDir(place); err != nil || len(empty) != 0 {
		t.Fatalf("task place %v, %v", empty, err)
	}
	s := starts()
	if len(s) != 1 {
		t.Fatalf("holder starts %v", s)
	}
	want := []string{fakeExe, "run", "--detached", "--id", reg.SessionID, "--cols", "100", "--rows", "30", "--cwd", place,
		"--task=task1", "--", fakeExe, "task", "run", "task1", "--", "claude", "Issue #493"}
	if !slices.Equal(s[0].args, want) {
		t.Fatalf("holder args\n%q\nwant\n%q", s[0].args, want)
	}
	if res.Existed || res.Task.ID != "task1" || res.Session == nil || res.Session.ID != reg.SessionID ||
		res.Session.Cwd != place || res.Session.Harness != wire.HarnessClaude {
		t.Fatalf("result %+v session %+v", res, res.Session)
	}
}

// A task that exists is answered as it is: nothing starts, and a retry
// keeps its branch.
func TestTaskCreateExisting(t *testing.T) {
	repo := checkout(t)
	known := wire.Task{ID: "task1", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-old-title", Base: "trunk", State: wire.TaskReady}
	d := &taskDaemon{tasks: []wire.Task{known}, reg: wire.TaskRegisterResult{Task: known, Existed: true}}
	a, starts := startTaskBridge(t, d)
	var res wire.TaskCreateResult
	if e := a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
		Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Title: "New title", Command: []string{"claude"},
	}, &res); e != nil {
		t.Fatal(e)
	}
	if !res.Existed || res.Task.ID != "task1" || res.Session != nil || len(starts()) != 0 {
		t.Fatalf("result %+v, starts %v", res, starts())
	}
	if d.register.Branch != "5-old-title" {
		t.Fatalf("registered branch %q", d.register.Branch)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(repo), "app-5")); !os.IsNotExist(err) {
		t.Fatalf("task place made for an existing task: %v", err)
	}
}

// in_place: the session starts in the checkout itself with the in_place
// options; no command opens the login shell.
func TestTaskCreateInPlaceTerminal(t *testing.T) {
	repo := checkout(t)
	t.Setenv("SHELL", "/bin/bash")
	d := &taskDaemon{reg: wire.TaskRegisterResult{Task: wire.Task{ID: "task2", State: wire.TaskPreparing}}}
	a, starts := startTaskBridge(t, d)
	var res wire.TaskCreateResult
	if e := a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
		Repo: repo, Kind: wire.TaskKindIssue, Number: 2, Title: "Here", InPlace: true, SwitchBranch: true, Setup: true,
	}, &res); e != nil {
		t.Fatal(e)
	}
	if !d.register.InPlace || d.register.Branch != "2-here" {
		t.Fatalf("task.register %+v", d.register)
	}
	s := starts()
	if len(s) != 1 {
		t.Fatalf("holder starts %v", s)
	}
	args := s[0].args
	if cwd := args[slices.Index(args, "--cwd")+1]; cwd != repo {
		t.Fatalf("cwd %q", cwd)
	}
	want := []string{fakeExe, "task", "run", "--switch-branch", "--setup", "task2", "--", "/bin/bash", "-l"}
	if got := args[slices.Index(args, "--")+1:]; !slices.Equal(got, want) {
		t.Fatalf("command %q, want %q", got, want)
	}
}

// in_place without switching records the checkout's current branch.
func TestTaskCreateInPlaceStays(t *testing.T) {
	repo := checkout(t)
	d := &taskDaemon{reg: wire.TaskRegisterResult{Task: wire.Task{ID: "task3"}}}
	a, _ := startTaskBridge(t, d)
	if e := a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
		Repo: repo, Kind: wire.TaskKindIssue, Number: 3, Title: "x", InPlace: true, Command: []string{"codex"},
	}, nil); e != nil {
		t.Fatal(e)
	}
	if d.register.Branch != "trunk" {
		t.Fatalf("registered branch %q", d.register.Branch)
	}
}

// A PR's branch is its head branch, as gh reports it.
func TestTaskCreatePR(t *testing.T) {
	repo := checkout(t)
	bin := t.TempDir()
	gh := "#!/bin/sh\n[ \"$1 $2 $3\" = 'pr view 7' ] || exit 1\necho '{\"headRefName\":\"feature/login\",\"baseRefName\":\"release\"}'\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	d := &taskDaemon{reg: wire.TaskRegisterResult{Task: wire.Task{ID: "task7"}}}
	a, _ := startTaskBridge(t, d)
	if e := a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
		Repo: repo, Kind: wire.TaskKindPR, Number: 7, Title: "Login", Branch: "ignored", Command: []string{"claude"},
	}, nil); e != nil {
		t.Fatal(e)
	}
	if d.register.Branch != "feature/login" || d.register.Base != "release" || d.register.Kind != wire.TaskKindPR {
		t.Fatalf("task.register %+v", d.register)
	}
}

// Refused before anything is registered or started: a coding agent's
// process tree (agent_refused), an invalid branch or a repo that is no
// checkout (bad_request).
func TestTaskCreateRefusals(t *testing.T) {
	repo := checkout(t)
	d := &taskDaemon{}
	a, starts := startTaskBridge(t, d, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	p := wire.TaskCreateParams{Repo: repo, Kind: wire.TaskKindIssue, Number: 1, Command: []string{"claude"}}
	if e := a.call(t, wire.MethodTaskCreate, p, nil); e == nil || e.Code != wire.ErrAgentRefused {
		t.Fatalf("from an agent: %v", e)
	}

	d2 := &taskDaemon{}
	a2, starts2 := startTaskBridge(t, d2)
	bad := p
	bad.Branch = "two words"
	if e := a2.call(t, wire.MethodTaskCreate, bad, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("invalid branch: %v", e)
	}
	bad = p
	bad.Repo = t.TempDir()
	if e := a2.call(t, wire.MethodTaskCreate, bad, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("no checkout: %v", e)
	}
	if slices.Contains(d.calls, wire.MethodTaskRegister) || slices.Contains(d2.calls, wire.MethodTaskRegister) || len(starts())+len(starts2()) != 0 {
		t.Fatalf("daemon calls %v %v, starts %v %v", d.calls, d2.calls, starts(), starts2())
	}
}
