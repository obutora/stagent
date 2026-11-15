package daemon

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	agentA = "00000000000000a1"
	agentB = "00000000000000b1"
	agentC = "00000000000000c1"
)

func bootIs(id string) func() string { return func() string { return id } }

// startStopped starts a daemon in the boot named boot, in the
// STAGENT_HOME of the test (startDaemon first, startDaemonAt after a
// restart).
func startStopped(t *testing.T, fresh bool, boot string, opts Options) *testEnv {
	t.Helper()
	if !stoppedServed {
		t.Skip("stopped agents are kept on Linux and macOS only")
	}
	opts.BootID = bootIs(boot)
	if opts.PrepareGrace == 0 {
		opts.PrepareGrace = time.Minute
	}
	if fresh {
		return startDaemon(t, fastConfig(), opts)
	}
	return startDaemonAt(t, fastConfig(), opts)
}

// runAgent registers a holder of session ws whose agent's hook reports
// conversation conv, and returns the holder's connection.
func (e *testEnv) runAgent(ws wire.Session, conv string) *rpc.Client {
	e.t.Helper()
	h, _ := e.client()
	call(e.t, h, wire.MethodHolderRegister, ws, nil)
	_, reply := e.hookAsync(wire.HookEventParams{
		Harness: ws.Harness, Event: "SessionStart", SessionID: ws.ID,
		Payload: json.RawMessage(`{"session_id":"` + conv + `"}`),
	})
	answered(e.t, reply, "SessionStart")
	return h
}

func agentSession(id, harnessID, cwd string, started int64) wire.Session {
	s := testSession(id)
	s.Harness, s.Command, s.Cwd, s.StartedAt = harnessID, []string{harnessID}, cwd, started
	return s
}

func (e *testEnv) stoppedList() []wire.StoppedAgent {
	e.t.Helper()
	c, _ := e.client()
	var w wire.WatchResult
	call(e.t, c, wire.MethodWatch, wire.WatchParams{}, &w)
	if w.Stopped == nil {
		e.t.Fatal("watch without stopped")
	}
	return w.Stopped
}

// stoppedUpdate returns the next stopped.updated whose list matches pred.
func stoppedUpdate(t *testing.T, events chan *wire.Msg, what string, pred func([]wire.StoppedAgent) bool) []wire.StoppedAgent {
	t.Helper()
	var got []wire.StoppedAgent
	await(t, events, what, func(m *wire.Msg) bool {
		if m.Method != wire.NotifyStoppedUpdated {
			return false
		}
		var p wire.StoppedUpdatedParams
		json.Unmarshal(m.Params, &p)
		got = p.Stopped
		return pred(got)
	})
	return got
}

func ids(list []wire.StoppedAgent) []string {
	out := make([]string, len(list))
	for i, st := range list {
		out[i] = st.ID
	}
	return out
}

// After a reboot every agent that ran in a session is a stopped agent —
// one that ended just before the daemon went down too — with what it needs
// to be resumed; schedule runs and sessions without a conversation are not.
func TestStoppedAgentsAfterReboot(t *testing.T) {
	e := startStopped(t, true, "boot-1", Options{})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	tk := registerTask(t, c, issueTask(abs("/w/repo"), 7, "00000000000000f1")).Task

	a := agentSession(agentA, wire.HarnessClaude, tk.Worktree, 3000)
	a.Title = "fix the build"
	e.runAgent(a, "conv-a")
	e.runAgent(agentSession(agentB, wire.HarnessCodex, "/home/u/api", 2000), "conv-b")
	hc := e.runAgent(agentSession(agentC, wire.HarnessClaude, "/home/u/web", 1000), "conv-c")
	hc.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: agentC, ExitCode: 129})
	await(t, events, "C ended", event(wire.EventSessionEnded, func(ev wire.Event) bool { return ev.SessionID == agentC }))
	run := agentSession("00000000000000d1", wire.HarnessClaude, "/home/u/cron", 4000)
	run.ScheduleID, run.RunID = "00000000000000e1", "00000000000000e2"
	e.runAgent(run, "conv-run")
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, agentSession("00000000000000d2", wire.HarnessClaude, "/home/u/x", 5000), nil)
	if l := e.stoppedList(); len(l) != 0 {
		t.Fatalf("stopped before the reboot %+v", l)
	}
	before := time.Now().UnixMilli()
	e.stop()

	e2 := startStopped(t, false, "boot-2", Options{})
	got := e2.stoppedList()
	if len(got) != 3 || got[0].ID != agentA || got[1].ID != agentB || got[2].ID != agentC {
		t.Fatalf("stopped after the reboot %v, want A, B, C", ids(got))
	}
	want := wire.StoppedAgent{
		ID: agentA, Harness: wire.HarnessClaude, ConversationID: "conv-a", Cwd: tk.Worktree,
		Title: "fix the build", TaskID: tk.ID, StartedAt: 3000, FoundAt: got[0].FoundAt, Reason: wire.StoppedReboot,
	}
	if got[0] != want || got[0].FoundAt < before {
		t.Fatalf("stopped A %+v, want %+v", got[0], want)
	}
	if b := got[1]; b.Harness != wire.HarnessCodex || b.ConversationID != "conv-b" || b.Cwd != "/home/u/api" ||
		b.Reason != wire.StoppedReboot || b.TaskID != "" {
		t.Fatalf("stopped B %+v", b)
	}
	if st, err := os.Stat(e2.layout.StoppedAgents); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("agents.json: %v, %v", st, err)
	}

	// They stay listed across a later restart in the same boot.
	e2.stop()
	e3 := startStopped(t, false, "boot-2", Options{})
	if l := e3.stoppedList(); len(l) != 3 || l[0].Reason != wire.StoppedReboot {
		t.Fatalf("stopped after a restart %+v", l)
	}
}

