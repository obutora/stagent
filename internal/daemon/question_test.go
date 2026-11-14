package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// askPayload is claude's hook payload of event for an AskUserQuestion call
// with two questions (the second multiSelect), from
// testdata/claude-ask-hooks.jsonl: Claude Code 2.1.325's hooks, captured
// with stagent's installed, for one call answered in the terminal
// (PreToolUse, PermissionRequest — without tool_use_id —, the
// permission_prompt Notification some 6 s later, PostToolUse, Stop; paths
// redacted).
func askPayload(event string) json.RawMessage {
	b, err := os.ReadFile(filepath.Join("testdata", "claude-ask-hooks.jsonl"))
	if err != nil {
		panic(err)
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		var p struct {
			Event string `json:"hook_event_name"`
		}
		if json.Unmarshal(line, &p) == nil && p.Event == event {
			return json.RawMessage(line)
		}
	}
	panic("no " + event + " in testdata/claude-ask-hooks.jsonl")
}

// awaitNo fails if a message matching bad arrives before one matching want.
func awaitNo(t *testing.T, ch chan *wire.Msg, what string, want, bad func(*wire.Msg) bool) *wire.Msg {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-ch:
			if bad(m) {
				t.Fatalf("while waiting for %s: unexpected %s %s", what, m.Method, m.Params)
			}
			if want(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func anyOf(preds ...func(*wire.Msg) bool) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		for _, p := range preds {
			if p(m) {
				return true
			}
		}
		return false
	}
}

// claude runs AskUserQuestion through its permission flow: the PreToolUse
// hook, then a PermissionRequest hook naming the tool, then a
// permission_prompt Notification ("Claude needs your permission") while the
// question is on screen; PostToolUse once it is answered. The session
// waits for input with the question attached; nothing is recorded as an
// approval and no hook is held.
func TestClaudeQuestionWaitsForInputWithoutApproval(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, _ := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	await(t, events, "state_changed working", event(wire.EventStateChanged, nil))

	approvalish := anyOf(event(wire.EventApprovalRequested, nil), sessionState(wire.StateNeedsApproval), notificationWith(reasonNeedsApproval))

	_, pre := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload("PreToolUse")})
	answered(t, pre, "PreToolUse")
	m := awaitNo(t, events, "question", sessionState(wire.StateWaitingInput), approvalish)
	var s wire.Session
	json.Unmarshal(m.Params, &s)
	want := &wire.Question{Tool: "AskUserQuestion", Questions: []wire.QuestionItem{
		{Question: "好きな色はどれですか？", Header: "色", Options: []string{"赤", "青", "緑"}},
		{Question: "好きな果物をすべて選んでください", Header: "果物", Multi: true, Options: []string{"りんご", "みかん", "ぶどう"}},
	}}
	if s.StateSource != wire.SourceHook || !sameQuestion(s.Question, want) {
		t.Fatalf("session after PreToolUse: state %s/%s question %+v", s.State, s.StateSource, s.Question)
	}

	// The PermissionRequest hook returns at once: claude shows its question.
	_, perm := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload("PermissionRequest")})
	answered(t, perm, "PermissionRequest of AskUserQuestion")
	_, note := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload("Notification")})
	answered(t, note, "Notification")

	m = awaitNo(t, events, "question notification", notificationWith(reasonWaitingInput), approvalish)
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != "好きな色はどれですか？ (2 questions)" || !n.Question || n.SessionID != sid {
		t.Fatalf("notification %+v", n)
	}
	var lw wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, &lw)
	if len(lw.Approvals) != 0 || len(lw.Sessions) != 1 || lw.Sessions[0].State != wire.StateWaitingInput || lw.Sessions[0].Question == nil {
		t.Fatalf("watch snapshot while asking: %+v", lw)
	}

	// Answered: back to working, the question gone.
	_, post := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload("PostToolUse")})
	answered(t, post, "PostToolUse")
	m = awaitNo(t, events, "answered", sessionState(wire.StateWorking), approvalish)
	s = wire.Session{}
	json.Unmarshal(m.Params, &s)
	if s.Question != nil {
		t.Fatalf("question kept after the answer: %+v", s.Question)
	}
	var list wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 0 {
		t.Fatalf("approvals after the answer %+v", list)
	}
}

