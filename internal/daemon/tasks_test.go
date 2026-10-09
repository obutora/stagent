package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

func issueTask(repo string, number int, sessionID string) wire.TaskRegisterParams {
	return wire.TaskRegisterParams{
		Repo: repo, Kind: wire.TaskKindIssue, Number: number, Title: "Fix the thing",
		URL: "https://github.com/o/r/issues/7", Branch: "7-fix-the-thing", Base: "main",
		Worktree: repo + "-" + itoa(number), SessionID: sessionID,
	}
}

func inPlaceTask(repo string, number int, sessionID string) wire.TaskRegisterParams {
	p := issueTask(repo, number, sessionID)
	p.InPlace, p.Worktree = true, ""
	return p
}

func itoa(n int) string { return strconv.Itoa(n) }

// abs turns the slash path p ("/w/repo") into an absolute path of this
// OS: on drive C: on Windows.
func abs(p string) string {
	if runtime.GOOS == "windows" {
		p = "C:" + p
	}
	return filepath.FromSlash(p)
}

func callErr(c *rpc.Client, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

func registerTask(t *testing.T, c *rpc.Client, p wire.TaskRegisterParams) wire.TaskRegisterResult {
	t.Helper()
	var r wire.TaskRegisterResult
	call(t, c, wire.MethodTaskRegister, p, &r)
	return r
}

func listTasks(t *testing.T, c *rpc.Client) []wire.Task {
	t.Helper()
	var r wire.TaskListResult
	call(t, c, wire.MethodTaskList, nil, &r)
	return r.Tasks
}

func taskUpdated(id, state string) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		if m.Method != wire.NotifyTaskUpdated {
			return false
		}
		var tk wire.Task
		json.Unmarshal(m.Params, &tk)
		return tk.ID == id && tk.State == state
	}
}

func decodeTask(t *testing.T, m *wire.Msg) wire.Task {
	t.Helper()
	var tk wire.Task
	if err := json.Unmarshal(m.Params, &tk); err != nil {
		t.Fatal(err)
	}
	return tk
}

func sessionByID(t *testing.T, c *rpc.Client, id string) wire.Session {
	t.Helper()
	var r wire.SessionsListResult
	call(t, c, wire.MethodSessionsList, nil, &r)
	for _, s := range r.Sessions {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %s not listed", id)
	return wire.Session{}
}

// A registered task is preparing, reaches watchers (task.updated, watch's
// tasks), is listed in the order of creation and survives a restart in
// tasks.json (0600).
func TestTaskRegisterListAndPersist(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	watcher, events := e.client()
	var w wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, &w)
	if w.Tasks == nil || len(w.Tasks) != 0 {
		t.Fatalf("watch tasks before any: %#v", w.Tasks)
	}
	c, _ := e.client()
	r := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1"))
	if r.Existed || r.Session != nil {
		t.Fatalf("new task: %+v", r)
	}
	tk := r.Task
	if tk.ID == "" || tk.Repo != abs("/w/repo") || tk.Worktree != abs("/w/repo-7") || tk.State != wire.TaskPreparing ||
		tk.SessionID != "00000000000000a1" || tk.CreatedAt.IsZero() || tk.InPlace {
		t.Fatalf("registered task %+v", tk)
	}
	if got := decodeTask(t, await(t, events, "task.updated", taskUpdated(tk.ID, wire.TaskPreparing))); got != tk {
		t.Fatalf("task.updated %+v, want %+v", got, tk)
	}
	second := registerTask(t, c, issueTask(abs("/w/other"), 3, "00000000000000a2")).Task
	if ts := listTasks(t, c); len(ts) != 2 || ts[0].ID != tk.ID || ts[1].ID != second.ID {
		t.Fatalf("task.list %+v", ts)
	}
	w2c, _ := e.client()
	call(t, w2c, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Tasks) != 2 || w.Tasks[0] != tk {
		t.Fatalf("watch tasks %+v", w.Tasks)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(e.layout.Tasks)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("tasks.json: %v %v", st, err)
		}
	}

	e.stop()
	e2 := startDaemonAt(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c2, _ := e2.client()
	if ts := listTasks(t, c2); len(ts) != 2 || ts[0] != tk || ts[1] != second {
		t.Fatalf("after restart: %+v", ts)
	}
}

