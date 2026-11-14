//go:build !windows

package bridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/gittest"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

// worktreeDaemon is a fake daemon for worktree.*: task records (task.forget
// drops one, telling its watchers), sessions (each live one with a fake
// holder whose hangup ends it), and config.set merging the patch's
// top-level keys into config.json. The bridge's removal sessions run in
// the test (runRemovals).
type worktreeDaemon struct {
	t        *testing.T
	l        *paths.Layout
	mu       sync.Mutex
	tasks    []wire.Task
	sessions []wire.Session
	signaled []string
	forgot   []string
	sets     []map[string]json.RawMessage
	watchers watchers
}

func startWorktreeBridge(t *testing.T, cfg wire.Config, opts ...func(*Bridge)) (*worktreeDaemon, *app) {
	t.Helper()
	d := &worktreeDaemon{t: t}
	opts = append(opts, runRemovals(t, d.watchers.sessionEnded), func(b *Bridge) {
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(b.l.Config, data, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	l, a := startBridge(t, opts...)
	d.l = l
	serveFake(t, l.DaemonAddr, d.handle)
	return d, a
}

// addSession adds a live session working in cwd (locked by the caller or
// before the bridge runs).
func (d *worktreeDaemon) addSession(id, cwd string) {
	d.sessions = append(d.sessions, wire.Session{ID: id, Cwd: cwd, State: wire.StateIdle})
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
		}
		return struct{}{}, nil
	})
}

func (d *worktreeDaemon) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch m.Method {
	case wire.MethodWatch:
		d.watchers.add(c)
	case wire.MethodTaskList:
		return wire.TaskListResult{Tasks: slices.Clone(d.tasks)}, nil
	case wire.MethodSessionsList:
		return wire.SessionsListResult{Sessions: slices.Clone(d.sessions)}, nil
	case wire.MethodTaskForget:
		var r wire.TaskRemoved
		rpc.Decode(m, &r)
		d.forgot = append(d.forgot, r.ID)
		d.tasks = slices.DeleteFunc(d.tasks, func(t wire.Task) bool { return t.ID == r.ID })
		d.watchers.push(wire.NotifyTaskRemoved, r)
	case wire.MethodConfigSet:
		var p wire.ConfigSetParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		var patch map[string]json.RawMessage
		json.Unmarshal(p.Config, &patch)
		d.sets = append(d.sets, patch)
		stored := map[string]json.RawMessage{}
		if b, err := os.ReadFile(d.l.Config); err == nil {
			json.Unmarshal(b, &stored)
		}
		for k, v := range patch {
			stored[k] = v
		}
		b, _ := json.Marshal(stored)
		if err := os.WriteFile(d.l.Config, b, 0o600); err != nil {
			return nil, err
		}
		var res wire.ConfigResult
		json.Unmarshal(b, &res.Config)
		return res, nil
	}
	return struct{}{}, nil
}

// addWorktree adds a worktree of repo at path on a new branch from
// origin/trunk (no upstream).
func addWorktree(t *testing.T, repo, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "worktree", "add", "-q", "--no-track", "-b", branch, path, "origin/trunk")
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// listed reports whether git lists worktree in repo.
func listed(t *testing.T, repo, worktree string) bool {
	t.Helper()
	return strings.Contains(gitOut(t, repo, "worktree", "list", "--porcelain")+"\n", "worktree "+worktree+"\n")
}

func gone(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	return os.IsNotExist(err)
}

// worktree.list finds the worktree folder's worktree and remembers its
// repository in worktree_repos through the daemon, dropping a remembered
// one whose source checkout is gone; the same set again is not written.
func TestWorktreeListRemembersRepos(t *testing.T) {
	repo := checkout(t)
	base := filepath.Dir(repo)
	folder := filepath.Join(base, "worktrees")
	wt := filepath.Join(folder, "app", "feat")
	addWorktree(t, repo, wt, "feat")
	d, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder, WorktreeRepos: []string{filepath.Join(base, "vanished")}})

	var res wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &res); e != nil {
		t.Fatal(e)
	}
	if res.WorktreeFolder != folder || len(res.Repos) != 1 || res.Repos[0].Path != repo || res.Repos[0].DefaultBranch != "trunk" ||
		len(res.Repos[0].Worktrees) != 1 || res.Repos[0].Worktrees[0].Path != wt || res.Repos[0].Worktrees[0].Branch != "feat" {
		t.Fatalf("worktree.list %+v", res)
	}
	if len(d.sets) != 1 || string(d.sets[0]["worktree_repos"]) != `["`+repo+`"]` {
		t.Fatalf("config.set %v", d.sets)
	}
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &res); e != nil {
		t.Fatal(e)
	}
	if len(d.sets) != 1 {
		t.Fatalf("worktree_repos written again: %v", d.sets)
	}
}

