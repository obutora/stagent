//go:build !windows

package bridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

// worktreeDaemon is a fake daemon for worktree.*: task records (task.forget
// drops one), sessions (each live one with a fake holder whose hangup ends
// it), and config.set merging the patch's top-level keys into config.json.
type worktreeDaemon struct {
	t        *testing.T
	l        *paths.Layout
	mu       sync.Mutex
	tasks    []wire.Task
	sessions []wire.Session
	signaled []string
	forgot   []string
	sets     []map[string]json.RawMessage
}

func startWorktreeBridge(t *testing.T, cfg wire.Config, opts ...func(*Bridge)) (*worktreeDaemon, *app) {
	t.Helper()
	opts = append(opts, func(b *Bridge) {
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(b.l.Config, data, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	l, a := startBridge(t, opts...)
	d := &worktreeDaemon{t: t, l: l}
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
	case wire.MethodTaskList:
		return wire.TaskListResult{Tasks: slices.Clone(d.tasks)}, nil
	case wire.MethodSessionsList:
		return wire.SessionsListResult{Sessions: slices.Clone(d.sessions)}, nil
	case wire.MethodTaskForget:
		var r wire.TaskRemoved
		rpc.Decode(m, &r)
		d.forgot = append(d.forgot, r.ID)
		d.tasks = slices.DeleteFunc(d.tasks, func(t wire.Task) bool { return t.ID == r.ID })
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

// Nothing is removed that cannot be: the source checkout and a locked
// worktree are bad_request, a path not in the inventory not_found. A
// coding agent's process tree may list but neither remove nor watch.
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
		{locked, wire.ErrBadRequest},
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
	if !slices.Equal(dry.Changes, []string{"?? new.txt", "?? sub/"}) || !slices.Equal(dry.Sessions, []string{sid}) {
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