// task.progress moves a preparing task through its stages; stage agent
// makes it ready, after which task.register answers it as it is.
func TestTaskProgressToReady(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	c, _ := e.client()
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1")).Task
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageSetup}, nil)
	got := decodeTask(t, await(t, events, "stage setup", func(m *wire.Msg) bool {
		return taskUpdated(tk.ID, wire.TaskPreparing)(m) && decodeTask(t, m).Stage == wire.TaskStageSetup
	}))
	if got.Error != "" {
		t.Fatalf("setup stage %+v", got)
	}
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent}, nil)
	if got := decodeTask(t, await(t, events, "ready", taskUpdated(tk.ID, wire.TaskReady))); got.Stage != "" {
		t.Fatalf("ready task keeps a stage: %+v", got)
	}
	r := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a3"))
	if !r.Existed || r.Task.State != wire.TaskReady || r.Task.SessionID != "00000000000000a1" || r.Session != nil {
		t.Fatalf("second task.register of a ready task: %+v", r)
	}
	if err := callErr(c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: "nope", Stage: wire.TaskStageAgent}, nil); errCode(err) != wire.ErrNotFound {
		t.Fatalf("progress of an unknown task: %v", err)
	}
	if err := callErr(c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: "dance"}, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("unknown stage: %v", err)
	}
}

// A task still preparing with its prepared session alive answers that
// session; a failed one is prepared again with the new session.
func TestTaskRegisterPreparingAndFailed(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	const prep, retry = "00000000000000a1", "00000000000000a2"
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, prep)).Task
	s := testSession(prep)
	s.Cwd = abs("/w/repo-7")
	e.register(s)
	r := registerTask(t, c, issueTask(abs("/w/repo"), 7, retry))
	if !r.Existed || r.Session == nil || r.Session.ID != prep || r.Task.ID != tk.ID || r.Task.State != wire.TaskPreparing {
		t.Fatalf("task.register while preparing: %+v", r)
	}

	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageSetup, Error: "setup failed:\n  exit status 1"}, nil)
	ts := listTasks(t, c)
	if ts[0].State != wire.TaskFailed || ts[0].Stage != wire.TaskStageSetup || ts[0].Error != "setup failed: exit status 1" {
		t.Fatalf("failed task %+v", ts[0])
	}
	r = registerTask(t, c, issueTask(abs("/w/repo"), 7, retry))
	if r.Existed || r.Session != nil || r.Task.ID != tk.ID || r.Task.State != wire.TaskPreparing ||
		r.Task.Error != "" || r.Task.Stage != "" || r.Task.SessionID != retry {
		t.Fatalf("task.register of a failed task: %+v", r)
	}
}

// An in_place task takes its checkout: another number's in_place task on
// it is in_use; worktree tasks of the checkout are not.
func TestTaskInPlaceInUse(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	first := registerTask(t, c, inPlaceTask(abs("/w/repo"), 12, "00000000000000a1")).Task
	if !first.InPlace || first.Worktree != abs("/w/repo") {
		t.Fatalf("in_place task %+v", first)
	}
	if err := callErr(c, wire.MethodTaskRegister, inPlaceTask(abs("/w/repo"), 13, "00000000000000a2"), nil); errCode(err) != wire.ErrInUse {
		t.Fatalf("second in_place task of the checkout: %v, want %s", err, wire.ErrInUse)
	}
	registerTask(t, c, issueTask(abs("/w/repo"), 13, "00000000000000a3"))
	registerTask(t, c, inPlaceTask(abs("/w/other"), 13, "00000000000000a4"))
	if r := registerTask(t, c, inPlaceTask(abs("/w/repo"), 12, "00000000000000a5")); !r.Existed || r.Task.ID != first.ID {
		t.Fatalf("same in_place task again: %+v", r)
	}
}

// A preparing task whose prepared session never registers, or ends, is
// failed with error interrupted.
func TestTaskInterrupted(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: 200 * time.Millisecond})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	c, _ := e.client()
	never := registerTask(t, c, issueTask(abs("/w/repo"), 1, "00000000000000a1")).Task
	got := decodeTask(t, await(t, events, "never registered", taskUpdated(never.ID, wire.TaskFailed)))
	if got.Error != wire.TaskErrInterrupted {
		t.Fatalf("interrupted task %+v", got)
	}

	const prep = "00000000000000a2"
	ended := registerTask(t, c, issueTask(abs("/w/repo"), 2, prep)).Task
	s := testSession(prep)
	s.Cwd = abs("/w/repo-2")
	h := e.register(s)
	time.Sleep(300 * time.Millisecond) // past the grace: the session counts
	if ts := listTasks(t, c); ts[1].State != wire.TaskPreparing {
		t.Fatalf("task of a live prepared session: %+v", ts[1])
	}
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: prep, ExitCode: 1})
	got = decodeTask(t, await(t, events, "prepared session ended", taskUpdated(ended.ID, wire.TaskFailed)))
	if got.Error != wire.TaskErrInterrupted {
		t.Fatalf("interrupted task %+v", got)
	}
	var w wire.WatchResult
	w2, _ := e.client()
	call(t, w2, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Tasks) != 2 || w.Tasks[0].State != wire.TaskFailed || w.Tasks[1].State != wire.TaskFailed {
		t.Fatalf("watch tasks %+v", w.Tasks)
	}
}