// A watch folder added (an existing directory, canonical) is kept in
// config.json's worktree_watch and leads worktree.list to the repository
// of the worktree in it; a directory that does not exist is bad_request;
// removing it forgets it.
func TestWorktreeWatch(t *testing.T) {
	repo := checkout(t)
	base := filepath.Dir(repo)
	orca := filepath.Join(base, "orca")
	addWorktree(t, repo, filepath.Join(orca, "wt-x"), "x")
	d, a := startWorktreeBridge(t, wire.Config{})

	var w wire.WorktreeWatchResult
	if e := a.call(t, wire.MethodWorktreeWatch, wire.WorktreeWatchParams{Add: orca + "/"}, &w); e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(w.WatchFolders, []string{orca}) {
		t.Fatalf("watch folders %q", w.WatchFolders)
	}
	var res wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &res); e != nil {
		t.Fatal(e)
	}
	if len(res.Repos) != 1 || res.Repos[0].Path != repo || !slices.Equal(res.WatchFolders, []string{orca}) {
		t.Fatalf("worktree.list %+v", res)
	}
	if e := a.call(t, wire.MethodWorktreeWatch, wire.WorktreeWatchParams{Add: filepath.Join(base, "nowhere")}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("watching a directory that is not there: %v", e)
	}
	if e := a.call(t, wire.MethodWorktreeWatch, wire.WorktreeWatchParams{Remove: orca}, &w); e != nil {
		t.Fatal(e)
	}
	if len(w.WatchFolders) != 0 {
		t.Fatalf("watch folders after remove %q", w.WatchFolders)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if n := len(d.sets); n < 3 {
		t.Fatalf("config.set %d times", n)
	}
}

// Nothing is removed that cannot be: the source checkout is bad_request,
// a locked worktree refused (its refusal locked, as task.remove's), a path
// not in the inventory not_found. A coding agent's process tree may list
// but neither remove nor watch.
func TestWorktreeRemoveRefusals(t *testing.T) {
	repo := checkout(t)
	locked := filepath.Join(filepath.Dir(repo), "wt-locked")
	addWorktree(t, repo, locked, "locked")
	gitRun(t, repo, "worktree", "lock", "--reason", "agent at work", locked)
	_, a := startWorktreeBridge(t, wire.Config{WorktreeRepos: []string{repo}})
	for _, c := range []struct {
		path string
		code string
	}{
		{"", wire.ErrBadRequest},
		{repo, wire.ErrBadRequest},
		{locked, wire.ErrRefused},
		{filepath.Join(filepath.Dir(repo), "nowhere"), wire.ErrNotFound},
	} {
		if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: c.path, Force: true}, nil); e == nil || e.Code != c.code {
			t.Errorf("worktree.remove %q: %v, want %s", c.path, e, c.code)
		}
	}
	if !listed(t, repo, locked) {
		t.Fatal("the locked worktree went")
	}

	_, agent := startWorktreeBridge(t, wire.Config{WorktreeRepos: []string{repo}}, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	if e := agent.call(t, wire.MethodWorktreeList, struct{}{}, nil); e != nil {
		t.Fatalf("worktree.list from an agent: %v", e)
	}
	if e := agent.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: locked, DryRun: true}, nil); e == nil || e.Code != wire.ErrAgentRefused {
		t.Fatalf("worktree.remove from an agent: %v", e)
	}
	if e := agent.call(t, wire.MethodWorktreeWatch, wire.WorktreeWatchParams{Add: repo}, nil); e == nil || e.Code != wire.ErrAgentRefused {
		t.Fatalf("worktree.watch from an agent: %v", e)
	}
}

