package daemon

import (
	"encoding/json"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// step applies one input to a stateTrack.
type step func(stateTrack) stateTrack

func holder(state, source string, at int64) step {
	return func(t stateTrack) stateTrack { return t.holderReport(state, source, at) }
}
func hook(state string, at int64) step {
	return func(t stateTrack) stateTrack { return t.hookState(state, at) }
}
func clearHook() step { return func(t stateTrack) stateTrack { return t.hookClear() } }
func exit() step      { return func(t stateTrack) stateTrack { return t.exited() } }

const (
	working = wire.StateWorking
	idle    = wire.StateIdle
	waiting = wire.StateWaitingInput
	needsAp = wire.StateNeedsApproval
	exited  = wire.StateExited
	srcHook = wire.SourceHook
	srcTerm = wire.SourceTerminal
	srcAct  = wire.SourceActivity
	srcProc = wire.SourceProcess
)

func TestMergedState(t *testing.T) {
	cases := []struct {
		name        string
		steps       []step
		state, from string
	}{
		{"nothing reported", nil, idle, srcAct},
		{"holder only", []step{holder(working, srcAct, 100)}, working, srcAct},
		{"terminal state without hook", []step{holder(waiting, srcTerm, 100)}, waiting, srcTerm},
		{"hook beats later idle activity", []step{hook(working, 100), holder(idle, srcAct, 5000)}, working, srcHook},
		{"approval survives output going quiet", []step{hook(needsAp, 100), holder(idle, srcAct, 5000)}, needsAp, srcHook},
		{"later working activity overrides hook", []step{hook(needsAp, 100), holder(working, srcAct, 2000)}, working, srcAct},
		{"working inside the grace does not override", []step{hook(needsAp, 100), holder(working, srcAct, 100+overrideGraceMs-1)}, needsAp, srcHook},
		{"working before the hook does not override", []step{holder(working, srcAct, 50), hook(needsAp, 100)}, needsAp, srcHook},
		{"terminal working does not override", []step{hook(waiting, 100), holder(working, srcTerm, 5000)}, waiting, srcHook},
		{"override consumes the hook state", []step{hook(idle, 100), holder(working, srcAct, 2000), holder(idle, srcAct, 6000)}, idle, srcAct},
		{"new hook after override wins again", []step{hook(idle, 100), holder(working, srcAct, 2000), hook(needsAp, 3000), holder(idle, srcAct, 9000)}, needsAp, srcHook},
		{"process exit beats hook", []step{hook(needsAp, 100), exit()}, exited, srcProc},
		{"holder reports exit", []step{holder(working, srcAct, 10), holder(exited, srcProc, 100)}, exited, srcProc},
		{"exit is final", []step{exit(), holder(working, srcAct, 9999), hook(working, 9999)}, exited, srcProc},
		{"SessionEnd hands back to the holder", []step{holder(idle, srcAct, 50), hook(working, 100), clearHook()}, idle, srcAct},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var tr stateTrack
			for _, s := range c.steps {
				tr = s(tr)
			}
			state, source := tr.merged()
			if state != c.state || source != c.from {
				t.Fatalf("merged = %s/%s, want %s/%s", state, source, c.state, c.from)
			}
		})
	}
}

func TestClassifyHook(t *testing.T) {
	payload := func(s string) hookPayload { return parseHookPayload(json.RawMessage(s)) }
	cases := []struct {
		name    string
		event   string
		payload string
		want    hookEffect
	}{
		{"prompt", "UserPromptSubmit", `{}`, hookEffect{event: hookUserPromptSubmit, state: working}},
		{"stop", "Stop", `{}`, hookEffect{event: hookStop, state: idle, turnComplete: true}},
		{"permission", "PermissionRequest", `{"tool_name":"Bash"}`, hookEffect{event: hookPermissionRequest, state: needsAp, approval: true}},
		{"event from payload", "", `{"hook_event_name":"UserPromptSubmit"}`, hookEffect{event: hookUserPromptSubmit, state: working}},
		{"permission prompt", "Notification", `{"notification_type":"permission_prompt","message":"x"}`, hookEffect{event: hookNotification, state: needsAp}},
		{"idle prompt", "Notification", `{"notification_type":"idle_prompt"}`, hookEffect{event: hookNotification, state: waiting}},
		{"elicitation", "Notification", `{"notification_type":"elicitation_dialog"}`, hookEffect{event: hookNotification, state: waiting}},
		{"auth success", "Notification", `{"notification_type":"auth_success","message":"ok"}`, hookEffect{event: hookNotification}},
		{"legacy permission message", "Notification", `{"message":"Claude needs your permission to use Bash"}`, hookEffect{event: hookNotification, state: needsAp}},
		{"legacy idle message", "Notification", `{"message":"Claude is waiting for your input"}`, hookEffect{event: hookNotification, state: waiting}},
		{"codex notify", "notify", `{"type":"agent-turn-complete","thread-id":"t1"}`, hookEffect{event: hookStop, state: idle, turnComplete: true}},
		{"session end", "SessionEnd", `{}`, hookEffect{event: hookSessionEnd, clear: true}},
		{"session start", "SessionStart", `{}`, hookEffect{event: hookSessionStart}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyHook(c.event, payload(c.payload)); got != c.want {
				t.Fatalf("classifyHook = %+v, want %+v", got, c.want)
			}
		})
	}
	if p := payload(`{"type":"agent-turn-complete","thread-id":"t1","last-assistant-message":"done"}`); p.SessionID != "t1" || p.LastAssistantMessage != "done" {
		t.Fatalf("codex notify payload not mapped: %+v", p)
	}
}

func TestNotifyReasonForTransition(t *testing.T) {
	cases := []struct{ from, to, want string }{
		{working, waiting, reasonWaitingInput},
		{idle, waiting, ""}, // idle reminders after a finished turn do not re-notify
		{idle, needsAp, reasonNeedsApproval},
		{working, needsAp, reasonNeedsApproval},
		{needsAp, needsAp, ""},
		{working, idle, ""},   // turn completion is notified by the Stop hook
		{working, exited, ""}, // exits are notified by session end
	}
	for _, c := range cases {
		if got := notifyReasonForTransition(c.from, c.to); got != c.want {
			t.Errorf("%s → %s = %q, want %q", c.from, c.to, got, c.want)
		}
	}
}
