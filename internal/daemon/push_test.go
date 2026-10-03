package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// ntfyReq is one request to the fake ntfy server: a push (POST) or a
// clear (PUT <topic>/<sequence id>/clear).
type ntfyReq struct {
	method, path, seq, title, body string
	at                             time.Time
}

func (r ntfyReq) clear() bool { return r.method == http.MethodPut }

// pushEnv runs a daemon pushing to a fake ntfy server (ntfy 2.16+: its
// reply names the sequence ID).
func pushEnv(t *testing.T, mutate func(*wire.Config), opts Options) (*testEnv, <-chan ntfyReq) {
	t.Helper()
	reqs := make(chan ntfyReq, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		title, _ := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		seq := r.Header.Get("X-Sequence-ID")
		reqs <- ntfyReq{r.Method, r.URL.Path, seq, title, string(b), time.Now()}
		json.NewEncoder(w).Encode(map[string]string{"id": "m1", "sequence_id": seq})
	}))
	t.Cleanup(srv.Close)
	cfg := fastConfig()
	cfg.Notify.HostLabel = "box"
	cfg.Notify.Ntfy = wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}
	if mutate != nil {
		mutate(&cfg)
	}
	return startDaemon(t, cfg, opts), reqs
}

func nextReq(t *testing.T, reqs <-chan ntfyReq, what string) ntfyReq {
	t.Helper()
	select {
	case r := <-reqs:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing reached ntfy", what)
	}
	return ntfyReq{}
}

func noReq(t *testing.T, reqs <-chan ntfyReq, what string, wait time.Duration) {
	t.Helper()
	select {
	case r := <-reqs:
		t.Fatalf("%s: unexpected %s %s %q", what, r.method, r.path, r.body)
	case <-time.After(wait):
	}
}

func wantPush(t *testing.T, reqs <-chan ntfyReq, what, id, body string) ntfyReq {
	t.Helper()
	r := nextReq(t, reqs, what)
	if r.clear() || r.path != "/t" || r.seq != id || r.body != body {
		t.Fatalf("%s: got %s %s seq %q body %q, want a push of %s %q", what, r.method, r.path, r.seq, r.body, id, body)
	}
	return r
}

func wantClear(t *testing.T, reqs <-chan ntfyReq, what, id string) {
	t.Helper()
	if r := nextReq(t, reqs, what); !r.clear() || r.path != "/t/"+id+"/clear" {
		t.Fatalf("%s: got %s %s %q, want the clear of %s", what, r.method, r.path, r.body, id)
	}
}

// register adds a session held by a new connection (its holder).
func (e *testEnv) register(s wire.Session) *rpc.Client {
	e.t.Helper()
	h, _ := e.client()
	call(e.t, h, wire.MethodHolderRegister, s, nil)
	return h
}

func (e *testEnv) hook(p wire.HookEventParams) {
	e.t.Helper()
	_, reply := e.hookAsync(p)
	answered(e.t, reply, p.Event)
}

func stop(id string) wire.HookEventParams {
	return wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", SessionID: id}
}

// Defaults: approvals, input, turn ends and abnormal exits are pushed; a
// normal exit, a program's own notification and a session the app hung up
// are not (in-app only). notify.reasons changes that per reason.
func TestPushReasons(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	const s1, s2, s3, s4 = "00000000000000b1", "00000000000000b2", "00000000000000b3", "00000000000000b4"

	h1 := e.register(testSession(s1))
	e.hook(stop(s1))
	if r := wantPush(t, reqs, "turn_complete", s1, "Turn complete"); r.title != "box · claude · proj" {
		t.Fatalf("push titled %q", r.title)
	}
	h1.Notify(wire.MethodHolderNotify, wire.HolderNotifyParams{ID: s1, Title: "done"})
	await(t, events, "terminal notification in-app", notificationWith(reasonTerminal))
	noReq(t, reqs, "terminal", 300*time.Millisecond)
	h1.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: s1, ExitCode: 0})
	await(t, events, "normal exit in-app", notificationWith(reasonExited))
	wantClear(t, reqs, "s1 ended", s1) // its turn_complete is moot
	noReq(t, reqs, "normal exit", 300*time.Millisecond)

	h2 := e.register(testSession(s2))
	h2.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: s2, ExitCode: 2})
	wantPush(t, reqs, "abnormal exit", s2, "Exited with code 2")

	h3 := e.register(testSession(s3))
	h3.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: s3, ExitCode: 129, HungUp: true})
	m := await(t, events, "hung-up session ended", event(wire.EventSessionEnded, func(ev wire.Event) bool { return ev.SessionID == s3 }))
	if _, se := decodeEvent[wire.SessionEndedData](t, m); !se.HungUp || se.ExitCode != 129 {
		t.Fatalf("session_ended %+v", se)
	}
	m = await(t, events, "hung-up exit in-app", notificationWith(reasonExited))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Level != wire.LevelInfo || n.SessionID != s3 {
		t.Fatalf("hung-up exit notification %+v", n)
	}
	noReq(t, reqs, "exit after hangup", 300*time.Millisecond)

	call(t, watcher, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(
		`{"notify": {"reasons": {"turn_complete": false, "terminal": true, "exited": "all"}}}`)}, nil)
	h4 := e.register(testSession(s4))
	e.hook(stop(s4))
	noReq(t, reqs, "turn_complete off", 300*time.Millisecond)
	h4.Notify(wire.MethodHolderNotify, wire.HolderNotifyParams{ID: s4, Title: "secret program text"})
	wantPush(t, reqs, "terminal on", s4, "Terminal notification")
	h4.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: s4, ExitCode: 0})
	wantClear(t, reqs, "s4 ended", s4)
	wantPush(t, reqs, "exited all", s4, "Exited")
}