// A worktree no task has goes as a task's would: its branch, with a commit
// on no remote and no merged pull request, is kept and named, the remote
// untouched; the last worktree in <worktree folder>/<repo name> takes that
// empty folder with it.
func TestWorktreeRemoveKeepsUnmergedBranch(t *testing.T) {
	repo := checkout(t)
	stubGH(t, stubcmd.Rule{Args: "pr view feat *", Stderr: "no pull requests found\n", Exit: 1})
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "feat")
	addWorktree(t, repo, wt, "feat")
	gitRun(t, wt, "commit", "-q", "--allow-empty", "-m", "work")
	remote := gitOut(t, repo, "ls-remote", "origin")
	_, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})

	var dry wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	if dry.Unpushed != 1 || len(dry.Changes) != 0 || len(dry.Sessions) != 0 || !listed(t, repo, wt) {
		t.Fatalf("dry run %+v", dry)
	}
	var res wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt}, &res); e != nil {
		t.Fatal(e)
	}
	if res.KeptBranch != "feat" || res.RemovedParent != filepath.Join(folder, "app") {
		t.Fatalf("result %+v", res)
	}
	if listed(t, repo, wt) || !gone(t, wt) || !gone(t, filepath.Join(folder, "app")) || gone(t, folder) {
		t.Fatal("worktree or its empty folder left, or the worktree folder removed")
	}
	if gitOut(t, repo, "rev-parse", "--verify", "-q", "refs/heads/feat") == "" {
		t.Fatal("unmerged branch deleted")
	}
	if after := gitOut(t, repo, "ls-remote", "origin"); after != remote {
		t.Fatalf("remote changed:\n%s\nwas\n%s", after, remote)
	}
}

// Removing a worktree no task has goes on when the bridge (the app's SSH
// connection) goes in the middle of it: the worktree goes, its merged
// branch is deleted and the folder <worktree folder>/<repo name> it leaves
// empty is removed after the bridge has exited.
func TestWorktreeRemoveOutlivesTheBridge(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "done")
	addWorktree(t, repo, wt, "done")
	hold, held := holdGit(t)
	_, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})

	a.goCall(wire.MethodWorktreeRemove, "remove", wire.WorktreeRemoveParams{Path: wt})
	eventually(t, "git worktree remove", func() bool { _, err := os.Stat(held); return err == nil })
	a.hangUp(t)
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the worktree, its merged branch and the empty folder gone", func() bool {
		return !listed(t, repo, wt) && gone(t, wt) && gitOut(t, repo, "branch", "--list", "done") == "" && gone(t, filepath.Join(folder, "app"))
	})
}

// Removing a task's worktree from the worktree screen goes on when the
// bridge goes, as task.remove does, and the folder <worktree
// folder>/<repo name> it leaves empty goes too.
func TestWorktreeRemoveTaskOutlivesTheBridge(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "app-5")
	addWorktree(t, repo, wt, "5-fix")
	hold, held := holdGit(t)
	d, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})
	d.tasks = []wire.Task{{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady}}

	a.goCall(wire.MethodWorktreeRemove, "remove", wire.WorktreeRemoveParams{Path: wt})
	eventually(t, "git worktree remove", func() bool { _, err := os.Stat(held); return err == nil })
	a.hangUp(t)
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the record, the worktree and the empty folder gone", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.forgot) == 1 && !listed(t, repo, wt) && gone(t, wt) && gone(t, filepath.Join(folder, "app"))
	})
}

// Uncommitted changes and live sessions are shown by a dry run and refuse
// the removal (not_clean, busy) until force and stop_sessions say so; then
// the sessions are hung up first. A folder with other worktrees stays.
func TestWorktreeRemoveChangesAndSessions(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "wip")
	addWorktree(t, repo, wt, "wip")
	addWorktree(t, repo, filepath.Join(folder, "app", "other"), "other")
	writeTree(t, wt, map[string]string{"new.txt": "n", "sub/x": "x"})
	const sid = "00000000000000b1"
	d, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})
	d.mu.Lock()
	d.addSession(sid, filepath.Join(wt, "sub"))
	d.mu.Unlock()

	var dry wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(dry.Changes, []string{"?? new.txt", "?? sub/x"}) || !slices.Equal(dry.Sessions, []string{sid}) {
		t.Fatalf("dry run %+v", dry)
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, StopSessions: true}, nil); e == nil || e.Code != wire.ErrNotClean {
		t.Fatalf("with changes: %v", e)
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, Force: true}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("with a live session: %v", e)
	}
	d.mu.Lock()
	signaled := slices.Clone(d.signaled)
	d.mu.Unlock()
	if len(signaled) != 0 || !listed(t, repo, wt) {
		t.Fatalf("a refused removal signaled %v or removed the worktree", signaled)
	}
	var res wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, Force: true, StopSessions: true}, &res); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Equal(d.signaled, []string{sid}) || listed(t, repo, wt) || !gone(t, wt) {
		t.Fatalf("signaled %v, worktree listed %v", d.signaled, listed(t, repo, wt))
	}
	if res.RemovedParent != "" || gone(t, filepath.Join(folder, "app")) {
		t.Fatalf("result %+v", res)
	}
}