// The question stays while its UI is on the screen (#1548): output then is
// the user moving through it — the highlight, the tabs, a multi-select's
// checks — not the agent working. The holder watches the screen for the
// question UI (holder.question_watch, one watch per question in force:
// claude's PermissionRequest after its PreToolUse starts none); its report
// that the UI left (holder.question_gone) ends the question — answered or
// cancelled there, and an Esc sends no hook — and hands the state back to
// the holder.
func TestQuestionStaysWhileItsUIIsOnScreen(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	await(t, events, "state_changed working", event(wire.EventStateChanged, nil))

	for _, ev := range []string{"PreToolUse", "PermissionRequest"} {
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload(ev)})
		answered(t, reply, ev)
	}
	await(t, events, "question", sessionState(wire.StateWaitingInput))
	w := nextQuestionWatch(t, holderMsgs, "question watch")
	if !w.On || w.ID != sid || w.Gen <= 0 || w.Harness != wire.HarnessClaude {
		t.Fatalf("question_watch %+v", w)
	}

	// The user moves through the question in the terminal, well after its
	// hooks.
	time.Sleep(overrideGraceMs*time.Millisecond + 50*time.Millisecond)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, Title: ptr("moved")})
	s := sessionUpdate(t, events, "title after the output", sid, func(s wire.Session) bool { return s.Title == "moved" })
	if s.State != wire.StateWaitingInput || s.Question == nil {
		t.Fatalf("session after output while the question is up: %s/%s question %+v", s.State, s.StateSource, s.Question)
	}

	// Its UI left the screen (Esc): the holder's state takes over.
	holderConn.Notify(wire.MethodHolderQuestionGone, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
	s = sessionUpdate(t, events, "question gone", sid, func(s wire.Session) bool { return s.State != wire.StateWaitingInput })
	if s.State != wire.StateWorking || s.StateSource != wire.SourceActivity || s.Question != nil {
		t.Fatalf("session after the question UI left: %s/%s question %+v", s.State, s.StateSource, s.Question)
	}
	if off := nextQuestionWatch(t, holderMsgs, "question watch off"); off != (wire.HolderPromptWatchParams{ID: sid, Gen: w.Gen}) {
		t.Fatalf("question_watch after the question went %+v", off)
	}
}

// A question that ends by a hook (answered: PostToolUse) ends its watch; a
// late report of that watch changes nothing, and the next question starts
// a new one.
func TestQuestionWatchEndsWithTheQuestion(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	hook := func(ev string) {
		t.Helper()
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: askPayload(ev)})
		answered(t, reply, ev)
	}
	hook("PreToolUse")
	w1 := nextQuestionWatch(t, holderMsgs, "first question watch")
	hook("PostToolUse")
	await(t, events, "answered", sessionState(wire.StateWorking))
	if off := nextQuestionWatch(t, holderMsgs, "watch off"); off != (wire.HolderPromptWatchParams{ID: sid, Gen: w1.Gen}) {
		t.Fatalf("question_watch after the answer %+v", off)
	}

	hook("PreToolUse")
	w2 := nextQuestionWatch(t, holderMsgs, "second question watch")
	if !w2.On || w2.Gen <= w1.Gen {
		t.Fatalf("second question_watch %+v after %+v", w2, w1)
	}
	await(t, events, "asked again", sessionState(wire.StateWaitingInput))
	holderConn.Notify(wire.MethodHolderQuestionGone, wire.HolderPromptGoneParams{ID: sid, Gen: w1.Gen})
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, Title: ptr("later")})
	if s := sessionUpdate(t, events, "title", sid, func(s wire.Session) bool { return s.Title == "later" }); s.State != wire.StateWaitingInput || s.Question == nil {
		t.Fatalf("an old watch's report ended the next question: %s question %+v", s.State, s.Question)
	}
}

