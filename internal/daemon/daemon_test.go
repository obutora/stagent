package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/follow"
	"github.com/obutora/stagent/internal/hostid"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

const sid = "0123456789abcdef"

type testEnv struct {
	t        *testing.T
	layout   *paths.Layout
	d        *Daemon
	done     chan error
	stopOnce sync.Once
}

// fastConfig keeps debounce and digest windows short so tests run quickly.
func fastConfig() wire.Config {
	return wire.Config{
		Notify: wire.NotifyConfig{DebounceMs: 30, DigestWindowMs: 30},
	}
}

// startDaemon runs a daemon in-process over real IPC in an isolated
// STAGENT_HOME.
func startDaemon(t *testing.T, cfg wire.Config, opts Options) *testEnv {
	t.Helper()
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	return startDaemonAt(t, cfg, opts)
}

func startDaemonAt(t *testing.T, cfg wire.Config, opts Options) *testEnv {
	t.Helper()
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(l.Config, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := ipc.Listen(l.DaemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	opts.Layout = l
	opts.Logf = t.Logf
	d, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e := &testEnv{t: t, layout: l, d: d, done: make(chan error, 1)}
	go func() { e.done <- d.Serve(ln) }()
	t.Cleanup(e.stop)
	return e
}

func (e *testEnv) stop() {
	e.stopOnce.Do(func() {
		e.d.Shutdown()
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
			e.t.Error("daemon did not stop")
		}
	})
}

// client connects to the daemon; notifications land in the returned channel.
func (e *testEnv) client() (*rpc.Client, chan *wire.Msg) {
	e.t.Helper()
	c, err := ipc.Dial(e.layout.DaemonAddr, time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	ch := make(chan *wire.Msg, 512)
	cl := rpc.NewClient(c, func(m *wire.Msg) { ch <- m })
	e.t.Cleanup(func() { cl.Close() })
	return cl, ch
}

func call(t *testing.T, c *rpc.Client, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// await returns the first notification matching pred, failing after 5s.
func await(t *testing.T, ch chan *wire.Msg, what string, pred func(*wire.Msg) bool) *wire.Msg {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-ch:
			if pred(m) {
				return m
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func event(kind string, match func(wire.Event) bool) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		if m.Method != wire.NotifyEvent {
			return false
		}
		var ev wire.Event
		json.Unmarshal(m.Params, &ev)
		return ev.Kind == kind && (match == nil || match(ev))
	}
}

func notificationWith(reason string) func(*wire.Msg) bool {
	return event(wire.EventNotification, func(ev wire.Event) bool {
		var n wire.NotificationData
		json.Unmarshal(ev.Data, &n)
		return n.Reason == reason
	})
}

func sessionState(state string) func(*wire.Msg) bool {
	return func(m *wire.Msg) bool {
		if m.Method != wire.NotifySessionUpdated {
			return false
		}
		var s wire.Session
		json.Unmarshal(m.Params, &s)
		return s.State == state
	}
}

func decodeEvent[T any](t *testing.T, m *wire.Msg) (wire.Event, T) {
	t.Helper()
	var ev wire.Event
	var data T
	if err := json.Unmarshal(m.Params, &ev); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatal(err)
	}
	return ev, data
}

func testSession(id string) wire.Session {
	return wire.Session{
		ID: id, Harness: wire.HarnessClaude, Command: []string{"claude"}, Cwd: "/home/u/proj",
		PID: 10, HolderPID: 11, Mode: wire.ModeDetached, State: wire.StateIdle,
		StateSource: wire.SourceActivity, Cols: 80, Rows: 24, StartedAt: time.Now().UnixMilli(),
	}
}

func ptr[T any](v T) *T { return &v }

// hookAsync sends one hook event on its own connection, like `stagent hook`;
// the reply channel yields the call's error once the daemon answers.
// Closing the client is the hook process going away.
func (e *testEnv) hookAsync(p wire.HookEventParams) (*rpc.Client, <-chan error) {
	hc, _ := e.client()
	out := make(chan error, 1)
	go func() {
		out <- hc.Call(context.Background(), wire.MethodHookEvent, p, nil)
	}()
	return hc, out
}

func answered(t *testing.T, reply <-chan error, what string) {
	t.Helper()
	select {
	case err := <-reply:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: hook not answered", what)
	}
}

func TestSessionLifecycleAndClaudeApproval(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	var w wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Sessions) != 0 || w.Seq != 0 || len(w.Missed) != 0 {
		t.Fatalf("fresh watch: %+v", w)
	}

	holderConn, _ := e.client()
	var reg wire.HolderRegisterResult
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), &reg)
	if reg.IdleAfterMs != 3000 {
		t.Fatalf("idle_after_ms = %d", reg.IdleAfterMs)
	}
	m := await(t, events, "session_started", event(wire.EventSessionStarted, nil))
	startedSeq, started := decodeEvent[wire.SessionStartedData](t, m)
	if started.Harness != wire.HarnessClaude || started.Mode != wire.ModeDetached {
		t.Fatalf("session_started %+v", started)
	}

	// Output activity: listed at once, recorded after the debounce.
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	await(t, events, "session working", sessionState(wire.StateWorking))
	m = await(t, events, "state_changed", event(wire.EventStateChanged, nil))
	if _, sc := decodeEvent[wire.StateChangedData](t, m); sc != (wire.StateChangedData{From: "idle", To: "working", Source: "activity"}) {
		t.Fatalf("state_changed %+v", sc)
	}

	hook, reply := e.hookAsync(wire.HookEventParams{
		Harness: wire.HarnessClaude, SessionID: sid,
		Payload: json.RawMessage(`{"hook_event_name":"PermissionRequest","session_id":"conv-1","transcript_path":"/t/conv-1.jsonl","cwd":"/home/u/proj","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"}}`),
	})
	m = await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil))
	_, ap := decodeEvent[wire.Approval](t, m)
	if ap.SessionID != sid || ap.ToolName != "Bash" || ap.Summary != "Bash: rm -rf build" {
		t.Fatalf("approval %+v", ap)
	}
	m = await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	var s wire.Session
	json.Unmarshal(m.Params, &s)
	if s.StateSource != wire.SourceHook || s.ConversationID != "conv-1" || s.TranscriptPath != "/t/conv-1.jsonl" {
		t.Fatalf("session after PermissionRequest %+v", s)
	}
	m = await(t, events, "needs_approval notification", notificationWith(reasonNeedsApproval))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Title != "claude · proj" || n.Body != "Needs approval: Bash" || n.SessionID != sid {
		t.Fatalf("notification %+v", n)
	}
	// A client connecting now sees the pending approval.
	joined, _ := e.client()
	var lw wire.WatchResult
	call(t, joined, wire.MethodWatch, wire.WatchParams{}, &lw)
	if len(lw.Approvals) != 1 || lw.Approvals[0].RequestID != ap.RequestID || len(lw.Sessions) != 1 || lw.Sessions[0].State != wire.StateNeedsApproval {
		t.Fatalf("watch snapshot %+v", lw)
	}
	var list wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 1 || list.Approvals[0].RequestID != ap.RequestID {
		t.Fatalf("approvals.list %+v", list)
	}
	// claude's prompt stays open, and the hook with it, until it is answered.
	select {
	case err := <-reply:
		t.Fatalf("hook answered while the prompt is open: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	err := watcher.Call(context.Background(), "approval.respond", map[string]string{"request_id": ap.RequestID, "decision": "allow"}, nil)
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrUnknownMethod {
		t.Fatalf("approval.respond: %v", err)
	}

	// claude stops the hook (the approved tool finished).
	hook.Close()
	m = await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd != (wire.ApprovalResolvedData{RequestID: ap.RequestID, By: "cancelled"}) {
		t.Fatalf("approval_resolved %+v", rd)
	}
	m = await(t, events, "needs_approval cleared", sessionState(wire.StateWorking))
	json.Unmarshal(m.Params, &s)
	if s.StateSource != wire.SourceActivity {
		t.Fatalf("session after the answer %+v", s)
	}
	call(t, watcher, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 0 {
		t.Fatalf("approvals.list after the answer %+v", list)
	}

	_, stop := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", SessionID: sid, Payload: json.RawMessage(`{"session_id":"conv-1"}`)})
	answered(t, stop, "Stop")
	await(t, events, "turn_complete", notificationWith(reasonTurnComplete))
	await(t, events, "idle", sessionState(wire.StateIdle))

	holderConn.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sid, ExitCode: 2})
	m = await(t, events, "session_ended", event(wire.EventSessionEnded, nil))
	if _, se := decodeEvent[wire.SessionEndedData](t, m); se.ExitCode != 2 {
		t.Fatalf("session_ended %+v", se)
	}
	m = await(t, events, "exited notification", notificationWith(reasonExited))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Level != wire.LevelWarn || n.Body != "Exited with code 2" || n.SessionID != sid {
		t.Fatalf("exit notification %+v", n)
	}

	// A reconnecting app replays what it missed.
	late, _ := e.client()
	var w2 wire.WatchResult
	call(t, late, wire.MethodWatch, wire.WatchParams{Since: startedSeq.Seq}, &w2)
	if w2.Truncated || len(w2.Missed) == 0 || w2.Missed[0].Seq != startedSeq.Seq+1 || w2.Missed[len(w2.Missed)-1].Seq != w2.Seq {
		t.Fatalf("watch since: seq=%d missed=%d truncated=%v", w2.Seq, len(w2.Missed), w2.Truncated)
	}
	if len(w2.Sessions) != 1 || w2.Sessions[0].State != wire.StateExited || w2.Sessions[0].ExitCode == nil || *w2.Sessions[0].ExitCode != 2 {
		t.Fatalf("ended session listing %+v", w2.Sessions)
	}
}

