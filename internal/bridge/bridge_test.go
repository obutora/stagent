package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

func TestMain(m *testing.M) { stubcmd.Main(m) }

const (
	idA = "aaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbb"
)

// app is the test's side of the bridge's stdin/stdout.
type app struct {
	*rpc.Client
	mu     sync.Mutex
	log    []string // "notify:<method>" / "resp:<tag>" in arrival order
	notifs chan *wire.Msg
}

type pipeRW struct {
	io.Reader
	io.WriteCloser
}

// startBridge runs a bridge over pipes in an isolated STAGENT_HOME. The
// daemon is dialed without auto-start so fake daemons can come and go.
func startBridge(t *testing.T, opts ...func(*Bridge)) (*paths.Layout, *app) {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	b := New(l, outW)
	b.dialDaemon = func() (net.Conn, error) { return ipc.Dial(l.DaemonAddr, 300*time.Millisecond) }
	// The test binary is not stagent; `<it> __env` would re-run the tests.
	b.loginEnv = func() ([]string, error) { return nil, errors.New("no login shell in tests") }
	for _, o := range opts {
		o(b)
	}
	served := make(chan error, 1)
	go func() { served <- b.Serve(context.Background(), inR) }()

	a := &app{notifs: make(chan *wire.Msg, 256)}
	a.Client = rpc.NewClient(pipeRW{outR, inW}, func(m *wire.Msg) {
		a.mu.Lock()
		a.log = append(a.log, "notify:"+m.Method)
		a.mu.Unlock()
		a.notifs <- m
	})
	t.Cleanup(func() {
		inW.Close()
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("bridge did not exit after stdin closed")
		}
		outR.Close()
	})
	return l, a
}

// call issues a request and returns its wire error (nil on success).
func (a *app) call(t *testing.T, method string, params, result any) *wire.Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := a.Call(ctx, method, params, result)
	if err == nil {
		return nil
	}
	var we *wire.Error
	if errors.As(err, &we) {
		return we
	}
	t.Fatalf("%s: %v", method, err)
	return nil
}

// goCall issues a request without waiting; the response lands on the
// returned channel and is logged as "resp:<tag>".
func (a *app) goCall(method, tag string, params any) <-chan *wire.Msg {
	ch := make(chan *wire.Msg, 1)
	p, _ := json.Marshal(params)
	a.Go(method, p, func(m *wire.Msg) {
		a.mu.Lock()
		a.log = append(a.log, "resp:"+tag)
		a.mu.Unlock()
		ch <- m
	})
	return ch
}

func (a *app) nextNotify(t *testing.T, method string) *wire.Msg {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m := <-a.notifs:
			if m.Method == method {
				return m
			}
		case <-timeout:
			t.Fatalf("no %s notification", method)
		}
	}
}

func wait(t *testing.T, ch <-chan *wire.Msg) *wire.Msg {
	t.Helper()
	select {
	case m := <-ch:
		if m == nil {
			t.Fatal("app connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no response")
		return nil
	}
}

// fakeServer is a stand-in daemon or holder on a real IPC address.
type fakeServer struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func serveFake(t *testing.T, addr string, h rpc.Handler) *fakeServer {
	t.Helper()
	ln, err := ipc.Listen(addr)
	if err != nil {
		t.Fatalf("listen %s: %v", addr, err)
	}
	s := &fakeServer{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, c)
			s.mu.Unlock()
			go rpc.Serve(context.Background(), c, h)
		}
	}()
	t.Cleanup(s.stop)
	return s
}

// stop simulates the process dying: no more accepts, connections dropped.
func (s *fakeServer) stop() {
	s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		c.Close()
	}
	s.conns = nil
}

func TestHelloAnswersWithServerProtocolOnMismatch(t *testing.T) {
	_, a := startBridge(t)
	var res wire.HelloResult
	if e := a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol + 7, Client: "test"}, &res); e != nil {
		t.Fatal(e)
	}
	if res.Protocol != version.Protocol || res.Version != version.Version {
		t.Fatalf("hello = %+v", res)
	}
}