// A task's worktree is removed as task.remove removes it: the record goes
// too (task.forget), so the task list loses it.
func TestWorktreeRemoveTask(t *testing.T) {
	repo := checkout(t)
	wt := filepath.Join(filepath.Dir(repo), "app-5")
	addWorktree(t, repo, wt, "5-fix")
	d, a := startWorktreeBridge(t, wire.Config{})
	d.tasks = []wire.Task{{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady}}

	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Repos) != 1 || len(list.Repos[0].Worktrees) != 1 || list.Repos[0].Worktrees[0].TaskID != "t5" {
		t.Fatalf("worktree.list %+v", list)
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Equal(d.forgot, []string{"t5"}) || len(d.tasks) != 0 || listed(t, repo, wt) || !gone(t, wt) {
		t.Fatalf("forgot %v, tasks %+v", d.forgot, d.tasks)
	}
}

// A task's worktree a killed `git worktree add` left half made is listed
// as the task's, not locked, without changes or refusals, and removing it
// is the task's 片付け. Another tool's half-made worktree may be in the
// making: it stays locked and is refused (locked), as is a task's
// worktree the user locked.
func TestWorktreeRemoveKilledAdd(t *testing.T) {
	repo := checkout(t)
	base := filepath.Dir(repo)
	mine, other, kept := filepath.Join(base, "app-5"), filepath.Join(base, "orca-x"), filepath.Join(base, "app-6")
	halfMade(t, repo, mine, "5-fix")
	gittest.KilledAdd(t, repo, "-b", "x", other)
	addWorktree(t, repo, kept, "6-fix")
	gitRun(t, repo, "worktree", "lock", "--reason", "agent at work", kept)
	d, a := startWorktreeBridge(t, wire.Config{WorktreeRepos: []string{repo}})
	d.tasks = []wire.Task{
		{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: mine, State: wire.TaskFailed, Error: wire.TaskErrInterrupted},
		{ID: "t6", Repo: repo, Kind: wire.TaskKindIssue, Number: 6, Branch: "6-fix", Worktree: kept, State: wire.TaskReady},
	}

	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	entries := map[string]wire.WorktreeEntry{}
	for _, w := range list.Repos[0].Worktrees {
		entries[w.Path] = w
	}
	locked := []wire.RemoveRefusal{{Kind: wire.RefusalLocked}}
	if w := entries[mine]; w.TaskID != "t5" || w.Locked || w.LockReason != "" || w.Changes != 0 || len(w.Refusals) != 0 {
		t.Fatalf("the task's half-made worktree %+v", w)
	}
	if w := entries[other]; !w.Locked || w.LockReason != "initializing" || w.TaskID != "" || !reflect.DeepEqual(w.Refusals, locked) {
		t.Fatalf("another half-made worktree %+v", w)
	}
	if w := entries[kept]; !w.Locked || w.LockReason != "agent at work" || !reflect.DeepEqual(w.Refusals, locked) {
		t.Fatalf("the locked task worktree %+v", w)
	}

	for _, p := range []string{other, kept} {
		var dry wire.WorktreeRemoveResult
		if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: p, DryRun: true}, &dry); e != nil || !reflect.DeepEqual(dry.Refusals, locked) {
			t.Fatalf("dry run of %s: %+v, %v", p, dry, e)
		}
		if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: p, Force: true, StopSessions: true}, nil); e == nil || e.Code != wire.ErrRefused {
			t.Fatalf("worktree.remove %s: %v, want %s", p, e, wire.ErrRefused)
		}
		if !listed(t, repo, p) {
			t.Fatalf("%s went", p)
		}
	}
	var dry wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: mine, DryRun: true}, &dry); e != nil || len(dry.Changes) != 0 {
		t.Fatalf("dry run %+v, %v", dry, e)
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: mine}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Equal(d.forgot, []string{"t5"}) || listed(t, repo, mine) || !gone(t, mine) {
		t.Fatalf("forgot %v, listed %v", d.forgot, listed(t, repo, mine))
	}
}