// Approvals nothing here can answer — an agent outside stagent sessions
// (an IDE, `claude -p`, the SDK) — are answered at once, neither registered
// nor pushed.
func TestApprovalOutsideSessionsIsNotTracked(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	for _, h := range []string{wire.HarnessClaude, wire.HarnessCodex} {
		_, reply := e.hookAsync(wire.HookEventParams{
			Harness: h, Event: "PermissionRequest",
			Payload: json.RawMessage(`{"session_id":"conv-x","cwd":"/srv/api","tool_name":"Bash","tool_input":{"command":"touch x"}}`),
		})
		answered(t, reply, h)
	}
	// Claude's own "waiting for permission" notification is not pushed either.
	_, reply := e.hookAsync(wire.HookEventParams{
		Harness: wire.HarnessClaude, Event: "Notification",
		Payload: json.RawMessage(`{"session_id":"conv-x","cwd":"/srv/api","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`),
	})
	answered(t, reply, "Notification")
	var list wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 0 {
		t.Fatalf("approvals.list %+v", list)
	}
	// A later sessionless Stop is reported; nothing about the approval came before it.
	_, stop := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", Payload: json.RawMessage(`{"cwd":"/srv/api"}`)})
	answered(t, stop, "Stop")
	await(t, events, "nothing before the turn_complete notification", func(m *wire.Msg) bool {
		if event(wire.EventApprovalRequested, nil)(m) || notificationWith(reasonNeedsApproval)(m) {
			t.Fatalf("sessionless approval reported: %s", m.Params)
		}
		return notificationWith(reasonTurnComplete)(m)
	})
}

// Codex shows its prompt only after the hook returned, so the hook returns
// at once; the approval closes when output resumes after the grace.
func TestCodexApprovalReturnsAtOnceAndClosesOnActivity(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, _ := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessCodex, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"cwd":"/srv/api","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: main.go\n*** End Patch"}}`)})
	answered(t, reply, "codex PermissionRequest")
	m := await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil))
	if _, ap := decodeEvent[wire.Approval](t, m); ap.SessionID != sid || ap.Harness != wire.HarnessCodex || ap.Summary != "apply_patch: Update main.go" {
		t.Fatalf("approval %+v", ap)
	}
	await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	// The user answered in the terminal: output resumes after the grace.
	time.Sleep(overrideGraceMs*time.Millisecond + 50*time.Millisecond)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	m = await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd.By != "cancelled" {
		t.Fatalf("resolved %+v", rd)
	}
	await(t, events, "working", sessionState(wire.StateWorking))
}

// Codex blinks its terminal title while its prompt is up, so no activity
// transition marks the answer, and an Esc ends the turn without Stop
// (#320). The holder watches for Codex's menu as for claude's: its report
// closes the approval and clears needs_approval.
func TestCodexApprovalClosesWhenPromptLeavesScreen(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessCodex, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"touch /tmp/approve"}}`)})
	answered(t, reply, "codex PermissionRequest")
	_, ap := decodeEvent[wire.Approval](t, await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil)))
	await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	w := nextPromptWatch(t, holderMsgs, "watch for the codex prompt")
	if !w.On || w.ID != sid || w.Gen <= 0 {
		t.Fatalf("prompt_watch %+v", w)
	}

	holderConn.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
	m := await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd != (wire.ApprovalResolvedData{RequestID: ap.RequestID, By: "cancelled"}) {
		t.Fatalf("approval_resolved %+v, want %s", rd, ap.RequestID)
	}
	m = await(t, events, "needs_approval cleared", sessionState(wire.StateIdle))
	var s wire.Session
	json.Unmarshal(m.Params, &s)
	if s.StateSource != wire.SourceActivity {
		t.Fatalf("session after the answer %+v", s)
	}
	if off := nextPromptWatch(t, holderMsgs, "watch off"); off != (wire.HolderPromptWatchParams{ID: sid, Gen: w.Gen}) {
		t.Fatalf("prompt_watch after the approval closed %+v", off)
	}
}