// A pushed notification is cleared once its session settles: the approval
// closed, a prompt was submitted, the session ended. A redraw (output
// activity) settles nothing a hook raised.
func TestPushClearedWhenSettled(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{})
	h := e.register(testSession(sid))

	hc, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"ls"}}`)})
	wantPush(t, reqs, "needs_approval", sid, "Needs approval: Bash")
	hc.Close() // answered on the PC
	<-reply
	wantClear(t, reqs, "approval answered", sid)

	e.hook(stop(sid))
	wantPush(t, reqs, "turn_complete", sid, "Turn complete")
	time.Sleep(1100 * time.Millisecond) // past the hook's override grace
	h.Notify(wire.MethodHolderUpdate, wire.SessionPatch{ID: sid, State: ptr(wire.StateWorking), StateSource: ptr(wire.SourceActivity)})
	noReq(t, reqs, "redraw", 400*time.Millisecond)
	e.hook(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "UserPromptSubmit", SessionID: sid})
	wantClear(t, reqs, "prompt submitted", sid)

	e.hook(stop(sid))
	wantPush(t, reqs, "turn_complete again", sid, "Turn complete")
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sid, ExitCode: 0})
	wantClear(t, reqs, "session ended", sid)
	noReq(t, reqs, "normal exit", 300*time.Millisecond)
}

// The needs_approval push is cleared once the holder reports claude's
// permission menu gone, while claude still holds the hook (the tool runs).
func TestPushClearedWhenPromptLeavesScreen(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{})
	h, holderMsgs := e.client()
	call(t, h, wire.MethodHolderRegister, testSession(sid), nil)
	_, reply := e.hookAsync(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "PermissionRequest", SessionID: sid,
		Payload: json.RawMessage(`{"tool_name":"Bash","tool_input":{"command":"sleep 25"}}`)})
	wantPush(t, reqs, "needs_approval", sid, "Needs approval: Bash")
	w := nextPromptWatch(t, holderMsgs, "prompt_watch")
	h.Notify(wire.MethodHolderPromptGone, wire.HolderPromptGoneParams{ID: sid, Gen: w.Gen})
	wantClear(t, reqs, "prompt answered", sid)
	answered(t, reply, "hook released")
}

// A passthrough session's push waits the grace and is dropped if the
// session settles meanwhile.
func TestPassthroughPushWaitsGrace(t *testing.T) {
	const grace = 400 * time.Millisecond
	e, reqs := pushEnv(t, nil, Options{PushGrace: grace})
	s := testSession(sid)
	s.Mode = wire.ModePassthrough
	e.register(s)

	e.hook(stop(sid))
	time.Sleep(grace / 2)
	e.hook(wire.HookEventParams{Harness: wire.HarnessClaude, Event: "UserPromptSubmit", SessionID: sid})
	noReq(t, reqs, "answered within the grace", 2*grace)

	start := time.Now()
	e.hook(stop(sid))
	if r := wantPush(t, reqs, "after the grace", sid, "Turn complete"); r.at.Sub(start) < grace {
		t.Fatalf("pushed after %v, before the %v grace", r.at.Sub(start), grace)
	}
}

// Someone present at the session (keystrokes, focus, the presence file)
// gets no push.
func TestPresentSessionIsNotPushed(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{})
	h := e.register(testSession(sid))
	patch := func(p wire.SessionPatch) {
		p.ID = sid
		h.Notify(wire.MethodHolderUpdate, p)
		time.Sleep(50 * time.Millisecond)
	}
	ago := func(d time.Duration) *int64 { return ptr(time.Now().Add(-d).UnixMilli()) }

	patch(wire.SessionPatch{LastLocalInputAt: ago(30 * time.Second)})
	e.hook(stop(sid))
	noReq(t, reqs, "typed 30 s ago", 300*time.Millisecond)

	patch(wire.SessionPatch{LastLocalInputAt: ago(5 * time.Minute), Focused: ptr(true)})
	e.hook(stop(sid))
	noReq(t, reqs, "focused, typed 5 min ago", 300*time.Millisecond)

	patch(wire.SessionPatch{Focused: ptr(false)})
	e.hook(stop(sid))
	wantPush(t, reqs, "unfocused, typed 5 min ago", sid, "Turn complete")
}

func TestPresentBoundaries(t *testing.T) {
	now := time.Now()
	file := filepath.Join(t.TempDir(), "presence")
	at := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	for _, c := range []struct {
		name    string
		s       wire.Session
		present bool
	}{
		{"never typed", wire.Session{}, false},
		{"typed 59 s ago", wire.Session{LastLocalInputAt: at(59 * time.Second)}, true},
		{"typed 61 s ago", wire.Session{LastLocalInputAt: at(61 * time.Second)}, false},
		{"focused, typed 9 min 59 s ago", wire.Session{LastLocalInputAt: at(10*time.Minute - time.Second), Focused: true}, true},
		{"focused, typed 10 min 1 s ago", wire.Session{LastLocalInputAt: at(10*time.Minute + time.Second), Focused: true}, false},
		{"focused, never typed", wire.Session{Focused: true}, false},
		{"presence file missing", wire.Session{PresenceFile: file}, false},
	} {
		if got := (&Daemon{}).presentLocked(&session{s: c.s}, now); got != c.present {
			t.Errorf("%s: present = %v", c.name, got)
		}
	}
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !(&Daemon{}).presentLocked(&session{s: wire.Session{PresenceFile: file, Mode: wire.ModeDetached}}, now) {
		t.Error("presence file present: not present")
	}
}

// While an app watching the host says it is in the foreground, nothing is
// pushed; a connection without a watch, or a closed one, does not count.
func TestForegroundAppStopsPushes(t *testing.T) {
	e, reqs := pushEnv(t, nil, Options{})
	e.register(testSession(sid))
	app, events := e.client()
	call(t, app, wire.MethodWatch, wire.WatchParams{}, nil)

	call(t, app, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	e.hook(stop(sid))
	await(t, events, "in-app notification", notificationWith(reasonTurnComplete))
	noReq(t, reqs, "app in front", 300*time.Millisecond)

	call(t, app, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: false}, nil)
	other, _ := e.client()
	call(t, other, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	e.hook(stop(sid))
	wantPush(t, reqs, "app in the background, no watch in front", sid, "Turn complete")

	call(t, app, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	app.Close()
	time.Sleep(50 * time.Millisecond)
	e.hook(stop(sid))
	wantPush(t, reqs, "app connection closed", sid, "Turn complete")
}

// skip_when_claude_app_notifies skips sessions whose hooks run connected to
// Remote Control, and only those.
func TestSkipWhenClaudeAppNotifies(t *testing.T) {
	e, reqs := pushEnv(t, func(c *wire.Config) { c.Notify.SkipWhenClaudeAppNotifies = true }, Options{})
	const rc, plain = "00000000000000c1", "00000000000000c2"
	e.register(testSession(rc))
	e.register(testSession(plain))

	p := stop(rc)
	p.RemoteControl = true
	e.hook(p)
	noReq(t, reqs, "connected to Remote Control", 300*time.Millisecond)
	e.hook(stop(plain))
	wantPush(t, reqs, "not connected", plain, "Turn complete")

	c, _ := e.client()
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {"skip_when_claude_app_notifies": false}}`)}, nil)
	e.hook(p)
	wantPush(t, reqs, "switch off", rc, "Turn complete")
}

