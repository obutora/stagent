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

// testdata/opencode-hooks.jsonl: the hooks stagent's plugin sent for the
// events of one OpenCode 2.3.0 terminal UI (verified against a local fake
// model; paths shortened), in order, but the Transcript reads the hook
// keeps to itself: a shell command allowed once and a file write allowed
// always, a shell command turned down (the turn interrupted:
// StopCancelled), a question answered, and a turn cut short by closing the
// terminal UI.
func openCodeHooks(t *testing.T) []json.RawMessage {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "opencode-hooks.jsonl"))
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

// Replayed in order, the session works, waits on each approval (the hook
// returns at once: OpenCode never waits on the plugin) until OpenCode
// tells it was answered (PermissionReplied, here or anywhere: the PC's
// answer closes the app's approval), waits on the question, and ends its
// turns, pushing turn_complete for those that ended with Stop. No prompt
// watch is needed: OpenCode reports every answer.
func TestOpenCodeHooksReplayed(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	states := []string{wire.StateIdle}
	var requested []string
	resolved, turns := 0, 0
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
			if ap.Harness != wire.HarnessOpenCode {
				t.Errorf("approval %+v", ap)
			}
			requested = append(requested, ap.ToolName)
		case event(wire.EventApprovalResolved, nil)(m):
			resolved++
		case notificationWith(reasonTurnComplete)(m):
			turns++
		}
	}

	for _, payload := range openCodeHooks(t) {
		var p struct {
			Event string `json:"hook_event_name"`
		}
		json.Unmarshal(payload, &p)
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessOpenCode, Event: p.Event, SessionID: sid, Payload: payload})
		answered(t, reply, p.Event)
		if p.Event == "PermissionReplied" {
			var list wire.ApprovalsListResult
			call(t, watcher, wire.MethodApprovalsList, nil, &list)
			if len(list.Approvals) != 0 {
				t.Errorf("after %s: approvals %+v", payload, list.Approvals)
			}
		}
	}

	want := []string{
		wire.StateIdle,
		wire.StateWorking, wire.StateNeedsApproval, // the shell command asked about
		wire.StateWorking, wire.StateNeedsApproval, // allowed once; the write asked about
		wire.StateWorking, wire.StateIdle, // allowed always; the turn ends
		wire.StateWorking, wire.StateNeedsApproval, wire.StateWorking, wire.StateIdle, // turned down; the turn ends
		wire.StateWorking, wire.StateWaitingInput, wire.StateWorking, wire.StateIdle, // the question
		wire.StateWorking, // cut short: the terminal UI closed
	}
	drain(events, take, func() bool { return len(states) >= len(want) && len(requested) >= 3 && resolved >= 3 && turns >= 2 })
	if got := states[:min(len(states), len(want))]; !slices.Equal(got, want) {
		t.Errorf("states\n  %v\nwant\n  %v", states, want)
	}
	if want := []string{"shell", "write", "shell"}; !slices.Equal(requested, want) || resolved != 3 {
		t.Errorf("approvals for %v (%d resolved), want %v", requested, resolved, want)
	}
	if turns != 2 {
		t.Errorf("%d turn_complete pushes, want 2 (the turns that ended with Stop)", turns)
	}
	for {
		select {
		case m := <-holderMsgs:
			if m.Method == wire.MethodHolderPromptWatch {
				t.Fatalf("prompt watch asked: %s", m.Params)
			}
		default:
			return
		}
	}
}