// A claude approval that output activity already closed releases its hook,
// and only the last pending approval of a session clears needs_approval.
func TestClaudeApprovalsCloseOneByOne(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, _ := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	first, _ := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Edit","tool_input":{"file_path":"/p/a.go"}}`)})
	await(t, events, "first approval", event(wire.EventApprovalRequested, nil))
	_, second := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Edit","tool_input":{"file_path":"/p/b.go"}}`)})
	await(t, events, "second approval", event(wire.EventApprovalRequested, nil))

	first.Close()
	await(t, events, "first resolved", event(wire.EventApprovalResolved, nil))
	var w wire.WatchResult
	late, _ := e.client()
	call(t, late, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Approvals) != 1 || w.Sessions[0].State != wire.StateNeedsApproval {
		t.Fatalf("one approval left: %+v", w)
	}

	time.Sleep(overrideGraceMs*time.Millisecond + 50*time.Millisecond)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	await(t, events, "second resolved", event(wire.EventApprovalResolved, nil))
	answered(t, second, "second hook released")
}

// nextPromptWatch returns the next holder.prompt_watch the holder got.
func nextPromptWatch(t *testing.T, holderMsgs chan *wire.Msg, what string) wire.HolderPromptWatchParams {
	t.Helper()
	m := await(t, holderMsgs, what, func(m *wire.Msg) bool { return m.Method == wire.MethodHolderPromptWatch })
	var p wire.HolderPromptWatchParams
	if err := json.Unmarshal(m.Params, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// claude keeps a PermissionRequest hook open after its prompt was answered
// (until the approved tool finished). The holder watching the screen
// reports the menu gone: the approvals of that watch close without any
// Stop, the hook is released and needs_approval clears; an approval
// registered after the watch started (the next prompt) stays pending.
func TestClaudeApprovalClosesWhenPromptLeavesScreen(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)
	permission := func(file string) <-chan error {
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
			Payload: json.RawMessage(`{"tool_name":"Write","tool_input":{"file_path":"` + file + `"}}`)})
		return reply
	}
	pending := func(what string, reply <-chan error) {
		t.Helper()
		select {
		case err := <-reply:
			t.Fatalf("%s: hook answered (%v)", what, err)
		case <-time.After(200 * time.Millisecond):
		}
	}

	first := permission("/p/a.txt")
	_, ap1 := decodeEvent[wire.Approval](t, await(t, events, "first approval", event(wire.EventApprovalRequested, nil)))
	await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	w1 := nextPromptWatch(t, holderMsgs, "watch for the first prompt")
	if !w1.On || w1.ID != sid || w1.Gen <= 0 {
		t.Fatalf("prompt_watch %+v", w1)
	}

	// The next prompt comes before the holder reported the first one gone.
	second := permission("/p/b.txt")
	_, ap2 := decodeEvent[wire.Approval](t, await(t, events, "second approval", event(wire.EventApprovalRequested, nil)))
	w2 := nextPromptWatch(t, holderMsgs, "watch for the second prompt")
	if !w2.On || w2.ID != sid || w2.Gen <= w1.Gen {
		t.Fatalf("second prompt_watch %+v after %+v", w2, w1)
	}
	pending("first", first)

	holderConn.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w1.Gen})
	m := await(t, events, "first resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd != (wire.ApprovalResolvedData{RequestID: ap1.RequestID, By: "cancelled"}) {
		t.Fatalf("approval_resolved %+v, want %s", rd, ap1.RequestID)
	}
	answered(t, first, "first hook released")
	pending("second", second)
	var w wire.WatchResult
	late, _ := e.client()
	call(t, late, wire.MethodWatch, wire.WatchParams{}, &w)
	if len(w.Approvals) != 1 || w.Approvals[0].RequestID != ap2.RequestID || w.Sessions[0].State != wire.StateNeedsApproval {
		t.Fatalf("after the first prompt left: %+v", w)
	}

	holderConn.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w2.Gen})
	m = await(t, events, "second resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd != (wire.ApprovalResolvedData{RequestID: ap2.RequestID, By: "cancelled"}) {
		t.Fatalf("approval_resolved %+v, want %s", rd, ap2.RequestID)
	}
	answered(t, second, "second hook released")
	m = await(t, events, "needs_approval cleared", sessionState(wire.StateIdle))
	var s wire.Session
	json.Unmarshal(m.Params, &s)
	if s.StateSource != wire.SourceActivity {
		t.Fatalf("session after the answer %+v", s)
	}
	if off := nextPromptWatch(t, holderMsgs, "watch off"); off != (wire.HolderPromptWatchParams{ID: sid, Gen: w2.Gen}) {
		t.Fatalf("prompt_watch after the last approval closed %+v", off)
	}
	var al wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &al)
	if len(al.Approvals) != 0 {
		t.Fatalf("approvals left %+v", al.Approvals)
	}
}