// A worktree whose directory is gone is listed as missing, and listing
// does not prune it; removing it unregisters it alone.
func TestWorktreeRemoveMissing(t *testing.T) {
	repo := checkout(t)
	base := filepath.Dir(repo)
	one, two := filepath.Join(base, "wt-one"), filepath.Join(base, "wt-two")
	addWorktree(t, repo, one, "one")
	addWorktree(t, repo, two, "two")
	for _, p := range []string{one, two} {
		if err := os.RemoveAll(p); err != nil {
			t.Fatal(err)
		}
	}
	_, a := startWorktreeBridge(t, wire.Config{WorktreeRepos: []string{repo}})
	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Repos) != 1 || len(list.Repos[0].Worktrees) != 2 || !list.Repos[0].Worktrees[0].Missing || !list.Repos[0].Worktrees[1].Missing {
		t.Fatalf("worktree.list %+v", list)
	}
	if !listed(t, repo, one) || !listed(t, repo, two) {
		t.Fatal("worktree.list pruned")
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: one}, nil); e != nil {
		t.Fatal(e)
	}
	if listed(t, repo, one) || !listed(t, repo, two) {
		t.Fatalf("after removing %s:\n%s", one, gitOut(t, repo, "worktree", "list"))
	}
}

// Removing a missing task's worktree on the worktree screen unregisters
// it alone, as task.remove does, and forgets the task.
func TestWorktreeRemoveMissingTask(t *testing.T) {
	repo := checkout(t)
	one, two, locked := missingWorktrees(t, repo)
	d, a := startWorktreeBridge(t, wire.Config{WorktreeRepos: []string{repo}})
	d.tasks = []wire.Task{{ID: "t1", Repo: repo, Kind: wire.TaskKindIssue, Number: 1, Branch: "1-one", Worktree: one, State: wire.TaskMissing}}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: one}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Equal(d.forgot, []string{"t1"}) || listed(t, repo, one) || !listed(t, repo, two) || !listed(t, repo, locked) {
		t.Fatalf("forgot %v:\n%s", d.forgot, gitOut(t, repo, "worktree", "list"))
	}
}

// A folder of the worktree folder that is no worktree: its dry run gives
// its size and largest entries; it is removed, and so is the then empty
// <worktree folder>/<repo name>.
func TestWorktreeRemoveFolder(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	notes := filepath.Join(folder, "app", "1108-notes")
	writeTree(t, notes, map[string]string{"big.txt": strings.Repeat("x", 64<<10), "small.txt": "s"})
	_, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder, WorktreeRepos: []string{repo}})

	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	if want := []wire.WorktreeFolder{{Path: notes, Repo: "app"}}; !slices.Equal(list.Folders, want) {
		t.Fatalf("folders %+v", list.Folders)
	}
	var dry wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: notes, DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	if dry.Bytes < 64<<10 || len(dry.Entries) != 2 || dry.Entries[0].Name != "big.txt" || dry.Entries[0].Bytes < 64<<10 ||
		dry.Entries[1].Name != "small.txt" || gone(t, notes) {
		t.Fatalf("dry run %+v", dry)
	}
	// Finder's .DS_Store does not keep <worktree folder>/<repo name>.
	writeTree(t, filepath.Join(folder, "app"), map[string]string{".DS_Store": "d"})
	var res wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: notes}, &res); e != nil {
		t.Fatal(e)
	}
	if !gone(t, notes) || res.RemovedParent != filepath.Join(folder, "app") || !gone(t, res.RemovedParent) {
		t.Fatalf("result %+v", res)
	}
}

// A folder of the worktree folder with a worktree somewhere deeper in it
// is not removed: deleting it would take the worktree with it.
func TestWorktreeRemoveFolderHoldingWorktree(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	loose := filepath.Join(folder, "loose")
	inner := filepath.Join(loose, "deeper", "inner")
	addWorktree(t, repo, inner, "inner")
	_, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder, WorktreeRepos: []string{repo}})
	e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: loose}, nil)
	if e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("removing %s: %v", loose, e)
	}
	if gone(t, inner) {
		t.Fatal("the worktree inside went")
	}
}

