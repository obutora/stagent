package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/grok-hooks.jsonl: the hooks Grok Build 1.0.77 sent for one
// terminal UI session (verified against a local fake model; paths
// shortened), in order, as stagent's hook forwards them: the Stop of its
// exit (reason shutdown) is dropped by the hook, and stagent does not
// install PermissionDenied. A turn without tools; a shell command and a
// write asked about and allowed; an auto-allowed command; commands turned
// down (StopCancelled, no Stop); more allowed and turned down; the
// idle_prompt that comes 60 s after a turn settled; a question; a menu
// dismissed with Ctrl-C; the end.
func grokHooks(t *testing.T) []json.RawMessage {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "grok-hooks.jsonl"))
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

// Replayed in order, the session works, waits on each approval —
// registered by the permission_prompt notification, named by the tool of
// the PreToolUse before it — until its menu is answered, waits on the
// question, and is idle after each turn. A menu allowed on the PC sends no
// hook until the tool finished: the holder reports the menu gone from the
// screen, and the session shows what its screen shows (here, idle). A
// menu turned down or dismissed ends the turn with StopCancelled, which
// closes the approval itself and pushes no turn_complete.
func TestGrokHooksReplayed(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	states := []string{wire.StateIdle}
	var requested []string
	turns, resolved := 0, 0
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
			if ap.Harness != wire.HarnessGrok {
				t.Errorf("approval %+v", ap)
			}
			requested = append(requested, ap.ToolName)
		case event(wire.EventApprovalResolved, nil)(m):
			resolved++
		case notificationWith(reasonTurnComplete)(m):
			turns++
		}
	}

	hooks := grokHooks(t)
	menus := 0 // the permission_prompt notifications sent
	for i, payload := range hooks {
		var p struct {
			Event string `json:"hook_event_name"`
			Note  string `json:"notification_type"`
			Tool  string `json:"tool_name"`
		}
		json.Unmarshal(payload, &p)
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessGrok, Event: p.Event, SessionID: sid, Payload: payload})
		answered(t, reply, p.Event)
		switch {
		case p.Event == "Notification" && p.Note == "permission_prompt":
			menus++
			w := nextPromptWatch(t, holderMsgs, "watch for the grok menu")
			for !w.On { // the end of the watch of the menu before
				w = nextPromptWatch(t, holderMsgs, "watch for the grok menu")
			}
			if w.Harness != wire.HarnessGrok {
				t.Fatalf("prompt_watch %+v", w)
			}
			if i+1 < len(hooks) && eventName(hooks[i+1]) == "StopCancelled" {
				break // the turn ends before the menu's leaving is seen
			}
			// Allowed on the PC: the menu left the screen. The next hook
			// comes on another connection: it waits until this approval
			// closed (every earlier one closed before it).
			holderConn.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
			drain(events, take, func() bool { return resolved >= menus })
		case p.Event == "PreToolUse" && p.Tool == "ask_user_question":
			m := await(t, holderMsgs, "question watch", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderQuestionWatch })
			var w wire.HolderPromptWatchParams
			json.Unmarshal(m.Params, &w)
			if !w.On || w.Harness != wire.HarnessGrok {
				t.Errorf("question_watch %+v: not Grok Build's question (Qwen Code's tool has its name too)", w)
			}
		}
	}

	asked := []string{wire.StateWorking, wire.StateNeedsApproval, wire.StateIdle}
	want := []string{wire.StateIdle, wire.StateWorking, wire.StateIdle}                               // no tools
	want = append(want, asked...)                                                                     // the command allowed
	want = append(want, wire.StateNeedsApproval, wire.StateIdle)                                      // the write allowed; Stop
	want = append(want, wire.StateWorking, wire.StateIdle)                                            // auto-allowed
	want = append(want, asked...)                                                                     // turned down
	want = append(want, asked...)                                                                     // turned down
	want = append(want, asked...)                                                                     // allowed; Stop
	want = append(want, wire.StateWorking, wire.StateIdle)                                            // auto-allowed
	want = append(want, asked...)                                                                     // allowed; Stop
	want = append(want, asked...)                                                                     // allowed; Stop
	want = append(want, asked...)                                                                     // turned down
	want = append(want, wire.StateWaitingInput)                                                       // idle_prompt, 60 s later
	want = append(want, wire.StateWorking, wire.StateWaitingInput, wire.StateWorking, wire.StateIdle) // the question
	want = append(want, asked...)                                                                     // Ctrl-C at the menu
	drain(events, take, func() bool {
		return len(states) >= len(want) && turns >= 8 && resolved >= len(requested) && len(requested) >= 9
	})
	if !slices.Equal(states, want) {
		t.Errorf("states\n  %v\nwant\n  %v", states, want)
	}
	if want := []string{
		"run_terminal_command", "write", "run_terminal_command", "run_terminal_command", "run_terminal_command",
		"run_terminal_command", "run_terminal_command", "run_terminal_command", "run_terminal_command",
	}; !slices.Equal(requested, want) {
		t.Errorf("approvals for %v, want %v", requested, want)
	}
	if resolved != len(requested) {
		t.Errorf("%d approvals closed of %d", resolved, len(requested))
	}
	if turns != 8 {
		t.Errorf("%d turn_complete pushes, want 8 (the turns that ended with Stop)", turns)
	}
}

func eventName(payload json.RawMessage) string {
	var p struct {
		Event string `json:"hook_event_name"`
	}
	json.Unmarshal(payload, &p)
	return p.Event
}