func TestSlowTargetsDoNotStallOthersAndEachTargetIsFIFO(t *testing.T) {
	l, a := startBridge(t)

	releaseA := make(chan struct{})
	var mu sync.Mutex
	var gotA []string
	serveFake(t, l.HolderAddr(idA), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		var p wire.InputParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		mu.Lock()
		gotA = append(gotA, p.Text)
		mu.Unlock()
		if p.Text != "slow" {
			return struct{}{}, nil
		}
		// Answer later without blocking this connection, so a bridge
		// that pipelined requests would deliver "second" meanwhile.
		go func() {
			<-releaseA
			c.Reply(m.ID, struct{}{})
		}()
		return rpc.Async, nil
	})
	serveFake(t, l.HolderAddr(idB), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return struct{}{}, nil
	})
	releaseD := make(chan struct{})
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		go func() {
			<-releaseD
			c.Reply(m.ID, wire.ConversationsListResult{})
		}()
		return rpc.Async, nil
	})

	slow := a.goCall(wire.MethodSessionInput, "slow", wire.InputParams{ID: idA, Text: "slow"})
	second := a.goCall(wire.MethodSessionInput, "second", wire.InputParams{ID: idA, Text: "second"})
	convs := a.goCall(wire.MethodConversationsList, "convs", wire.ConversationsListParams{})

	if e := a.call(t, wire.MethodSessionInput, wire.InputParams{ID: idB, Text: "b"}, nil); e != nil {
		t.Fatalf("input to B while A and the daemon are busy: %v", e)
	}
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(gotA) != 1 {
		t.Fatalf("holder A received %q before its first input was answered", gotA)
	}
	mu.Unlock()

	close(releaseA)
	if m := wait(t, slow); m.Error != nil {
		t.Fatal(m.Error)
	}
	if m := wait(t, second); m.Error != nil {
		t.Fatal(m.Error)
	}
	mu.Lock()
	if len(gotA) != 2 || gotA[0] != "slow" || gotA[1] != "second" {
		t.Fatalf("holder A order = %q", gotA)
	}
	mu.Unlock()

	close(releaseD)
	if m := wait(t, convs); m.Error != nil {
		t.Fatal(m.Error)
	}
}

func TestHolderRouting(t *testing.T) {
	l, a := startBridge(t)

	if e := a.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: idA}, nil); e == nil || e.Code != wire.ErrNotFound {
		t.Fatalf("info for a session without holder: %v", e)
	}
	if e := a.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: "../../stagent"}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("info for a malformed id: %v", e)
	}

	stuck := make(chan struct{})
	defer close(stuck)
	h := func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodSessionSignal {
			<-stuck
		}
		return wire.Session{ID: idA, State: wire.StateIdle}, nil
	}
	srv := serveFake(t, l.HolderAddr(idA), h)
	var s wire.Session
	if e := a.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: idA}, &s); e != nil || s.ID != idA {
		t.Fatalf("info = %+v, %v", s, e)
	}

	// The holder dies while a request is in flight.
	inflight := a.goCall(wire.MethodSessionSignal, "signal", wire.SignalParams{ID: idA, Signal: wire.SignalInterrupt})
	time.Sleep(50 * time.Millisecond)
	srv.stop()
	if m := wait(t, inflight); m.Error == nil || m.Error.Code != wire.ErrNotFound {
		t.Fatalf("in-flight request to a dead holder: %+v", m)
	}
	if e := a.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: idA}, nil); e == nil || e.Code != wire.ErrNotFound {
		t.Fatalf("info after the holder died: %v", e)
	}

	// A holder serving the id again is reached through a fresh connection.
	serveFake(t, l.HolderAddr(idA), h)
	if e := a.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: idA}, &s); e != nil {
		t.Fatalf("info after the holder came back: %v", e)
	}
}