// A holder that reconnects while a claude approval is pending is asked to
// watch again; a report from a connection that is not the session's holder
// closes nothing.
func TestPromptWatchFollowsHolderReconnect(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{GoneGrace: 5 * time.Second})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, holderMsgs := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)
	_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"make"}}`)})
	await(t, events, "approval", event(wire.EventApprovalRequested, nil))
	w1 := nextPromptWatch(t, holderMsgs, "watch")

	holderConn.Close()
	again, againMsgs := e.client()
	call(t, again, wire.MethodHolderRegister, testSession(sid), nil)
	if w := nextPromptWatch(t, againMsgs, "watch after re-registering"); w != w1 {
		t.Fatalf("prompt_watch after re-registering %+v, want %+v", w, w1)
	}
	stranger, _ := e.client()
	stranger.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w1.Gen})
	select {
	case err := <-reply:
		t.Fatalf("hook answered on a stranger's report (%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	again.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w1.Gen})
	await(t, events, "resolved", event(wire.EventApprovalResolved, nil))
	answered(t, reply, "hook released")
}

// Hooks of programs not started through stagent notify in-app only; the
// notifications of sessions within one window fold into one digest push.
func TestSessionlessStayInAppAndSessionsFoldIntoOneDigest(t *testing.T) {
	bodies := make(chan wire.NotificationData, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n wire.NotificationData
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &n)
		bodies <- n
	}))
	defer srv.Close()
	cfg := fastConfig()
	cfg.Notify.DigestWindowMs = 300
	cfg.Notify.Webhook = wire.WebhookConfig{Enabled: true, URL: srv.URL}
	cfg.Notify.HostLabel = "box"
	e := startDaemon(t, cfg, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	for _, cwd := range []string{"/w/a", "/w/b"} {
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", Payload: json.RawMessage(`{"cwd":"` + cwd + `"}`)})
		answered(t, reply, "Stop")
	}
	for _, want := range []string{"claude · a", "claude · b"} {
		m := await(t, events, "in-app notification", notificationWith(reasonTurnComplete))
		if _, n := decodeEvent[wire.NotificationData](t, m); n.Title != want || n.SessionID != "" {
			t.Fatalf("in-app notification %q, want %q", n.Title, want)
		}
	}
	select {
	case n := <-bodies:
		t.Fatalf("a hook outside stagent sessions was pushed: %+v", n)
	case <-time.After(600 * time.Millisecond):
	}

	for i, id := range []string{"00000000000000a1", "00000000000000a2", "00000000000000a3"} {
		h, _ := e.client()
		s := testSession(id)
		s.Cwd = "/w/" + string(rune('a'+i))
		call(t, h, wire.MethodHolderRegister, s, nil)
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", SessionID: id})
		answered(t, reply, "Stop")
	}
	select {
	case n := <-bodies:
		if n.Reason != "digest" || n.Count != 3 || n.Title != "box: 3 agents finished their turn" || n.SessionID != "" {
			t.Fatalf("push %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no push")
	}
	select {
	case n := <-bodies:
		t.Fatalf("second push %+v", n)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestHolderDropWithoutEndMarksSessionLost(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{GoneGrace: 150 * time.Millisecond})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	// A holder that reconnects within the grace keeps its session.
	h1, _ := e.client()
	call(t, h1, wire.MethodHolderRegister, testSession(sid), nil)
	await(t, events, "session_started", event(wire.EventSessionStarted, nil))
	h1.Close()
	h2, _ := e.client()
	call(t, h2, wire.MethodHolderRegister, testSession(sid), nil)
	time.Sleep(300 * time.Millisecond)

	// One that does not is declared lost.
	h2.Close()
	m := await(t, events, "session_ended", event(wire.EventSessionEnded, nil))
	if _, se := decodeEvent[wire.SessionEndedData](t, m); se.ExitCode != -1 {
		t.Fatalf("session_ended %+v", se)
	}
	m = await(t, events, "lost notification", notificationWith(reasonExited))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Level != wire.LevelWarn {
		t.Fatalf("notification %+v", n)
	}
	started := 0
	e.d.log.Any(func(ev *wire.Event) bool {
		if ev.Kind == wire.EventSessionStarted {
			started++
		}
		return false
	})
	if started != 1 {
		t.Fatalf("session_started recorded %d times", started)
	}
}

func TestRestartKeepsSeqAndRecognizesReregistration(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	var st wire.DaemonStatus
	call(t, h, wire.MethodDaemonStatus, nil, &st)
	if st.Sessions != 1 || st.PID != os.Getpid() {
		t.Fatalf("status %+v", st)
	}
	w0, _ := e.client()
	var before wire.WatchResult
	call(t, w0, wire.MethodWatch, wire.WatchParams{}, &before)
	e.stop()

	e2 := startDaemonAt(t, fastConfig(), Options{})
	watcher, events := e2.client()
	var w wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{Since: before.Seq}, &w)
	if w.Seq != before.Seq || len(w.Sessions) != 0 {
		t.Fatalf("after restart: seq %d (was %d), sessions %d", w.Seq, before.Seq, len(w.Sessions))
	}
	h2, _ := e2.client()
	call(t, h2, wire.MethodHolderRegister, testSession(sid), nil)
	await(t, events, "session listed again", func(m *wire.Msg) bool { return m.Method == wire.NotifySessionUpdated })
	var sl wire.SessionsListResult
	call(t, watcher, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 {
		t.Fatalf("sessions %+v", sl)
	}
	if e2.d.log.Seq() != before.Seq {
		t.Fatalf("re-registration logged new events: seq %d → %d", before.Seq, e2.d.log.Seq())
	}
}

func TestTranscriptGetSubscribeAndConversations(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	roots := transcript.Roots{Claude: filepath.Join(home, "claude", "projects"), Codex: filepath.Join(home, "codex"), Omp: filepath.Join(home, "omp")}
	conv := "2a425a50-3f53-4e94-8acb-f2d54d242f33"
	tpath := filepath.Join(roots.Claude, "-home-u-proj", conv+".jsonl")
	os.MkdirAll(filepath.Dir(tpath), 0o700)
	line := func(role, text string) string {
		if role == "user" {
			return `{"type":"user","cwd":"/home/u/proj","entrypoint":"cli","timestamp":"2026-09-01T00:00:00Z","message":{"role":"user","content":"` + text + `"}}` + "\n"
		}
		return `{"type":"assistant","timestamp":"2026-09-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}]}}` + "\n"
	}
	os.WriteFile(tpath, []byte(line("user", "hello")+line("assistant", "hi")), 0o600)

	e := startDaemonAt(t, fastConfig(), Options{Roots: &roots, TranscriptPoll: 50 * time.Millisecond})
	c, notes := e.client()
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	tpathJSON, _ := json.Marshal(tpath) // a Windows path has backslashes
	call(t, h, wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "SessionStart", SessionID: sid,
		Payload: json.RawMessage(`{"session_id":"` + conv + `","transcript_path":` + string(tpathJSON) + `,"cwd":"/home/u/proj"}`)}, nil)

	var got wire.TranscriptGetResult
	call(t, c, wire.MethodTranscriptGet, wire.TranscriptGetParams{SessionID: sid}, &got)
	if got.Path != tpath || got.Harness != wire.HarnessClaude || len(got.Messages) != 2 || got.Messages[1].Text != "hi" || got.Cursor != 0 {
		t.Fatalf("transcript.get %+v", got)
	}
	var byConv wire.TranscriptGetResult
	call(t, c, wire.MethodTranscriptGet, wire.TranscriptGetParams{Harness: wire.HarnessClaude, ConversationID: conv, Limit: 1}, &byConv)
	if len(byConv.Messages) != 1 || byConv.Messages[0].Text != "hi" || byConv.Cursor == 0 {
		t.Fatalf("transcript.get by conversation %+v", byConv)
	}
	err := c.Call(context.Background(), wire.MethodTranscriptGet, wire.TranscriptGetParams{Path: filepath.Join(home, "secret.jsonl")}, nil)
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("path outside the transcript roots: %v", err)
	}

	var convs wire.ConversationsListResult
	call(t, c, wire.MethodConversationsList, wire.ConversationsListParams{}, &convs)
	if len(convs.Conversations) != 1 || convs.Conversations[0].ID != conv || convs.Conversations[0].LiveSessionID != sid {
		t.Fatalf("conversations %+v", convs)
	}

	call(t, c, wire.MethodTranscriptSubscribe, wire.TranscriptSubscribeParams{SessionID: sid}, nil)
	f, _ := os.OpenFile(tpath, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(line("user", "next question"))
	f.Close()
	m := await(t, notes, "transcript", func(m *wire.Msg) bool { return m.Method == wire.NotifyTranscript })
	var tp wire.TranscriptParams
	json.Unmarshal(m.Params, &tp)
	if tp.SessionID != sid || tp.Path != tpath || len(tp.Messages) != 1 || tp.Messages[0].Text != "next question" {
		t.Fatalf("transcript notification %+v", tp)
	}

	call(t, c, wire.MethodTranscriptUnsubscribe, wire.TranscriptSubscribeParams{SessionID: sid}, nil)
	f, _ = os.OpenFile(tpath, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(line("assistant", "after unsubscribe"))
	f.Close()
	select {
	case m := <-notes:
		if m.Method == wire.NotifyTranscript {
			t.Fatalf("notification after unsubscribe: %s", m.Params)
		}
	case <-time.After(300 * time.Millisecond):
	}
	e.d.mu.Lock()
	tailing := e.d.tailing
	e.d.mu.Unlock()
	if tailing {
		t.Fatal("tail loop still polling without subscriptions")
	}
}

// config.set is a JSON Merge Patch on config.json as stored: only the keys
// it names change, unknown and unnamed keys stay, null deletes a key, and
// the defaults of the result never reach the file.
func TestConfigSetMergesPatch(t *testing.T) {
	e := startDaemon(t, wire.Config{}, Options{})
	stored := `{"future_key": {"a": 1}, "idle_after_ms": 4000, "retired_key": 90,
	  "notify": {"ntfy": {"enabled": true, "topic": "t", "unknown": "x"}}}`
	if err := os.WriteFile(e.layout.Config, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := e.client()
	patch := `{"disable_handoff": true, "retired_key": null, "notify": {"ntfy": {"topic": "u"}}}`
	var res wire.ConfigResult
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(patch)}, &res)
	n := res.Config.Notify.Ntfy
	if !res.Config.DisableHandoff || res.Config.IdleAfterMs != 4000 ||
		!n.Enabled || n.Topic != "u" || n.Server != "https://ntfy.sh" || res.Config.Retention.EventsMax != 10000 {
		t.Fatalf("config.set result %+v", res.Config)
	}
	fileJSON := func() any {
		t.Helper()
		st, err := os.Stat(e.layout.Config)
		// Windows has no permission bits: Go reports 0666 for any writable file.
		if err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
			t.Fatalf("config file %v %v", st, err)
		}
		b, _ := os.ReadFile(e.layout.Config)
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("config file %q: %v", b, err)
		}
		return v
	}
	var want any
	json.Unmarshal([]byte(`{"future_key": {"a": 1}, "idle_after_ms": 4000, "disable_handoff": true,
	  "notify": {"ntfy": {"enabled": true, "topic": "u", "unknown": "x"}}}`), &want)
	if got := fileJSON(); !reflect.DeepEqual(got, want) {
		t.Fatalf("config file = %v, want %v", got, want)
	}
	var get wire.ConfigResult
	call(t, c, wire.MethodConfigGet, nil, &get)
	if !reflect.DeepEqual(get.Config, res.Config) {
		t.Fatalf("config.get %+v, config.set returned %+v", get.Config, res.Config)
	}
	var reg wire.HolderRegisterResult
	call(t, c, wire.MethodHolderRegister, testSession(sid), &reg)
	if reg.IdleAfterMs != 4000 {
		t.Fatalf("idle_after_ms %d", reg.IdleAfterMs)
	}

	// A result that is not a valid Config is refused and nothing is written.
	for _, bad := range []string{
		`{"notify": {"webhook": {"enabled": true, "url": "ftp://x"}}}`,
		`{"idle_after_ms": "soon"}`,
		`[1]`,
	} {
		var we *wire.Error
		err := c.Call(t.Context(), wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(bad)}, nil)
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("config.set %s: %v, want bad_request", bad, err)
		}
	}
	if got := fileJSON(); !reflect.DeepEqual(got, want) {
		t.Fatalf("config file after refused patches = %v", got)
	}
}

