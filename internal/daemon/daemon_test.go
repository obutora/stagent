package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
		Notify:             wire.NotifyConfig{DebounceMs: 30, DigestWindowMs: 30},
		ApprovalTimeoutSec: 30,
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

type hookReply struct {
	res wire.HookEventResult
	err error
}

func (e *testEnv) hookAsync(p wire.HookEventParams) <-chan hookReply {
	hc, _ := e.client()
	out := make(chan hookReply, 1)
	go func() {
		var r wire.HookEventResult
		err := hc.Call(context.Background(), wire.MethodHookEvent, p, &r)
		out <- hookReply{r, err}
	}()
	return out
}

func TestSessionLifecycleAndAppApproval(t *testing.T) {
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

	reply := e.hookAsync(wire.HookEventParams{
		Harness: wire.HarnessClaude, SessionID: sid,
		Payload: json.RawMessage(`{"hook_event_name":"PermissionRequest","session_id":"conv-1","transcript_path":"/t/conv-1.jsonl","cwd":"/home/u/proj","tool_name":"Bash","tool_input":{"command":"rm -rf build","description":"clean"}}`),
	})
	m = await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil))
	_, ap := decodeEvent[wire.Approval](t, m)
	if ap.SessionID != sid || ap.ToolName != "Bash" || ap.Summary != "Bash: rm -rf build" || ap.ExpiresAt-ap.CreatedAt != 30000 {
		t.Fatalf("approval %+v", ap)
	}
	m = await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	var s wire.Session
	json.Unmarshal(m.Params, &s)
	if s.StateSource != wire.SourceHook || s.ConversationID != "conv-1" || s.TranscriptPath != "/t/conv-1.jsonl" {
		t.Fatalf("session after PermissionRequest %+v", s)
	}
	m = await(t, events, "needs_approval notification", notificationWith(reasonNeedsApproval))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Title != "claude · proj" || n.Body != "Needs approval: Bash" {
		t.Fatalf("notification %+v", n)
	}
	var list wire.ApprovalsListResult
	call(t, watcher, wire.MethodApprovalsList, nil, &list)
	if len(list.Approvals) != 1 || list.Approvals[0].RequestID != ap.RequestID {
		t.Fatalf("approvals.list %+v", list)
	}
	select {
	case r := <-reply:
		t.Fatalf("hook answered before the app: %+v", r)
	default:
	}

	call(t, watcher, wire.MethodApprovalRespond, wire.ApprovalRespondParams{RequestID: ap.RequestID, Decision: wire.DecisionDeny}, nil)
	select {
	case r := <-reply:
		if r.err != nil || r.res.Decision != wire.DecisionDeny || r.res.Message != defaultDenyMessage {
			t.Fatalf("hook result %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook not answered")
	}
	m = await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd != (wire.ApprovalResolvedData{RequestID: ap.RequestID, Decision: "deny", By: "app"}) {
		t.Fatalf("approval_resolved %+v", rd)
	}
	await(t, events, "working after the decision", sessionState(wire.StateWorking))
	err := watcher.Call(context.Background(), wire.MethodApprovalRespond, wire.ApprovalRespondParams{RequestID: ap.RequestID, Decision: wire.DecisionAllow}, nil)
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrApprovalClosed {
		t.Fatalf("second respond: %v", err)
	}

	r := <-e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", SessionID: sid, Payload: json.RawMessage(`{"session_id":"conv-1"}`)})
	if r.err != nil || r.res.Decision != "" {
		t.Fatalf("Stop hook result %+v", r)
	}
	await(t, events, "turn_complete", notificationWith(reasonTurnComplete))
	await(t, events, "idle", sessionState(wire.StateIdle))

	holderConn.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sid, ExitCode: 2})
	m = await(t, events, "session_ended", event(wire.EventSessionEnded, nil))
	if _, se := decodeEvent[wire.SessionEndedData](t, m); se.ExitCode != 2 {
		t.Fatalf("session_ended %+v", se)
	}
	m = await(t, events, "exited notification", notificationWith(reasonExited))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Level != wire.LevelWarn || n.Body != "Exited with code 2" {
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

func TestApprovalTimeoutFallsBackToTerminal(t *testing.T) {
	cfg := fastConfig()
	cfg.ApprovalTimeoutSec = 1
	e := startDaemon(t, cfg, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	// An agent not wrapped by stagent (no STAGENT_SESSION_ID).
	start := time.Now()
	reply := e.hookAsync(wire.HookEventParams{
		Harness: wire.HarnessCodex, Event: "PermissionRequest",
		Payload: json.RawMessage(`{"cwd":"/srv/api","tool_name":"apply_patch","tool_input":{"command":"*** Begin Patch\n*** Update File: main.go\n*** End Patch"}}`),
	})
	m := await(t, events, "approval_requested", event(wire.EventApprovalRequested, nil))
	_, ap := decodeEvent[wire.Approval](t, m)
	if ap.SessionID != "" || ap.Harness != wire.HarnessCodex || ap.Summary != "apply_patch: Update main.go" {
		t.Fatalf("approval %+v", ap)
	}
	m = await(t, events, "sessionless notification", notificationWith(reasonNeedsApproval))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Title != "codex · api" {
		t.Fatalf("notification title %q", n.Title)
	}
	select {
	case r := <-reply:
		if r.err != nil || r.res.Decision != wire.DecisionNone {
			t.Fatalf("hook result %+v", r)
		}
		if d := time.Since(start); d < 900*time.Millisecond {
			t.Fatalf("answered after %v, before the timeout", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout never answered the hook")
	}
	m = await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd.Decision != wire.DecisionNone || rd.By != "timeout" {
		t.Fatalf("resolved %+v", rd)
	}
	err := watcher.Call(context.Background(), wire.MethodApprovalRespond, wire.ApprovalRespondParams{RequestID: ap.RequestID, Decision: wire.DecisionAllow}, nil)
	var we *wire.Error
	if !errors.As(err, &we) || we.Code != wire.ErrApprovalClosed {
		t.Fatalf("respond after timeout: %v", err)
	}
}

func TestTerminalAnswerCancelsApproval(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	holderConn, _ := e.client()
	call(t, holderConn, wire.MethodHolderRegister, testSession(sid), nil)

	reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Edit","tool_input":{"file_path":"/p/a.go"}}`)})
	await(t, events, "needs_approval", sessionState(wire.StateNeedsApproval))
	// The user answered in the terminal: output resumes after the grace.
	time.Sleep(overrideGraceMs*time.Millisecond + 50*time.Millisecond)
	holderConn.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	m := await(t, events, "approval_resolved", event(wire.EventApprovalResolved, nil))
	if _, rd := decodeEvent[wire.ApprovalResolvedData](t, m); rd.By != "cancelled" || rd.Decision != wire.DecisionNone {
		t.Fatalf("resolved %+v", rd)
	}
	if r := <-reply; r.err != nil || r.res.Decision != wire.DecisionNone {
		t.Fatalf("hook result %+v", r)
	}
}

func TestSessionlessNotificationsFoldIntoOneDigestPush(t *testing.T) {
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
	e := startDaemon(t, cfg, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)

	for _, cwd := range []string{"/w/a", "/w/b", "/w/c"} {
		r := <-e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", Payload: json.RawMessage(`{"cwd":"` + cwd + `"}`)})
		if r.err != nil {
			t.Fatal(r.err)
		}
	}
	for _, want := range []string{"claude · a", "claude · b", "claude · c"} {
		m := await(t, events, "in-app notification", notificationWith(reasonTurnComplete))
		if _, n := decodeEvent[wire.NotificationData](t, m); n.Title != want {
			t.Fatalf("in-app notification %q, want %q", n.Title, want)
		}
	}
	select {
	case n := <-bodies:
		if n.Reason != "digest" || n.Count != 3 || n.Title != "3 agents finished their turn" {
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
	call(t, h, wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "SessionStart", SessionID: sid,
		Payload: json.RawMessage(`{"session_id":"` + conv + `","transcript_path":"` + tpath + `","cwd":"/home/u/proj"}`)}, nil)

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

func TestConfigSetPersistsWithDefaults(t *testing.T) {
	e := startDaemon(t, wire.Config{}, Options{})
	c, _ := e.client()
	in := wire.Config{Notify: wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Topic: "t"}}, ApprovalTimeoutSec: 90}
	var res wire.ConfigResult
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: in}, &res)
	if res.Config.ApprovalTimeoutSec != 90 || res.Config.Notify.Ntfy.Server != "https://ntfy.sh" || res.Config.Retention.EventsMax != 10000 {
		t.Fatalf("config.set result %+v", res.Config)
	}
	st, err := os.Stat(e.layout.Config)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("config file %v %v", st, err)
	}
	var get wire.ConfigResult
	call(t, c, wire.MethodConfigGet, nil, &get)
	if get.Config.ApprovalTimeoutSec != 90 || !get.Config.Notify.Ntfy.Enabled {
		t.Fatalf("config.get %+v", get.Config)
	}
	var reg wire.HolderRegisterResult
	call(t, c, wire.MethodHolderRegister, testSession(sid), &reg)
	if reg.IdleAfterMs != 3000 {
		t.Fatalf("idle_after_ms %d", reg.IdleAfterMs)
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
	err = c.Call(context.Background(), wire.MethodApprovalRespond, wire.ApprovalRespondParams{RequestID: "x", Decision: "maybe"}, nil)
	if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("bad decision: %v", err)
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