func TestAttachResultPrecedesStreamedOutput(t *testing.T) {
	l, a := startBridge(t)
	payload := []byte("\x1b[2J\x1b[Hsnapshot \xff\x00")
	serveFake(t, l.HolderAddr(idA), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method != wire.MethodSessionAttach {
			return struct{}{}, nil
		}
		c.Reply(m.ID, wire.AttachResult{Cols: 80, Rows: 24, Mode: wire.AttachRaw})
		c.Notify(wire.NotifyOutput, wire.OutputParams{ID: idA, Data: payload, Reset: true})
		return rpc.Async, nil
	})
	const rounds = 30
	for i := 0; i < rounds; i++ {
		wait(t, a.goCall(wire.MethodSessionAttach, "attach", wire.AttachParams{ID: idA, Mode: wire.AttachRaw}))
		m := a.nextNotify(t, wire.NotifyOutput)
		var out wire.OutputParams
		if err := json.Unmarshal(m.Params, &out); err != nil || out.ID != idA || !out.Reset || string(out.Data) != string(payload) {
			t.Fatalf("relayed output = %s (%v)", m.Params, err)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i+1 < len(a.log); i += 2 {
		if a.log[i] != "resp:attach" || a.log[i+1] != "notify:output" {
			t.Fatalf("message order %q", a.log)
		}
	}
}

func TestDaemonDropFailsInFlightAndReconnectsLazily(t *testing.T) {
	l, a := startBridge(t)
	srv := serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return rpc.Async, nil // never answers
	})
	inflight := a.goCall(wire.MethodSessionsList, "list", nil)
	time.Sleep(50 * time.Millisecond)
	srv.stop()
	if m := wait(t, inflight); m.Error == nil || m.Error.Code != wire.ErrUnavailable {
		t.Fatalf("in-flight daemon request after drop: %+v", m)
	}
	if e := a.call(t, wire.MethodSessionsList, nil, nil); e == nil || e.Code != wire.ErrUnavailable {
		t.Fatalf("daemon request with no daemon: %v", e)
	}

	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return wire.SessionsListResult{Sessions: []wire.Session{{ID: idA}}}, nil
	})
	var res wire.SessionsListResult
	if e := a.call(t, wire.MethodSessionsList, nil, &res); e != nil || len(res.Sessions) != 1 {
		t.Fatalf("after the daemon came back: %+v, %v", res, e)
	}
}