// config.set stores the ntfy channel and the host label the app sets,
// keeping the keys it does not name; config.get and config.set report the
// channels' last failures ({} when none).
func TestConfigSetStoresNotifySettings(t *testing.T) {
	e := startDaemon(t, wire.Config{}, Options{})
	stored := `{"future_key": 1, "notify": {"debounce_ms": 100, "webhook": {"enabled": true, "url": "https://hook.example", "extra": true}}}`
	if err := os.WriteFile(e.layout.Config, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := e.client()
	set := func(patch string) (wire.ConfigResult, json.RawMessage) {
		t.Helper()
		var raw json.RawMessage
		call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(patch)}, &raw)
		var res wire.ConfigResult
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatal(err)
		}
		return res, raw
	}
	hostID := hostid.Read(e.layout.HostID)
	if !hostid.Valid(hostID) {
		t.Fatalf("daemon start left no host id (%q)", hostID)
	}
	res, raw := set(`{"notify": {"host_label": "開発機", "click_base": "sshtermx://open", "click_page": "https://sshterm.iru-yo.com/open", "ntfy": {"server": "https://ntfy.example", "token": "tk", "topic": "t1", "enabled": true}}}`)
	want := wire.NtfyConfig{Enabled: true, Server: "https://ntfy.example", Topic: "t1", Token: "tk"}
	if res.Config.Notify.Ntfy != want || res.Config.Notify.HostLabel != "開発機" || res.Config.Notify.ClickBase != "sshtermx://open" ||
		res.Config.Notify.ClickPage != "https://sshterm.iru-yo.com/open" || res.Config.Notify.DebounceMs != 100 || !res.Config.Notify.Webhook.Enabled {
		t.Fatalf("config.set result %+v", res.Config.Notify)
	}
	if err := c.Call(context.Background(), wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {"click_base": "open"}}`)}, nil); err == nil {
		t.Fatal("a click_base without a scheme was accepted")
	}
	// Refused click_pages write nothing (the file is checked below).
	for _, page := range []string{"http://sshterm.iru-yo.com/open", "sshtermx://open", "/open", "https://sshterm.iru-yo.com/open?x=1", "https://sshterm.iru-yo.com/open#h=1"} {
		var we *wire.Error
		patch, _ := json.Marshal(map[string]any{"notify": map[string]any{"click_page": page}})
		if err := c.Call(context.Background(), wire.MethodConfigSet, wire.ConfigSetParams{Config: patch}, nil); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("click_page %q: %v, want bad_request", page, err)
		}
	}
	var doc struct {
		Notify map[string]json.RawMessage `json:"notify"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || string(doc.Notify["last_error"]) != "{}" {
		t.Fatalf("config.set reply %s", raw)
	}
	res, _ = set(`{"notify": {"ntfy": {"enabled": false}}}`)
	want.Enabled = false
	if res.Config.Notify.Ntfy != want || res.Config.Notify.HostLabel != "開発機" {
		t.Fatalf("after disabling %+v", res.Config.Notify)
	}
	b, _ := os.ReadFile(e.layout.Config)
	var got, wantFile any
	json.Unmarshal(b, &got)
	json.Unmarshal([]byte(`{"future_key": 1, "notify": {"debounce_ms": 100, "host_label": "開発機", "click_base": "sshtermx://open", "click_page": "https://sshterm.iru-yo.com/open",
	  "webhook": {"enabled": true, "url": "https://hook.example", "extra": true},
	  "ntfy": {"server": "https://ntfy.example", "token": "tk", "topic": "t1", "enabled": false}}}`), &wantFile)
	if !reflect.DeepEqual(got, wantFile) {
		t.Fatalf("config file = %v, want %v", got, wantFile)
	}
	// Removing notify (unpreparing the host) keeps the host id.
	set(`{"notify": null}`)
	if got := hostid.Read(e.layout.HostID); got != hostID {
		t.Fatalf("host id %q changed to %q", hostID, got)
	}
	var getRaw json.RawMessage
	call(t, c, wire.MethodConfigGet, nil, &getRaw)
	if err := json.Unmarshal(getRaw, &doc); err != nil || string(doc.Notify["last_error"]) != "{}" {
		t.Fatalf("config.get reply %s", getRaw)
	}
}