// After a restart a preparing task waits for its holder to register
// again: one that does stays preparing, one that does not is interrupted.
func TestTaskInterruptedAfterRestart(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	const back, gone = "00000000000000a1", "00000000000000a2"
	kept := registerTask(t, c, issueTask(abs("/w/repo"), 1, back)).Task
	lost := registerTask(t, c, issueTask(abs("/w/repo"), 2, gone)).Task
	e.stop()

	e2 := startDaemonAt(t, fastConfig(), Options{PrepareGrace: 300 * time.Millisecond})
	c2, events := e2.client()
	call(t, c2, wire.MethodWatch, wire.WatchParams{}, nil)
	if ts := listTasks(t, c2); ts[0].State != wire.TaskPreparing || ts[1].State != wire.TaskPreparing {
		t.Fatalf("right after the restart: %+v", ts)
	}
	s := testSession(back)
	s.Cwd = abs("/w/repo-1")
	e2.register(s)
	await(t, events, "lost task interrupted", taskUpdated(lost.ID, wire.TaskFailed))
	ts := listTasks(t, c2)
	if ts[0].ID != kept.ID || ts[0].State != wire.TaskPreparing || ts[1].Error != wire.TaskErrInterrupted {
		t.Fatalf("after the grace: %+v", ts)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// task.list finds a ready task missing when its worktree directory is
// gone, or git no longer lists it.
func TestTaskMissing(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	os.Mkdir(repo, 0o700)
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	canon, err := task.Canonical(context.Background(), nil, repo)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := e.client()
	ready := func(n int) wire.Task {
		wt := canon + "-" + itoa(n)
		git(t, repo, "worktree", "add", "-q", "-b", "b"+itoa(n), wt)
		p := issueTask(canon, n, "00000000000000a"+itoa(n))
		tk := registerTask(t, c, p).Task
		call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent}, nil)
		return tk
	}
	deleted, pruned, kept := ready(1), ready(2), ready(3)
	inPlace := registerTask(t, c, inPlaceTask(canon, 4, "00000000000000a4")).Task
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: inPlace.ID, Stage: wire.TaskStageAgent}, nil)
	for _, tk := range listTasks(t, c) {
		if tk.State != wire.TaskReady {
			t.Fatalf("before: %+v", tk)
		}
	}
	os.RemoveAll(deleted.Worktree)
	git(t, repo, "worktree", "remove", pruned.Worktree)
	os.Mkdir(pruned.Worktree, 0o700) // a directory git does not know
	ts := listTasks(t, c)
	want := map[string]string{deleted.ID: wire.TaskMissing, pruned.ID: wire.TaskMissing, kept.ID: wire.TaskReady, inPlace.ID: wire.TaskReady}
	for _, tk := range ts {
		if tk.State != want[tk.ID] {
			t.Errorf("task #%d: %s, want %s", tk.Number, tk.State, want[tk.ID])
		}
	}
}