// nextQuestionWatch returns the next holder.question_watch the holder got.
func nextQuestionWatch(t *testing.T, holderMsgs chan *wire.Msg, what string) wire.HolderPromptWatchParams {
	t.Helper()
	m := await(t, holderMsgs, what, func(m *wire.Msg) bool { return m.Method == wire.MethodHolderQuestionWatch })
	var p wire.HolderPromptWatchParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// One question: the body is its text alone.
func TestSingleQuestionNotificationBody(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, _ := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	await(t, events, "state_changed working", event(wire.EventStateChanged, nil))

	_, perm := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, SessionID: sid, Payload: json.RawMessage(
		`{"hook_event_name":"PermissionRequest","session_id":"conv-1","tool_name":"AskUserQuestion","tool_input":{"questions":[{"question":"Ship  it\nnow?","header":"Ship","options":[{"label":"Yes"},{"label":"No"}]}]}}`)})
	answered(t, perm, "PermissionRequest")
	m := await(t, events, "question notification", notificationWith(reasonWaitingInput))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != "Ship it now?" || !n.Question {
		t.Fatalf("notification %+v", n)
	}
}

// codex's request_user_input and omp's ask are question tools too: their
// PreToolUse puts the session in waiting_input with the question and a
// push of its text, their PostToolUse (answered) puts it back to working.
//
// codex: codex 0.160.1's PreToolUse and PostToolUse of a request_user_input
// call in Plan mode, as a hook command got them (the paths redacted); its
// tool_input is the call's arguments, tool_response the answers.
// omp: what stagent's omp extension sends for omp 18.12.6's tool_call /
// tool_result events of its ask tool (tools/ask.ts parameters).
func TestCodexAndOmpQuestionsWaitForInput(t *testing.T) {
	for _, c := range []struct {
		harness, pre, post string
		want               *wire.Question
	}{
		{
			harness: wire.HarnessCodex,
			pre: `{"session_id":"01a1d9ae-6af8-7b22-8786-d8b914307c02","turn_id":"01a1d9b8-774b-7231-b895-f9e0fe1720f1","transcript_path":"/home/u/.codex/sessions/2026/11/14/rollout-2026-11-14T19-50-48-01a1d9ae-6af8-7b22-8786-d8b914307c02.jsonl","cwd":"/home/u/proj","hook_event_name":"PreToolUse","model":"gpt-6.1-sol","permission_mode":"default","tool_name":"request_user_input",` +
				`"tool_input":{"questions":[{"id":"drink","header":"飲み物","question":"飲み物はどれにしますか？","options":[{"label":"コーヒー","description":"コーヒーを選びます。"},{"label":"紅茶","description":"紅茶を選びます。"}]}]},"tool_use_id":"call_9d63ee32b3784cf593069e9563caceb5"}`,
			post: `{"session_id":"01a1d9ae-6af8-7b22-8786-d8b914307c02","turn_id":"01a1d9b8-774b-7231-b895-f9e0fe1720f1","transcript_path":"/home/u/.codex/sessions/2026/11/14/rollout-2026-11-14T19-50-48-01a1d9ae-6af8-7b22-8786-d8b914307c02.jsonl","cwd":"/home/u/proj","hook_event_name":"PostToolUse","model":"gpt-6.1-sol","permission_mode":"default","tool_name":"request_user_input",` +
				`"tool_input":{"questions":[{"id":"drink","header":"飲み物","question":"飲み物はどれにしますか？","options":[{"label":"コーヒー","description":"コーヒーを選びます。"},{"label":"紅茶","description":"紅茶を選びます。"}]}]},"tool_response":"{\"answers\":{\"drink\":{\"answers\":[\"コーヒー\"]}}}","tool_use_id":"call_9d63ee32b3784cf593069e9563caceb5"}`,
			want: &wire.Question{Tool: "request_user_input", Questions: []wire.QuestionItem{{ID: "drink", Question: "飲み物はどれにしますか？", Header: "飲み物", Options: []string{"コーヒー", "紅茶"}}}},
		},
		{
			harness: wire.HarnessOmp,
			pre: `{"hook_event_name":"PreToolUse","session_id":"01a1d94f-a347-77b1-86d2-9bf3d2036b63","transcript_path":"","cwd":"/home/u/proj","tool_name":"ask",` +
				`"tool_input":{"questions":[{"id":"fruit","question":"Which fruits do you like?","header":"Fruit","options":[{"label":"Apple"},{"label":"Banana","description":"yellow"}],"multi":true,"recommended":0}]}}`,
			post: `{"hook_event_name":"PostToolUse","session_id":"01a1d94f-a347-77b1-86d2-9bf3d2036b63","transcript_path":"","cwd":"/home/u/proj","tool_name":"ask"}`,
			want: &wire.Question{Tool: "ask", Questions: []wire.QuestionItem{{ID: "fruit", Question: "Which fruits do you like?", Header: "Fruit", Multi: true, Options: []string{"Apple", "Banana"}}}},
		},
	} {
		t.Run(c.harness, func(t *testing.T) {
			e := startDaemon(t, fastConfig(), Options{})
			watcher, events := e.client()
			call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
			holderConn, _ := e.client()
			ts := testSession(sid)
			ts.Harness, ts.Command = c.harness, []string{c.harness}
			call(t, holderConn, wire.MethodHolderRegister, ts, nil)
			holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
			await(t, events, "state_changed working", event(wire.EventStateChanged, nil))
			approvalish := anyOf(event(wire.EventApprovalRequested, nil), sessionState(wire.StateNeedsApproval), notificationWith(reasonNeedsApproval))

			_, pre := e.hookAsync(wire.HookEventParams{Harness: c.harness, SessionID: sid, Payload: json.RawMessage(c.pre)})
			answered(t, pre, "PreToolUse")
			m := awaitNo(t, events, "question", sessionState(wire.StateWaitingInput), approvalish)
			var s wire.Session
			json.Unmarshal(m.Params, &s)
			if s.StateSource != wire.SourceHook || !sameQuestion(s.Question, c.want) {
				t.Fatalf("session after PreToolUse: state %s/%s question %+v", s.State, s.StateSource, s.Question)
			}
			m = awaitNo(t, events, "question notification", notificationWith(reasonWaitingInput), approvalish)
			if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != c.want.Questions[0].Question || !n.Question || n.SessionID != sid {
				t.Fatalf("notification %+v", n)
			}

			_, post := e.hookAsync(wire.HookEventParams{Harness: c.harness, SessionID: sid, Payload: json.RawMessage(c.post)})
			answered(t, post, "PostToolUse")
			m = awaitNo(t, events, "answered", sessionState(wire.StateWorking), approvalish)
			s = wire.Session{}
			json.Unmarshal(m.Params, &s)
			if s.Question != nil {
				t.Fatalf("question kept after the answer: %+v", s.Question)
			}
		})
	}
}