// notify.test refuses without a channel and sends nothing; otherwise it
// pushes a host-labelled test. A failure is reported without the topic,
// shows in config.get until a push succeeds, and survives a restart.
func TestNotifyTestAndLastErrors(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusTooManyRequests
	titles := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title, _ := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		titles <- title
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	defer srv.Close()

	e := startDaemon(t, fastConfig(), Options{})
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	var we *wire.Error
	if err := c.Call(t.Context(), wire.MethodNotifyTest, nil, nil); !errors.As(err, &we) || we.Code != wire.ErrNotConfigured {
		t.Fatalf("notify.test without a channel: %v, want not_configured", err)
	}
	for quiet := time.After(200 * time.Millisecond); ; {
		select {
		case m := <-events:
			if notificationWith(reasonTest)(m) {
				t.Fatal("notify.test without a channel emitted a notification")
			}
			continue
		case <-quiet:
		}
		break
	}

	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(
		`{"notify": {"host_label": "box", "ntfy": {"enabled": true, "server": "` + srv.URL + `", "topic": "topic-secret"}}}`)}, nil)
	err := c.Call(t.Context(), wire.MethodNotifyTest, nil, nil)
	if !errors.As(err, &we) || we.Code != wire.ErrUnavailable || !strings.Contains(we.Message, "429 Too Many Requests") || strings.Contains(we.Message, "secret") {
		t.Fatalf("notify.test against a failing server: %v", err)
	}
	if title := <-titles; title != "box · SSH Term" {
		t.Fatalf("test push titled %q", title)
	}
	await(t, events, "in-app test notification", notificationWith(reasonTest))
	lastError := func(c *rpc.Client) map[string]wire.NotifyFailure {
		t.Helper()
		var get wire.ConfigResult
		call(t, c, wire.MethodConfigGet, nil, &get)
		return get.Notify.LastError
	}
	failed := lastError(c)
	if f := failed[wire.ChannelNtfy]; len(failed) != 1 || f.Status != 429 || f.Error != "429 Too Many Requests" || f.At == 0 {
		t.Fatalf("last_error %+v", failed)
	}

	e.stop()
	cfg := fastConfig()
	cfg.Notify.Ntfy = wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "topic-secret"}
	e = startDaemonAt(t, cfg, Options{})
	c, _ = e.client()
	if got := lastError(c); !reflect.DeepEqual(got, failed) {
		t.Fatalf("last_error after a restart %+v, want %+v", got, failed)
	}

	mu.Lock()
	status = http.StatusOK
	mu.Unlock()
	call(t, c, wire.MethodNotifyTest, nil, nil)
	<-titles
	if got := lastError(c); got == nil || len(got) != 0 {
		t.Fatalf("last_error after a success %+v", got)
	}
}

// config.set changing notify.chat.url (to null included) forgets the
// chat destination's last failure; disabling it or changing another
// channel keeps it.
func TestConfigSetChatURLClearsLastError(t *testing.T) {
	const failures = `{"ntfy": {"at": 1767225600000, "status": 429, "error": "429 Too Many Requests"},
	  "chat": {"at": 1767225600000, "status": 404, "error": "404 Not Found: Unknown Webhook (10015)", "kind": "revoked"}}`
	cfg := fastConfig()
	cfg.Notify.Chat = wire.ChatConfig{Enabled: true, URL: "discord://123456/AAold"}
	e := startDaemon(t, cfg, Options{})
	e.stop()
	restart := func() *rpc.Client {
		t.Helper()
		e.stop()
		if err := os.WriteFile(e.layout.NotifyErrors, []byte(failures), 0o600); err != nil {
			t.Fatal(err)
		}
		e = startDaemonAt(t, cfg, Options{})
		c, _ := e.client()
		return c
	}
	set := func(c *rpc.Client, patch string) map[string]wire.NotifyFailure {
		t.Helper()
		var res wire.ConfigResult
		call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(patch)}, &res)
		return res.Notify.LastError
	}

	c := restart()
	if got := set(c, `{"notify": {"chat": {"enabled": false}}}`); got[wire.ChannelChat].Kind != wire.FailureRevoked || len(got) != 2 {
		t.Fatalf("after disabling the chat: %+v", got)
	}
	if got := set(c, `{"notify": {"chat": {"url": "discord://123456/AAold"}, "ntfy": {"topic": "t2"}}}`); len(got) != 2 {
		t.Fatalf("after setting the same chat url and another topic: %+v", got)
	}
	if got := set(c, `{"notify": {"chat": {"enabled": true, "url": "discord://123456/AAnew"}}}`); len(got) != 1 || got[wire.ChannelNtfy].Status != 429 {
		t.Fatalf("after a new chat url: %+v", got)
	}
	if b, _ := os.ReadFile(e.layout.NotifyErrors); strings.Contains(string(b), "revoked") {
		t.Fatalf("the failures file kept the chat failure: %s", b)
	}

	c = restart()
	if got := set(c, `{"notify": {"chat": {"url": null}}}`); len(got) != 1 || got[wire.ChannelNtfy].Status != 429 {
		t.Fatalf("after removing the chat url: %+v", got)
	}
}

// notify.test pushes only to the channels it names; an unknown name is
// bad_request and named channels that are all off are not_configured.
// config.set refuses a chat URL of no known service without writing
// anything or repeating the URL.
func TestNotifyTestChannelsAndChatURL(t *testing.T) {
	got := make(chan string, 8)
	server := func(name string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- name }))
		t.Cleanup(srv.Close)
		return srv
	}
	ntfy, hook := server(wire.ChannelNtfy), server(wire.ChannelWebhook)
	e := startDaemon(t, fastConfig(), Options{})
	c, _ := e.client()
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {
	  "ntfy": {"enabled": true, "server": "` + ntfy.URL + `", "topic": "t"},
	  "webhook": {"enabled": true, "url": "` + hook.URL + `"},
	  "chat": {"enabled": false, "url": "tgram://123456789:AAsecret/@my_channel"}}}`)}, nil)
	before, err := os.ReadFile(e.layout.Config)
	if err != nil {
		t.Fatal(err)
	}

	call(t, c, wire.MethodNotifyTest, wire.NotifyTestParams{Channels: []string{wire.ChannelWebhook}}, nil)
	if ch := <-got; ch != wire.ChannelWebhook {
		t.Fatalf("notify.test webhook pushed to %s", ch)
	}
	call(t, c, wire.MethodNotifyTest, json.RawMessage(`{}`), nil)
	if a, b := <-got, <-got; a == b {
		t.Fatalf("notify.test without channels pushed to %s and %s", a, b)
	}
	var we *wire.Error
	if err := c.Call(t.Context(), wire.MethodNotifyTest, wire.NotifyTestParams{Channels: []string{"ntfy", "email"}}, nil); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("notify.test with an unknown channel: %v, want bad_request", err)
	}
	if err := c.Call(t.Context(), wire.MethodNotifyTest, wire.NotifyTestParams{Channels: []string{wire.ChannelChat}}, nil); !errors.As(err, &we) || we.Code != wire.ErrNotConfigured {
		t.Fatalf("notify.test of a disabled chat: %v, want not_configured", err)
	}
	select {
	case ch := <-got:
		t.Fatalf("a refused notify.test pushed to %s", ch)
	case <-time.After(200 * time.Millisecond):
	}

	for _, u := range []string{
		"https://hooks.slack.com/triggers/T000/123/AAsecret",
		"https://discord.com.example/api/webhooks/1/AAsecret",
		"tgram://123456789:AAsecret/not-a-chat",
	} {
		err := c.Call(t.Context(), wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {"chat": {"enabled": true, "url": "` + u + `"}}}`)}, nil)
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest || strings.Contains(we.Message, "secret") {
			t.Fatalf("config.set chat url %s: %v, want bad_request without the URL", u, err)
		}
	}
	if after, _ := os.ReadFile(e.layout.Config); !bytes.Equal(after, before) {
		t.Fatalf("a refused config.set wrote %s", after)
	}
}

