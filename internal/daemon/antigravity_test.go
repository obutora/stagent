package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/antigravity-hooks.jsonl: the hook.event calls stagent's hook
// made for the hooks Antigravity CLI 1.5.14 sent in one conversation
// (home shortened), in order: the hook names the first model call of a
// turn UserPromptSubmit and drops the later ones and PostInvocation. A
// command asked about and allowed; a question answered; a file write
// asked about and dismissed (Esc), then turned down (No) — both end the
// turn with no hook; a turn cut short by Esc while the model worked (no
// hook either); a turn without tools.
func antigravityHooks(t *testing.T) []wire.HookEventParams {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "antigravity-hooks.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hooks []wire.HookEventParams
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var p wire.HookEventParams
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		hooks = append(hooks, p)
	}
	return hooks
}

// Replayed in order, with the holder telling what the screen showed in
// between, the session works on each prompt; waits on each approval —
// registered when the holder saw the menu its PreToolUse may bring come on
// the screen (Antigravity CLI sends no hook for it), named by that
// PreToolUse's tool — until the menu left; waits on the question until it
// was answered; and is idle after each turn. A turn that ends with no
// hook (a menu dismissed or turned down, an Esc while it works) is idle
// once the holder reports its output quiet. Only the turns that ended with
// Stop push turn_complete.
func TestAntigravityHooksReplayed(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	states := []string{wire.StateIdle}
	var requested []wire.Approval
	var question *wire.Question
	turns, resolved := 0, 0
	take := func(m *wire.Msg) {
		switch {
		case m.Method == wire.NotifySessionUpdated:
			var s wire.Session
			json.Unmarshal(m.Params, &s)
			if states[len(states)-1] != s.State {
				states = append(states, s.State)
			}
			if s.Question != nil {
				question = s.Question
			}
		case event(wire.EventApprovalRequested, nil)(m):
			_, ap := decodeEvent[wire.Approval](t, m)
			requested = append(requested, ap)
		case event(wire.EventApprovalResolved, nil)(m):
			resolved++
		case notificationWith(reasonTurnComplete)(m):
			turns++
		}
	}
	reach := func(state string) {
		t.Helper()
		drain(events, take, func() bool { return states[len(states)-1] == state })
		if got := states[len(states)-1]; got != state {
			t.Fatalf("state %s, want %s (states %v)", got, state, states)
		}
	}
	report := func(state string) {
		p := wire.SessionPatch{ID: sid, State: ptr(state), StateSource: ptr(wire.SourceActivity)}
		if state == wire.StateWorking {
			p.LastInputAt = ptr(time.Now().UnixMilli()) // the key answering the menu
		}
		holderConn.Notify(wire.MethodHolderUpdate, p)
	}
	hooks := antigravityHooks(t)
	next := 0
	hook := func(event string) {
		t.Helper()
		p := hooks[next]
		next++
		if p.Event != event {
			t.Fatalf("hook %d is %s, want %s", next, p.Event, event)
		}
		p.SessionID = sid
		_, reply := e.hookAsync(p)
		answered(t, reply, p.Event)
	}
	// menu: the check's watch looks for Antigravity CLI's menu and tells
	// when it shows; it came, then left (answered with the input before).
	menu := func(answer string) {
		t.Helper()
		w := nextPromptWatch(t, holderMsgs, "watch for the menu of the check")
		for !w.On { // the end of the watch of the menu before
			w = nextPromptWatch(t, holderMsgs, "watch for the menu of the check")
		}
		if !w.Shown || w.Harness != wire.HarnessAntigravity {
			t.Fatalf("prompt_watch %+v", w)
		}
		holderConn.Notify(wire.MethodHolderPromptShown, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
		reach(wire.StateNeedsApproval)
		report(answer)
		holderConn.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
		drain(events, take, func() bool { return resolved == len(requested) })
	}

	hook("SessionStart")
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	// A command asked about, allowed: it runs, the turn ends.
	hook("PreToolUse")
	menu(wire.StateWorking)
	reach(wire.StateWorking)
	hook("PostToolUse")
	hook("Stop")
	reach(wire.StateIdle)
	// A question, answered.
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	hook("PreToolUse")
	reach(wire.StateWaitingInput)
	hook("PostToolUse")
	reach(wire.StateWorking)
	hook("Stop")
	reach(wire.StateIdle)
	// A write asked about, dismissed: Antigravity CLI redraws, then waits.
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	hook("PreToolUse")
	menu(wire.StateWorking)
	report(wire.StateIdle)
	reach(wire.StateIdle)
	// The same, turned down.
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	hook("PreToolUse")
	menu(wire.StateWorking)
	report(wire.StateIdle)
	reach(wire.StateIdle)
	// Esc while the model works: its spinner stops, the output goes
	// quiet.
	report(wire.StateWorking)
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	report(wire.StateIdle)
	reach(wire.StateIdle)
	// A turn without tools.
	report(wire.StateWorking)
	hook("UserPromptSubmit")
	reach(wire.StateWorking)
	hook("Stop")
	reach(wire.StateIdle)
	drain(events, take, func() bool { return turns >= 3 })
	if next != len(hooks) {
		t.Fatalf("%d hooks replayed of %d", next, len(hooks))
	}

	asked := []string{wire.StateNeedsApproval, wire.StateWorking}
	want := []string{wire.StateIdle, wire.StateWorking}
	want = append(want, asked...)                                          // the command allowed
	want = append(want, wire.StateIdle, wire.StateWorking)                 // Stop; the question's turn
	want = append(want, wire.StateWaitingInput, wire.StateWorking)         // the question
	want = append(want, wire.StateIdle, wire.StateWorking)                 // Stop; the write's turn
	want = append(want, asked...)                                          // dismissed
	want = append(want, wire.StateIdle, wire.StateWorking)                 // quiet; the next turn
	want = append(want, asked...)                                          // turned down
	want = append(want, wire.StateIdle, wire.StateWorking, wire.StateIdle) // quiet; Esc while working
	want = append(want, wire.StateWorking, wire.StateIdle)                 // a turn without tools
	if !slices.Equal(states, want) {
		t.Errorf("states\n  %v\nwant\n  %v", states, want)
	}
	var tools []string
	for _, ap := range requested {
		if ap.Harness != wire.HarnessAntigravity || ap.SessionID != sid {
			t.Errorf("approval %+v", ap)
		}
		tools = append(tools, ap.ToolName)
	}
	if want := []string{"run_command", "write_file", "write_file"}; !slices.Equal(tools, want) {
		t.Errorf("approvals for %v, want %v", tools, want)
	}
	if len(requested) == 3 && (requested[0].Summary != "run_command: curl -sI https://example.com" || requested[1].Summary != "write_file: /tmp/agy-outside.txt") {
		t.Errorf("approval summaries %q, %q", requested[0].Summary, requested[1].Summary)
	}
	if resolved != len(requested) {
		t.Errorf("%d approvals closed of %d", resolved, len(requested))
	}
	if question == nil || len(question.Questions) != 1 || question.Questions[0].Question != "Do you prefer red or blue?" ||
		!slices.Equal(question.Questions[0].Options, []string{"Red", "Blue"}) || question.Questions[0].Multi {
		t.Errorf("question %+v", question)
	}
	if turns != 3 {
		t.Errorf("%d turn_complete pushes, want 3 (the turns that ended with Stop)", turns)
	}
}

// A check whose call its rules allow brings no menu: the call's next hook
// (here its PostToolUse) ends the check's watch, and no approval is
// registered — not even by a late report of that watch.
func TestAntigravityCheckWithoutMenu(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	hooks := antigravityHooks(t)
	for _, p := range hooks[1:4] { // UserPromptSubmit, PreToolUse, PostToolUse
		p.SessionID = sid
		_, reply := e.hookAsync(p)
		answered(t, reply, p.Event)
	}
	w := nextPromptWatch(t, holderMsgs, "watch for the menu of the check")
	if !w.On || !w.Shown {
		t.Fatalf("prompt_watch %+v", w)
	}
	if off := nextPromptWatch(t, holderMsgs, "end of the check's watch"); off.On || off.Gen != w.Gen {
		t.Fatalf("prompt_watch %+v after the call ran", off)
	}
	holderConn.Notify(wire.MethodHolderPromptShown, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
	awaitNo(t, events, "the turn working on", sessionState(wire.StateWorking), event(wire.EventApprovalRequested, nil))
	time.Sleep(100 * time.Millisecond)
	for {
		select {
		case m := <-events:
			if event(wire.EventApprovalRequested, nil)(m) || sessionState(wire.StateNeedsApproval)(m) {
				t.Fatalf("a check gone by registered an approval: %s", m.Params)
			}
			continue
		default:
		}
		break
	}
}