// A worktree no task has that holds another worktree is listed with that
// refusal, which its dry run lists too; removing it is refused (refused)
// whatever the flags, a task's worktree likewise, and both stay. Once the
// inner one is removed it goes as usual.
func TestWorktreeRemoveNestedWorktree(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "outer")
	addWorktree(t, repo, wt, "outer")
	inner := nestedWorktree(t, repo, wt)
	taskWt := filepath.Join(folder, "app", "app-5")
	addWorktree(t, repo, taskWt, "5-fix")
	taskInner := filepath.Join(taskWt, ".claude", "worktrees", "y")
	addWorktree(t, taskWt, taskInner, "y")
	d, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})
	d.tasks = []wire.Task{{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: taskWt, State: wire.TaskReady}}

	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	refusals := map[string][]wire.RemoveRefusal{}
	for _, w := range list.Repos[0].Worktrees {
		refusals[w.Path] = w.Refusals
	}
	nested := func(p string) []wire.RemoveRefusal {
		return []wire.RemoveRefusal{{Kind: wire.RefusalNestedWorktrees, Paths: []string{p}}}
	}
	want := map[string][]wire.RemoveRefusal{wt: nested(inner), inner: nil, taskWt: nested(taskInner), taskInner: nil}
	if !reflect.DeepEqual(refusals, want) {
		t.Fatalf("worktree.list refusals %+v, want %+v", refusals, want)
	}

	for path, inside := range map[string]string{wt: inner, taskWt: taskInner} {
		var dry wire.WorktreeRemoveResult
		if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: path, DryRun: true}, &dry); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(dry.Refusals, nested(inside)) {
			t.Fatalf("dry run of %s: %+v", path, dry)
		}
		e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: path, Force: true, StopSessions: true}, nil)
		if e == nil || e.Code != wire.ErrRefused || !strings.Contains(e.Message, inside) {
			t.Fatalf("worktree.remove %s: %v, want %s", path, e, wire.ErrRefused)
		}
		if !listed(t, repo, path) || !listed(t, repo, inside) {
			t.Fatalf("a refused removal of %s removed a worktree", path)
		}
	}
	if gone(t, filepath.Join(inner, "wip.txt")) {
		t.Fatal("the inner worktree's change is gone")
	}
	d.mu.Lock()
	if len(d.forgot) != 0 {
		t.Fatalf("refused removal forgot %v", d.forgot)
	}
	d.mu.Unlock()

	gitRun(t, repo, "worktree", "remove", "--force", inner)
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt}, nil); e != nil {
		t.Fatal(e)
	}
	if listed(t, repo, wt) || !gone(t, wt) {
		t.Fatal("the outer worktree stayed once the inner one was removed")
	}
}

// A worktree no task has with a submodule (git worktree remove refuses
// it) is listed with that refusal, so that select merged and clean skips
// it, lists why in the dry run and is refused (refused) whatever the
// flags.
func TestWorktreeRemoveRefusesSubmodules(t *testing.T) {
	repo := checkout(t)
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	wt := filepath.Join(folder, "app", "subs")
	addWorktree(t, repo, wt, "subs")
	gitRun(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(filepath.Dir(repo), "seed"), "sub")
	gitRun(t, wt, "commit", "-q", "-m", "submodule")
	_, a := startWorktreeBridge(t, wire.Config{WorktreeFolder: folder})
	want := []wire.RemoveRefusal{{Kind: wire.RefusalSubmodules, Paths: []string{filepath.Join(wt, "sub")}}}
	var list wire.WorktreeListResult
	if e := a.call(t, wire.MethodWorktreeList, struct{}{}, &list); e != nil {
		t.Fatal(e)
	}
	if len(list.Repos) != 1 || len(list.Repos[0].Worktrees) != 1 || !reflect.DeepEqual(list.Repos[0].Worktrees[0].Refusals, want) {
		t.Fatalf("worktree.list %+v, want refusals %+v", list, want)
	}
	var dry wire.WorktreeRemoveResult
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, DryRun: true}, &dry); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(dry.Refusals, want) {
		t.Fatalf("dry run %+v, want refusals %+v", dry, want)
	}
	if e := a.call(t, wire.MethodWorktreeRemove, wire.WorktreeRemoveParams{Path: wt, Force: true, StopSessions: true}, nil); e == nil || e.Code != wire.ErrRefused {
		t.Fatalf("worktree.remove: %v, want %s", e, wire.ErrRefused)
	}
	if !listed(t, repo, wt) {
		t.Fatal("refused worktree went")
	}
}