func TestSlowReaderIsDisconnected(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	conns := make(chan *rpc.Conn, 1)
	served := make(chan struct{})
	go func() {
		rpc.Serve(context.Background(), a, func(_ context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
			conns <- c
			return rpc.Async, nil
		})
		close(served)
	}()
	// Send one request, then never read: every write from the server blocks.
	go wire.NewCodec(b, b).Request(1, wire.MethodWatch, nil)
	o := newOutbox(<-conns)
	for i := 0; i < outboxSize+2; i++ {
		o.push(&wire.Msg{Method: wire.NotifyEvent})
	}
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a reader that fell behind was not disconnected")
	}
}

func TestRetentionSweep(t *testing.T) {
	cfg := fastConfig()
	cfg.Retention = wire.RetentionConfig{ScrollbackDays: 3, ScrollbackTotalMiB: 1}
	e := startDaemon(t, cfg, Options{})
	h, _ := e.client()
	live := "aaaaaaaaaaaaaaaa"
	call(t, h, wire.MethodHolderRegister, testSession(live), nil)

	mk := func(id string, age time.Duration, size int) string {
		dir := e.layout.SessionDataDir(id)
		os.MkdirAll(dir, 0o700)
		p := filepath.Join(dir, "0000000000.seg")
		os.WriteFile(p, make([]byte, size), 0o600)
		mt := time.Now().Add(-age)
		os.Chtimes(p, mt, mt)
		os.Chtimes(dir, mt, mt)
		return dir
	}
	liveDir := mk(live, 10*24*time.Hour, 10)             // old but registered
	oldDir := mk("bbbbbbbbbbbbbbbb", 4*24*time.Hour, 10) // past ScrollbackDays
	bigOld := mk("cccccccccccccccc", time.Hour, 700<<10) // over the total, oldest
	bigNew := mk("dddddddddddddddd", time.Minute, 700<<10)
	e.d.sweepNow()
	for dir, keep := range map[string]bool{liveDir: true, oldDir: false, bigOld: false, bigNew: true} {
		_, err := os.Stat(dir)
		if exists := err == nil; exists != keep {
			t.Errorf("%s exists=%v, want %v", filepath.Base(dir), exists, keep)
		}
	}
}

func TestUnknownMethodAndBadParams(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	c, _ := e.client()
	var we *wire.Error
	err := c.Call(context.Background(), "session.attach", wire.AttachParams{ID: sid}, nil)
	if !errors.As(err, &we) || we.Code != wire.ErrUnknownMethod {
		t.Fatalf("holder method on the daemon: %v", err)
	}
}

func TestFlappingStateIsDebounced(t *testing.T) {
	cfg := fastConfig()
	cfg.Notify.DebounceMs = 300
	e := startDaemon(t, cfg, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil) // idle
	for _, st := range []string{wire.StateWorking, wire.StateIdle, wire.StateWorking, wire.StateIdle} {
		h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(st), StateSource: ptr(wire.SourceActivity)})
	}
	// Every change is listed live...
	for i := 0; i < 4; i++ {
		await(t, events, "session.updated", func(m *wire.Msg) bool { return m.Method == wire.NotifySessionUpdated })
	}
	// ...but back where it started, nothing is recorded.
	time.Sleep(600 * time.Millisecond)
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	m := await(t, events, "state_changed", event(wire.EventStateChanged, nil))
	if _, sc := decodeEvent[wire.StateChangedData](t, m); sc.From != wire.StateIdle || sc.To != wire.StateWorking {
		t.Fatalf("state_changed %+v", sc)
	}
	var w wire.WatchResult
	call(t, watcher, wire.MethodWatch, wire.WatchParams{Since: 1}, &w)
	changes := 0
	for _, ev := range w.Missed {
		if ev.Kind == wire.EventStateChanged {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("%d state_changed events recorded, want 1", changes)
	}
}

func TestActivityOnlyUpdatesAreHeldBack(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	await(t, events, "registration", func(m *wire.Msg) bool { return m.Method == wire.NotifySessionUpdated })

	updated := func() wire.Session {
		m := await(t, events, "session.updated", func(m *wire.Msg) bool { return m.Method == wire.NotifySessionUpdated })
		var s wire.Session
		json.Unmarshal(m.Params, &s)
		return s
	}
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastActivityAt: ptr(int64(1000))})
	if s := updated(); s.LastActivityAt != 1000 {
		t.Fatalf("first activity push carries %d", s.LastActivityAt)
	}
	// Within the interval, timestamps alone are not pushed...
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastActivityAt: ptr(int64(2000))})
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastActivityAt: ptr(int64(3000))})
	// ...but the next real change carries the latest one.
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, Title: ptr("build")})
	if s := updated(); s.Title != "build" || s.LastActivityAt != 3000 {
		t.Fatalf("next push = title %q activity %d, want build/3000", s.Title, s.LastActivityAt)
	}
}

// sessionUpdate returns the next session.updated of id.
func sessionUpdate(t *testing.T, events chan *wire.Msg, what, id string, pred func(wire.Session) bool) wire.Session {
	t.Helper()
	var s wire.Session
	await(t, events, what, func(m *wire.Msg) bool {
		if m.Method != wire.NotifySessionUpdated {
			return false
		}
		var u wire.Session
		json.Unmarshal(m.Params, &u)
		if u.ID != id || !pred(u) {
			return false
		}
		s = u
		return true
	})
	return s
}

// attached goes out as soon as the holder reports it (not held back like
// activity), survives a holder reconnect and ends with the session.
func TestAttachedIsPushedAtOnce(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	sessionUpdate(t, events, "registration", sid, func(s wire.Session) bool { return !s.Attached })
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastActivityAt: ptr(int64(1000))})
	sessionUpdate(t, events, "first activity push", sid, func(s wire.Session) bool { return s.LastActivityAt == 1000 })

	// Within the activity interval: attached is a real change.
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, Attached: ptr(true)})
	sessionUpdate(t, events, "attached", sid, func(s wire.Session) bool { return s.Attached })
	var sl wire.SessionsListResult
	call(t, watcher, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 || !sl.Sessions[0].Attached {
		t.Fatalf("sessions.list %+v", sl.Sessions)
	}
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, Attached: ptr(false)})
	sessionUpdate(t, events, "detached", sid, func(s wire.Session) bool { return !s.Attached })

	// A reconnecting holder registers its current value.
	h.Close()
	h2, _ := e.client()
	ws := testSession(sid)
	ws.Attached = true
	call(t, h2, wire.MethodHolderRegister, ws, nil)
	sessionUpdate(t, events, "attached after re-registration", sid, func(s wire.Session) bool { return s.Attached })

	h2.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sid})
	sessionUpdate(t, events, "ended, not attached", sid, func(s wire.Session) bool { return s.ExitCode != nil && !s.Attached })
}