// Session.task_id: a worktree task takes the sessions in its worktree,
// live ones when it is registered and new ones when they start; an
// in_place task only its prepared session and those started for it.
// task.forget unties them and task.removed names the branch it kept.
func TestSessionTaskID(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	c, _ := e.client()
	const early, inside, sibling, checkout, spawned, prepared = "00000000000000b1", "00000000000000b2", "00000000000000b3", "00000000000000b4", "00000000000000b5", "00000000000000b6"
	at := func(id, cwd, taskID string) wire.Session {
		s := testSession(id)
		s.Cwd, s.TaskID = cwd, taskID
		return s
	}
	e.register(at(early, abs("/w/repo-5/src"), ""))
	wt := registerTask(t, c, issueTask(abs("/w/repo"), 5, "00000000000000a1")).Task
	await(t, events, "live session tied", func(m *wire.Msg) bool {
		var s wire.Session
		json.Unmarshal(m.Params, &s)
		return m.Method == wire.NotifySessionUpdated && s.ID == early && s.TaskID == wt.ID
	})
	e.register(at(inside, abs("/w/repo-5"), ""))
	e.register(at(sibling, abs("/w/repo-50"), ""))
	ip := registerTask(t, c, inPlaceTask(abs("/w/repo"), 9, prepared)).Task
	e.register(at(checkout, abs("/w/repo"), ""))
	e.register(at(spawned, abs("/w/repo/sub"), ip.ID))
	e.register(at(prepared, abs("/w/repo"), ""))
	for id, want := range map[string]string{early: wt.ID, inside: wt.ID, sibling: "", checkout: "", spawned: ip.ID, prepared: ip.ID} {
		if got := sessionByID(t, c, id).TaskID; got != want {
			t.Errorf("session %s: task_id %q, want %q", id, got, want)
		}
	}

	call(t, c, wire.MethodTaskForget, wire.TaskRemoved{ID: ip.ID, KeptBranch: "9-fix-the-thing"}, nil)
	await(t, events, "task.removed", func(m *wire.Msg) bool {
		var r wire.TaskRemoved
		json.Unmarshal(m.Params, &r)
		return m.Method == wire.NotifyTaskRemoved && r.ID == ip.ID && r.KeptBranch == "9-fix-the-thing"
	})
	for _, id := range []string{spawned, prepared} {
		if got := sessionByID(t, c, id).TaskID; got != "" {
			t.Errorf("session %s keeps task %q after task.forget", id, got)
		}
	}
	if ts := listTasks(t, c); len(ts) != 1 || ts[0].ID != wt.ID {
		t.Fatalf("after task.forget: %+v", ts)
	}
}

// The pushes of a task's session are titled "<host> · #N <title>".
func TestPushTitleNamesTask(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	p := issueTask(abs("/home/u/repo"), 42, "00000000000000a1")
	p.Worktree = abs("/home/u/proj")
	registerTask(t, c, p)
	s := testSession(sid)
	s.Cwd = abs("/home/u/proj")
	e.register(s)
	e.hook(stop(sid))
	if r := wantPush(t, reqs, "turn_complete", sid, "Turn complete"); r.title != "box · #42 claude · proj" {
		t.Fatalf("push titled %q", r.title)
	}
}

// A coding agent's process tree may list tasks but not change them.
func TestTaskMethodsRefusedToAgents(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{Guard: guardAs("claude")})
	c, _ := e.client()
	for _, m := range []struct {
		method string
		params any
	}{
		{wire.MethodTaskRegister, issueTask(abs("/w/repo"), 1, "00000000000000a1")},
		{wire.MethodTaskProgress, wire.TaskProgressParams{ID: "x", Stage: wire.TaskStageAgent}},
		{wire.MethodTaskForget, wire.TaskRef{ID: "x"}},
	} {
		if err := callErr(c, m.method, m.params, nil); errCode(err) != wire.ErrAgentRefused {
			t.Errorf("%s from an agent: %v, want %s", m.method, err, wire.ErrAgentRefused)
		}
	}
	if err := callErr(c, wire.MethodTaskList, nil, nil); err != nil {
		t.Errorf("task.list from an agent: %v", err)
	}
}

// Stage stop or archive starts removing a task; a failed step takes it
// back to where it was, keeping the error. A preparing task is not removed
// by stages.
func TestTaskProgressRemoving(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	// Read through watch: task.list would find these worktrees missing.
	watched := func() []wire.Task {
		t.Helper()
		w, _ := e.client()
		var r wire.WatchResult
		call(t, w, wire.MethodWatch, wire.WatchParams{}, &r)
		return r.Tasks
	}
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1")).Task
	if err := callErr(c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageStop}, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("stop while preparing: %v", err)
	}
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent}, nil)
	step := func(stage, msg, state, wantErr string) {
		t.Helper()
		call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: stage, Error: msg}, nil)
		got := watched()[0]
		if got.State != state || got.Stage != stage || got.Error != wantErr {
			t.Fatalf("after %s %q: %+v, want %s", stage, msg, got, state)
		}
	}
	step(wire.TaskStageStop, "", wire.TaskRemoving, "")
	if err := callErr(c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageStop}, nil); errCode(err) != wire.ErrBusy {
		t.Fatalf("second stop while removing: %v, want %s", err, wire.ErrBusy)
	}
	step(wire.TaskStageArchive, "", wire.TaskRemoving, "")
	step(wire.TaskStageArchive, "archive exited with 2", wire.TaskReady, "archive exited with 2")
	step(wire.TaskStageArchive, "", wire.TaskRemoving, "")
	step(wire.TaskStageWorktree, "", wire.TaskRemoving, "")
	step(wire.TaskStageBranch, "", wire.TaskRemoving, "")
	if err := callErr(c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageSetup}, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("setup while removing: %v", err)
	}

	failed := registerTask(t, c, issueTask(abs("/w/repo"), 8, "00000000000000a2")).Task
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: failed.ID, Stage: wire.TaskStageFetch, Error: "offline"}, nil)
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: failed.ID, Stage: wire.TaskStageStop}, nil)
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: failed.ID, Stage: wire.TaskStageWorktree, Error: "worktree is locked"}, nil)
	if got := watched()[1]; got.State != wire.TaskFailed || got.Stage != wire.TaskStageWorktree || got.Error != "worktree is locked" {
		t.Fatalf("failed task after a failed removal: %+v", got)
	}
}

