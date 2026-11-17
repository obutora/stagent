package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/qwen-hooks.jsonl: the hooks Qwen Code 0.27.4 sent for one
// conversation (verified against a local fake model; paths shortened), in
// the order they came: a shell command and a file write asked about (the
// first allowed on the PC, the second turned down with Esc), an
// auto-allowed command, a turn without tools, a question, the end, and a
// resume whose asked-about command is turned down too.
func qwenHooks(t *testing.T) []json.RawMessage {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "qwen-hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hooks []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		hooks = append(hooks, json.RawMessage(slices.Clone(sc.Bytes())))
	}
	return hooks
}

// Qwen Code sends idle_prompt at the end of every reply of its model:
// amid a turn, with the PreToolUse of each tool call (either first), and
// right after Stop. Replayed in order on the daemon's clock set to the
// times they came, with the timers due meanwhile run in between, the
// session works, asks for approval (the hook returns at once: Qwen Code
// shows its menu only after it), waits on the question (never an
// approval) and ends its turns; it waits for input only after a menu was
// turned down, for which idle_prompt is the only hook.
func TestQwenHooksReplayed(t *testing.T) {
	hooks := qwenHooks(t)
	stamps := make([]time.Time, len(hooks))
	for i, payload := range hooks {
		var p struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("hook %d: %v", i, err)
		}
		stamps[i] = p.Timestamp
	}
	timers := &fakeTimers{clock: &fakeClock{t: stamps[0]}}
	e := startDaemon(t, fastConfig(), Options{Now: timers.clock.Now, AfterFunc: timers.AfterFunc})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	states := []string{wire.StateIdle}
	var approvals []wire.Approval
	take := func(m *wire.Msg) {
		switch {
		case m.Method == wire.NotifySessionUpdated:
			var s wire.Session
			json.Unmarshal(m.Params, &s)
			if states[len(states)-1] != s.State {
				states = append(states, s.State)
			}
		case event(wire.EventApprovalRequested, nil)(m):
			_, ap := decodeEvent[wire.Approval](t, m)
			approvals = append(approvals, ap)
		}
	}

	for i, payload := range hooks {
		timers.advance(stamps[i])
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessQwen, SessionID: sid, Payload: payload})
		answered(t, reply, "hook "+string(payload[:40]))
	}
	timers.advance(stamps[len(stamps)-1].Add(replyIdleGrace))

	want := []string{
		wire.StateIdle,
		wire.StateWorking, wire.StateNeedsApproval, // run_shell_command asked about
		wire.StateWorking, wire.StateNeedsApproval, // allowed; write_file asked about
		wire.StateWaitingInput,            // turned down (Esc)
		wire.StateWorking, wire.StateIdle, // an auto-allowed command
		wire.StateWorking, wire.StateIdle, // no tools
		wire.StateWorking, wire.StateWaitingInput, wire.StateWorking, wire.StateIdle, // the question
		wire.StateWorking, wire.StateNeedsApproval, wire.StateWaitingInput, // resumed; turned down
	}
	wantTools := []string{"run_shell_command", "write_file", "run_shell_command"}
	drain(events, take, func() bool { return len(states) >= len(want) && len(approvals) >= len(wantTools) })
	if !slices.Equal(states, want) {
		t.Errorf("states\n  %v\nwant\n  %v", states, want)
	}
	var tools []string
	for _, ap := range approvals {
		if ap.Harness != wire.HarnessQwen {
			t.Errorf("approval %+v", ap)
		}
		tools = append(tools, ap.ToolName)
	}
	if !slices.Equal(tools, wantTools) {
		t.Errorf("approvals for %v, want %v", tools, wantTools)
	}
	if w := nextPromptWatch(t, holderMsgs, "watch for the qwen menu"); !w.On || w.Harness != wire.HarnessQwen {
		t.Errorf("prompt_watch %+v", w)
	}
}

// fakeTimers are the timers of Options.AfterFunc on a fakeClock: they run
// on the test's goroutine when it moves the clock past them (advance).
type fakeTimers struct {
	clock   *fakeClock
	mu      sync.Mutex
	pending []*fakeTimer
}

type fakeTimer struct {
	timers *fakeTimers
	at     time.Time
	f      func()
}

func (ft *fakeTimers) AfterFunc(d time.Duration, f func()) Timer {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	t := &fakeTimer{timers: ft, at: ft.clock.Now().Add(d), f: f}
	ft.pending = append(ft.pending, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	ft := t.timers
	ft.mu.Lock()
	defer ft.mu.Unlock()
	n := len(ft.pending)
	ft.pending = slices.DeleteFunc(ft.pending, func(x *fakeTimer) bool { return x == t })
	return len(ft.pending) < n
}

// advance moves the clock on to at (never back: hooks of the same moment
// may be logged out of order), running each timer due by then at its time,
// earliest first.
func (ft *fakeTimers) advance(at time.Time) {
	for {
		ft.mu.Lock()
		var next *fakeTimer
		for _, t := range ft.pending {
			if !t.at.After(at) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			ft.mu.Unlock()
			break
		}
		ft.pending = slices.DeleteFunc(ft.pending, func(x *fakeTimer) bool { return x == next })
		ft.mu.Unlock()
		if next.at.After(ft.clock.Now()) {
			ft.clock.Set(next.at)
		}
		next.f()
	}
	if at.After(ft.clock.Now()) {
		ft.clock.Set(at)
	}
}

// Outside a stagent session an idle_prompt of Qwen Code is not told: it
// would come with every tool call, and no later hook can take it back.
func TestQwenIdlePromptOutsideSessions(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	note := json.RawMessage(`{"session_id":"a2730fef","cwd":"/home/u/proj","hook_event_name":"Notification","message":"Qwen Code is waiting for your input","notification_type":"idle_prompt"}`)
	_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessQwen, Payload: note})
	answered(t, reply, "idle_prompt")
	_, stop := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessQwen, Payload: json.RawMessage(`{"session_id":"a2730fef","cwd":"/home/u/proj","hook_event_name":"Stop"}`)})
	answered(t, stop, "Stop")
	await(t, events, "only the turn_complete notification", func(m *wire.Msg) bool {
		if notificationWith(reasonWaitingInput)(m) {
			t.Fatalf("sessionless idle_prompt told: %s", m.Params)
		}
		return notificationWith(reasonTurnComplete)(m)
	})
}