// The watch, and the app's foreground (presence.set) with it, come back on
// the restarted daemon.
func TestWatchIsRestoredAfterDaemonRestart(t *testing.T) {
	l, a := startBridge(t)
	first := serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method != wire.MethodWatch {
			return struct{}{}, nil
		}
		c.Reply(m.ID, wire.WatchResult{Sessions: []wire.Session{{ID: idA}, {ID: idB}}, Seq: 5,
			Unwrapped: []wire.UnwrappedLaunch{{ConversationID: "conv", Harness: wire.HarnessClaude, Reason: wire.UnwrappedOldTerminal}}})
		c.Notify(wire.NotifyEvent, wire.Event{Seq: 6, Kind: wire.EventNotification})
		c.Notify(wire.NotifySessionUpdated, wire.Session{ID: "cccccccccccccccc"})
		return rpc.Async, nil
	})
	var res wire.WatchResult
	if e := a.call(t, wire.MethodWatch, wire.WatchParams{}, &res); e != nil || len(res.Sessions) != 2 {
		t.Fatalf("watch = %+v, %v", res, e)
	}
	a.nextNotify(t, wire.NotifyEvent)
	a.nextNotify(t, wire.NotifySessionUpdated)
	if e := a.call(t, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil); e != nil {
		t.Fatal(e)
	}

	since := make(chan int64, 1)
	foreground := make(chan bool, 1)
	first.stop()
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodPresenceSet {
			var p wire.PresenceSetParams
			rpc.Decode(m, &p)
			foreground <- p.Foreground
		}
		if m.Method != wire.MethodWatch {
			return struct{}{}, nil
		}
		var p wire.WatchParams
		rpc.Decode(m, &p)
		since <- p.Since
		return wire.WatchResult{
			Sessions: []wire.Session{{ID: idA}, {ID: "cccccccccccccccc"}},
			Missed:   []wire.Event{{Seq: 7, Kind: wire.EventSessionEnded, SessionID: idB}},
			Seq:      7,
		}, nil
	})

	select {
	case s := <-since:
		if s != 6 {
			t.Fatalf("resubscribed with since=%d, want 6", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not resubscribe")
	}
	var ev wire.Event
	json.Unmarshal(a.nextNotify(t, wire.NotifyEvent).Params, &ev)
	if ev.Seq != 7 {
		t.Fatalf("replayed event = %+v", ev)
	}
	updated := map[string]bool{}
	for range 2 {
		var s wire.Session
		json.Unmarshal(a.nextNotify(t, wire.NotifySessionUpdated).Params, &s)
		updated[s.ID] = true
	}
	if !updated[idA] || !updated["cccccccccccccccc"] {
		t.Fatalf("updated after resubscribe: %v", updated)
	}
	var gone wire.SessionRef
	json.Unmarshal(a.nextNotify(t, wire.NotifySessionRemoved).Params, &gone)
	if gone.ID != idB {
		t.Fatalf("removed = %q, want %q", gone.ID, idB)
	}
	// The restarted daemon forgot the agents started without stagent: the
	// app's list is replaced by an empty one.
	if p := a.nextNotify(t, wire.NotifyUnwrappedUpdated).Params; string(p) != `{"unwrapped":[]}` {
		t.Fatalf("unwrapped.updated after resubscribe = %s", p)
	}
	select {
	case fg := <-foreground:
		if !fg {
			t.Fatal("restored presence.set foreground false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("foreground not restored with the watch")
	}
}

func TestSpawnRejectsInvalidRequests(t *testing.T) {
	_, a := startBridge(t)
	for _, p := range []wire.SpawnParams{
		{Cols: 80, Rows: 24},
		{Command: []string{"sh"}, Cwd: "/nonexistent/stagent-test", Cols: 80, Rows: 24},
		{Command: []string{"sh"}, Env: map[string]string{"A=B": "c"}},
		{Shell: true, Command: []string{"sh"}},
	} {
		if e := a.call(t, wire.MethodSessionSpawn, p, nil); e == nil || e.Code != wire.ErrBadRequest {
			t.Errorf("spawn %+v: %v", p, e)
		}
	}
}

func TestLostWatchIsRestoredByTheNextDaemonRequest(t *testing.T) {
	// No background retries: the daemon stays away longer than the bridge
	// waits for it.
	l, a := startBridge(t, func(b *Bridge) { b.resubscribeFor = 0 })
	first := serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return wire.WatchResult{Sessions: []wire.Session{{ID: idA}}, Seq: 3}, nil
	})
	if e := a.call(t, wire.MethodWatch, wire.WatchParams{}, nil); e != nil {
		t.Fatal(e)
	}
	first.stop()
	time.Sleep(100 * time.Millisecond)

	var mu sync.Mutex
	var calls []string
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		mu.Lock()
		calls = append(calls, m.Method)
		mu.Unlock()
		if m.Method == wire.MethodWatch {
			return wire.WatchResult{Sessions: []wire.Session{{ID: idB}}, Seq: 4}, nil
		}
		return wire.ApprovalsListResult{}, nil
	})
	if e := a.call(t, wire.MethodApprovalsList, nil, nil); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	if len(calls) != 2 || calls[0] != wire.MethodWatch || calls[1] != wire.MethodApprovalsList {
		t.Fatalf("daemon saw %q, want the watch restored before the request", calls)
	}
	mu.Unlock()
	var s wire.Session
	json.Unmarshal(a.nextNotify(t, wire.NotifySessionUpdated).Params, &s)
	var gone wire.SessionRef
	json.Unmarshal(a.nextNotify(t, wire.NotifySessionRemoved).Params, &gone)
	if s.ID != idB || gone.ID != idA {
		t.Fatalf("after restore: updated %q, removed %q", s.ID, gone.ID)
	}
}