// In the same boot an agent whose holder went away is lost, one whose
// holder still answers is running (its conversation comes back when the
// holder registers again), and one that ended is over.
func TestStoppedAgentsSameBoot(t *testing.T) {
	e := startStopped(t, true, "boot-1", Options{})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	e.runAgent(agentSession(agentA, wire.HarnessClaude, "/home/u/a", 1000), "conv-a")
	e.runAgent(agentSession(agentB, wire.HarnessClaude, "/home/u/b", 2000), "conv-b")
	hc := e.runAgent(agentSession(agentC, wire.HarnessClaude, "/home/u/c", 3000), "conv-c")
	hc.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: agentC, ExitCode: 0})
	await(t, events, "C ended", event(wire.EventSessionEnded, func(ev wire.Event) bool { return ev.SessionID == agentC }))
	e.stop()

	ln, err := ipc.Listen(e.layout.HolderAddr(agentB))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e2 := startStopped(t, false, "boot-1", Options{})
	got := e2.stoppedList()
	if len(got) != 1 || got[0].ID != agentA || got[0].Reason != wire.StoppedLost || got[0].ConversationID != "conv-a" {
		t.Fatalf("stopped %+v, want A lost", got)
	}
	w, _ := e2.client()
	h, _ := e2.client()
	call(t, h, wire.MethodHolderRegister, agentSession(agentB, wire.HarnessClaude, "/home/u/b", 2000), nil)
	var sl wire.SessionsListResult
	call(t, w, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 || sl.Sessions[0].ConversationID != "conv-b" {
		t.Fatalf("B registered again %+v", sl.Sessions)
	}

	// B is still recorded: its holder going away now makes it lost at the
	// next start.
	e2.stop()
	ln.Close()
	e3 := startStopped(t, false, "boot-1", Options{})
	if l := e3.stoppedList(); len(l) != 2 || l[0].ID != agentB || l[0].Reason != wire.StoppedLost {
		t.Fatalf("stopped %v, want B then A", ids(l))
	}
}

// A holder that goes away without holder.ended while the daemon runs makes
// its agent a stopped agent at once (lost); an agent that ends normally
// (holder.ended, any code) does not. When the host then restarts, one found
// lost just before the daemon went down stopped with the reboot.
func TestStoppedAgentLostWhileRunning(t *testing.T) {
	e := startStopped(t, true, "boot-1", Options{GoneGrace: 100 * time.Millisecond})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	ha := e.runAgent(agentSession(agentA, wire.HarnessClaude, "/home/u/a", 1000), "conv-a")
	hb := e.runAgent(agentSession(agentB, wire.HarnessClaude, "/home/u/b", 2000), "conv-b")
	hb.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: agentB, ExitCode: 1})
	await(t, events, "B ended", event(wire.EventSessionEnded, func(ev wire.Event) bool { return ev.SessionID == agentB }))
	ha.Close()
	got := stoppedUpdate(t, events, "A lost", func(l []wire.StoppedAgent) bool { return len(l) > 0 })
	if len(got) != 1 || got[0].ID != agentA || got[0].Reason != wire.StoppedLost || got[0].ConversationID != "conv-a" {
		t.Fatalf("stopped %+v, want A lost", got)
	}
	if l := e.stoppedList(); len(l) != 1 {
		t.Fatalf("stopped %+v", l)
	}
	e.stop()

	if l := startStopped(t, false, "boot-2", Options{}).stoppedList(); len(l) != 2 ||
		l[0].ID != agentB || l[1].ID != agentA || l[0].Reason != wire.StoppedReboot || l[1].Reason != wire.StoppedReboot {
		t.Fatalf("stopped after the reboot %+v, want B then A, both reboot", l)
	}
}

// startRebooted records agents A (claude) and B (codex) and restarts the
// daemon in another boot, where both are stopped.
func startRebooted(t *testing.T, opts Options) *testEnv {
	t.Helper()
	e := startStopped(t, true, "boot-1", Options{})
	e.runAgent(agentSession(agentA, wire.HarnessClaude, t.TempDir(), 1000), "conv-a")
	e.runAgent(agentSession(agentB, wire.HarnessCodex, t.TempDir(), 2000), "conv-b")
	e.stop()
	e2 := startStopped(t, false, "boot-2", opts)
	if l := e2.stoppedList(); len(l) != 2 {
		t.Fatalf("stopped %+v", l)
	}
	return e2
}