// Outside a stagent session a question is told in-app once: claude's by
// its PermissionRequest (its PreToolUse came first), codex's and omp's by
// their PreToolUse, the only hook they send for it.
func TestQuestionOutsideSessionsNotifiesOnce(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	question := notificationWith(reasonWaitingInput)

	_, pre := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Payload: askPayload("PreToolUse")})
	answered(t, pre, "claude PreToolUse")
	_, pre = e.hookAsync(wire.HookEventParams{Harness: wire.HarnessCodex, Payload: json.RawMessage(
		`{"session_id":"019a0000-0000-7000-8000-000000000002","cwd":"/home/u/api","hook_event_name":"PreToolUse","tool_name":"request_user_input","tool_use_id":"call_2","tool_input":{"questions":[{"id":"go","header":"Go","question":"Deploy now?","options":[{"label":"Yes","description":""},{"label":"No","description":""}]}]}}`)})
	answered(t, pre, "codex PreToolUse")
	m := await(t, events, "codex question", question)
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != "Deploy now?" || !n.Question || n.Title != "codex · api" {
		t.Fatalf("codex notification %+v (claude's PreToolUse must not notify)", n)
	}

	_, perm := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Payload: askPayload("PermissionRequest")})
	answered(t, perm, "claude PermissionRequest")
	m = await(t, events, "claude question", question)
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != "好きな色はどれですか？ (2 questions)" {
		t.Fatalf("claude notification %+v", n)
	}
}

func sameQuestion(a, b *wire.Question) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