// A removal cut short by a restart is over: the task is ready again with
// error interrupted, keeping the stage, and can be removed again.
func TestTaskRemovingInterruptedByRestart(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1")).Task
	for _, stage := range []string{wire.TaskStageAgent, wire.TaskStageStop, wire.TaskStageArchive} {
		call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: stage}, nil)
	}
	e.stop()

	e2 := startDaemonAt(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c2, _ := e2.client()
	var w wire.WatchResult
	call(t, c2, wire.MethodWatch, wire.WatchParams{}, &w)
	if got := w.Tasks[0]; got.State != wire.TaskReady || got.Error != wire.TaskErrInterrupted || got.Stage != wire.TaskStageArchive {
		t.Fatalf("after the restart: %+v", got)
	}
	call(t, c2, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageStop}, nil)
	e2.stop()
	e3 := startDaemonAt(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c3, _ := e3.client()
	call(t, c3, wire.MethodWatch, wire.WatchParams{}, &w)
	if got := w.Tasks[0]; got.State != wire.TaskReady || got.Stage != wire.TaskStageStop {
		t.Fatalf("interrupted again, stored: %+v", got)
	}
}

// The prepared session failing to become the agent after stage agent
// fails its ready task (stage agent); another session cannot.
func TestTaskAgentFailsAfterReady(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	const prep = "00000000000000a1"
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, prep)).Task
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent}, nil)
	other := wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent, Error: "exec: no such file", SessionID: "00000000000000a9"}
	if err := callErr(c, wire.MethodTaskProgress, other, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("agent error from another session: %v", err)
	}
	call(t, c, wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent, Error: "exec: no such file", SessionID: prep}, nil)
	var w wire.WatchResult
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &w)
	if got := w.Tasks[0]; got.State != wire.TaskFailed || got.Stage != wire.TaskStageAgent || got.Error != "exec: no such file" {
		t.Fatalf("task after the agent failed: %+v", got)
	}
}

// When tasks.json cannot be written, task.register, task.progress and
// task.forget fail (internal), change nothing and tell no watcher.
func TestTaskSaveFailureChangesNothing(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this user cannot write")
	}
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	c, _ := e.client()
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1")).Task
	await(t, events, "registered", taskUpdated(tk.ID, wire.TaskPreparing))
	dir := filepath.Dir(e.layout.Tasks)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	for _, tc := range []struct {
		method string
		params any
	}{
		{wire.MethodTaskProgress, wire.TaskProgressParams{ID: tk.ID, Stage: wire.TaskStageAgent}},
		{wire.MethodTaskRegister, issueTask(abs("/w/repo"), 8, "00000000000000a2")},
		{wire.MethodTaskForget, wire.TaskRemoved{ID: tk.ID}},
	} {
		if err := callErr(c, tc.method, tc.params, nil); errCode(err) != wire.ErrInternal {
			t.Fatalf("%s without tasks.json: %v, want %s", tc.method, err, wire.ErrInternal)
		}
	}
	var w wire.WatchResult
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Tasks) != 1 || w.Tasks[0] != tk {
		t.Fatalf("tasks after failed writes: %+v, want %+v", w.Tasks, tk)
	}
	select {
	case m := <-events:
		if m.Method == wire.NotifyTaskUpdated || m.Method == wire.NotifyTaskRemoved {
			t.Fatalf("watcher told %s %s", m.Method, m.Params)
		}
	case <-time.After(200 * time.Millisecond):
	}
}