// A live session that carries a stopped agent's conversation continues it:
// the stopped agent goes, and a claim of it answers that session.
func TestStoppedAgentContinuedElsewhere(t *testing.T) {
	e := startRebooted(t, Options{})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	const other = "00000000000000f2"
	e.runAgent(agentSession(other, wire.HarnessClaude, "/home/u/z", 9000), "conv-a")
	got := stoppedUpdate(t, events, "A continued", func(l []wire.StoppedAgent) bool { return len(l) == 1 })
	if got[0].ID != agentB {
		t.Fatalf("stopped %+v", got)
	}
	var res wire.StoppedClaimResult
	call(t, c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentA, SessionID: "00000000000000f3"}, &res)
	if res.Session == nil || res.Session.ID != other || res.Agent != nil {
		t.Fatalf("claim of the continued agent %+v", res)
	}
}

// A claim reserves the stopped agent for the resuming session: another
// claim meanwhile is busy, the session registering drops the stopped agent,
// and later claims answer that session. An unclaimed agent can be claimed
// again; stopped.remove drops one (again: nothing).
func TestStoppedClaimAndRemove(t *testing.T) {
	e := startRebooted(t, Options{})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	const s1, s2, s3 = "00000000000000f1", "00000000000000f2", "00000000000000f3"

	var res wire.StoppedClaimResult
	call(t, c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentA, SessionID: s1}, &res)
	if res.Agent == nil || res.Agent.ID != agentA || res.Agent.ConversationID != "conv-a" || res.Session != nil {
		t.Fatalf("claim %+v", res)
	}
	if err := callErr(c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentA, SessionID: s2}, nil); errCode(err) != wire.ErrBusy {
		t.Fatalf("second claim while starting: %v, want busy", err)
	}
	h, _ := e.client()
	resumed := testSession(s1)
	resumed.Harness, resumed.Command = "", []string{"/bin/sh", "-c", "trap : INT; claude --resume conv-a; exec '/bin/zsh' -l"}
	call(t, h, wire.MethodHolderRegister, resumed, nil)
	if got := stoppedUpdate(t, events, "A resumed", func(l []wire.StoppedAgent) bool { return len(l) == 1 }); got[0].ID != agentB {
		t.Fatalf("stopped %+v", got)
	}
	res = wire.StoppedClaimResult{}
	call(t, c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentA, SessionID: s2}, &res)
	if res.Session == nil || res.Session.ID != s1 {
		t.Fatalf("claim after the resume %+v", res)
	}

	call(t, c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentB, SessionID: s2}, nil)
	call(t, c, wire.MethodStoppedUnclaim, wire.StoppedClaimParams{ID: agentB, SessionID: s2}, nil)
	res = wire.StoppedClaimResult{}
	call(t, c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentB, SessionID: s3}, &res)
	if res.Agent == nil || res.Agent.ID != agentB {
		t.Fatalf("claim after unclaim %+v", res)
	}

	call(t, c, wire.MethodStoppedRemove, wire.StoppedRef{ID: agentB}, nil)
	stoppedUpdate(t, events, "B removed", func(l []wire.StoppedAgent) bool { return len(l) == 0 })
	call(t, c, wire.MethodStoppedRemove, wire.StoppedRef{ID: agentB}, nil)
	if err := callErr(c, wire.MethodStoppedClaim, wire.StoppedClaimParams{ID: agentB, SessionID: s2}, nil); errCode(err) != wire.ErrNotFound {
		t.Fatalf("claim of a removed agent: %v, want not_found", err)
	}
	e.stop()
	if l := startStopped(t, false, "boot-2", Options{}).stoppedList(); len(l) != 0 {
		t.Fatalf("stopped after a restart %+v", l)
	}
}

// A stopped agent is listed for 7 days after it was found.
func TestStoppedAgentsExpire(t *testing.T) {
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	e := startRebooted(t, Options{Now: clock})
	mu.Lock()
	now = now.Add(stoppedKeep - time.Minute)
	mu.Unlock()
	if l := e.stoppedList(); len(l) != 2 {
		t.Fatalf("stopped before 7 days %+v", l)
	}
	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	if l := e.stoppedList(); len(l) != 0 {
		t.Fatalf("stopped after 7 days %+v", l)
	}
	e.stop()
	e2 := startStopped(t, false, "boot-2", Options{Now: clock})
	if l := e2.stoppedList(); len(l) != 0 {
		t.Fatalf("stopped after 7 days and a restart %+v", l)
	}
	f, err := loadAgents(e2.layout.StoppedAgents)
	if err != nil || len(f.Stopped) != 0 || f.BootID != "boot-2" {
		t.Fatalf("agents.json %+v, %v", f, err)
	}
}