// notify.lang writes stagent's phrases (in-app and pushed) in that
// language; config.set refuses unknown languages and exited values and
// config.get shows the defaults.
func TestNotifyLangAndReasonsConfig(t *testing.T) {
	e, reqs := pushEnv(t, func(c *wire.Config) { c.Notify.Lang = wire.LangJa }, Options{})
	watcher, events := e.client()
	call(t, watcher, wire.MethodWatch, wire.WatchParams{}, nil)
	e.register(testSession(sid))
	e.hook(stop(sid))
	m := await(t, events, "in-app notification", notificationWith(reasonTurnComplete))
	if _, n := decodeEvent[wire.NotificationData](t, m); n.Body != "ターン完了" || n.Reason != reasonTurnComplete {
		t.Fatalf("in-app notification %+v", n)
	}
	wantPush(t, reqs, "ja push", sid, "ターン完了")

	var res wire.ConfigResult
	call(t, watcher, wire.MethodConfigGet, nil, &res)
	r := res.Config.Notify.Reasons
	if *r.NeedsApproval != true || *r.WaitingInput != true || *r.TurnComplete != true || *r.Terminal != false ||
		r.Exited != wire.ExitedError || res.Config.Notify.Lang != wire.LangJa || res.Config.Notify.SkipWhenClaudeAppNotifies {
		t.Fatalf("effective notify %+v", res.Config.Notify)
	}
	for _, patch := range []string{
		`{"notify": {"lang": "fr"}}`,
		`{"notify": {"reasons": {"exited": "sometimes"}}}`,
	} {
		var we *wire.Error
		err := watcher.Call(t.Context(), wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(patch)}, nil)
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("config.set %s: %v, want bad_request", patch, err)
		}
	}
}