// last_local_input_at goes out as soon as the holder reports it (not held
// back like activity) and survives a holder reconnect.
func TestLocalInputIsPushedAtOnce(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	h, _ := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	sessionUpdate(t, events, "registration", sid, func(s wire.Session) bool { return s.LastLocalInputAt == 0 })
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastActivityAt: ptr(int64(1000))})
	sessionUpdate(t, events, "first activity push", sid, func(s wire.Session) bool { return s.LastActivityAt == 1000 })

	// Within the activity interval: each keystroke time is a real change.
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastLocalInputAt: ptr(int64(1500))})
	sessionUpdate(t, events, "first keystroke", sid, func(s wire.Session) bool { return s.LastLocalInputAt == 1500 })
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, LastLocalInputAt: ptr(int64(2500))})
	sessionUpdate(t, events, "next keystroke", sid, func(s wire.Session) bool { return s.LastLocalInputAt == 2500 })
	var sl wire.SessionsListResult
	call(t, watcher, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 || sl.Sessions[0].LastLocalInputAt != 2500 {
		t.Fatalf("sessions.list %+v", sl.Sessions)
	}

	// A reconnecting holder registers its current value.
	h.Close()
	h2, _ := e.client()
	ws := testSession(sid)
	ws.LastLocalInputAt = 3500
	call(t, h2, wire.MethodHolderRegister, ws, nil)
	sessionUpdate(t, events, "keystroke after re-registration", sid, func(s wire.Session) bool { return s.LastLocalInputAt == 3500 })
}

// fakeProbe stands in for follow.AgentProbe: pids in running have their
// agent running.
type fakeProbe struct {
	mu      sync.Mutex
	running map[int]bool
	asked   map[int]int // probes per pid
}

func (p *fakeProbe) probe(checks []follow.SessionAgent) ([]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]bool, len(checks))
	for i, c := range checks {
		p.asked[c.PID]++
		out[i] = p.running[c.PID] && c.Harness == wire.HarnessClaude
	}
	return out, nil
}

func (p *fakeProbe) set(pid int, running bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running[pid] = running
	p.asked[pid] = 0
}

// waitAsked waits until pid was probed n times since the last set.
func (p *fakeProbe) waitAsked(t *testing.T, pid, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p.mu.Lock()
		asked := p.asked[pid]
		p.mu.Unlock()
		if asked >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never probed", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A shell session that hooks promoted to claude returns to other once the
// probe no longer finds claude running in it, dropping everything the
// agent's hooks set; a session started as claude is never probed.
func TestShellSessionRevertsWhenAgentLeaves(t *testing.T) {
	fp := &fakeProbe{running: map[int]bool{}, asked: map[int]int{}}
	e := startDaemon(t, fastConfig(), Options{AgentProbe: fp.probe, ProbeInterval: 20 * time.Millisecond})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	const agentSID = "fedcba9876543210"
	h, _ := e.client()
	shell := testSession(sid)
	shell.Harness, shell.Command = "", []string{"bash", "-l"}
	call(t, h, wire.MethodHolderRegister, shell, nil)
	sessionUpdate(t, events, "shell registered", sid, func(s wire.Session) bool { return s.Harness == wire.HarnessOther })
	h2, _ := e.client()
	agent := testSession(agentSID)
	agent.PID = 20
	call(t, h2, wire.MethodHolderRegister, agent, nil)

	hook := func(id, event, payload string) <-chan error {
		_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: event, SessionID: id, Payload: json.RawMessage(payload)})
		return reply
	}
	fp.set(10, true)
	<-hook(agentSID, "UserPromptSubmit", `{"session_id":"conv-a","prompt":"other"}`)
	<-hook(sid, "SessionStart", `{"session_id":"conv-1","transcript_path":"/t/conv-1.jsonl"}`)
	sessionUpdate(t, events, "promoted to claude", sid, func(s wire.Session) bool {
		return s.Harness == wire.HarnessClaude && s.ConversationID == "conv-1" && s.TranscriptPath == "/t/conv-1.jsonl"
	})
	<-hook(sid, "UserPromptSubmit", `{"session_id":"conv-1","prompt":"fix the build"}`)
	sessionUpdate(t, events, "working on a prompt", sid, func(s wire.Session) bool {
		return s.LastMessage == "fix the build" && s.StateSource == wire.SourceHook
	})
	reply := hook(sid, "PermissionRequest", `{"session_id":"conv-1","tool_name":"Bash","tool_input":{"command":"make"}}`)
	await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil))
	// Probes taken after the last hook see claude running.
	fp.set(10, true)
	fp.waitAsked(t, 10, 2)
	e.d.mu.Lock()
	seen := e.d.sessions[sid].agentSeen
	e.d.mu.Unlock()
	if !seen {
		t.Fatal("probe did not record claude as running")
	}

	// claude quits: its approval is cancelled and the session is a plain
	// shell again.
	fp.set(10, false)
	m := await(t, events, "approval cancelled", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd.By != "cancelled" {
		t.Fatalf("resolved %+v", rd)
	}
	s := sessionUpdate(t, events, "reverted to other", sid, func(s wire.Session) bool { return s.Harness == wire.HarnessOther })
	if s.ConversationID != "" || s.TranscriptPath != "" || s.LastMessage != "" ||
		s.State != wire.StateIdle || s.StateSource != wire.SourceActivity {
		t.Fatalf("reverted session %+v", s)
	}
	answered(t, reply, "PermissionRequest released")
	var al wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &al)
	if len(al.Approvals) != 0 {
		t.Fatalf("approvals left %+v", al.Approvals)
	}

	// A later hook promotes it again; an agent the probe never finds is
	// given up after unseenProbes.
	<-hook(sid, "UserPromptSubmit", `{"session_id":"conv-2","prompt":"again"}`)
	sessionUpdate(t, events, "promoted again", sid, func(s wire.Session) bool {
		return s.Harness == wire.HarnessClaude && s.ConversationID == "conv-2"
	})
	sessionUpdate(t, events, "never seen, reverted", sid, func(s wire.Session) bool { return s.Harness == wire.HarnessOther })

	fp.mu.Lock()
	askedAgent := fp.asked[20]
	fp.mu.Unlock()
	if askedAgent != 0 {
		t.Fatalf("session started as claude was probed %d times", askedAgent)
	}
	var sl wire.SessionsListResult
	call(t, watcher, wire.MethodSessionsList, nil, &sl)
	for _, ls := range sl.Sessions {
		if ls.ID == agentSID && ls.Harness != wire.HarnessClaude {
			t.Fatalf("agent session %+v", ls)
		}
	}
	// Nothing promoted is left: the probe loop stops.
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.d.mu.Lock()
		probing := e.d.probing
		e.d.mu.Unlock()
		if !probing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe loop still running without promoted sessions")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
