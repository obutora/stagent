package hostscreen

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// fakeStream records what the server asks of it; its Events are driven by
// the test.
type fakeStream struct {
	mu     sync.Mutex
	ev     Events
	calls  []string
	closed bool
}

func (s *fakeStream) Start(ev Events) { s.mu.Lock(); s.ev = ev; s.mu.Unlock(); s.note("start") }
func (s *fakeStream) Pause()          { s.note("pause") }
func (s *fakeStream) Resume()         { s.note("resume") }
func (s *fakeStream) Close()          { s.note("close"); s.mu.Lock(); s.closed = true; s.mu.Unlock() }
func (s *fakeStream) note(c string)   { s.mu.Lock(); s.calls = append(s.calls, c); s.mu.Unlock() }
func (s *fakeStream) frame(data string) {
	s.mu.Lock()
	ev := s.ev
	s.mu.Unlock()
	ev.Frame(Frame{Data: data, Width: 800, Height: 600})
}
func (s *fakeStream) callList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// inputStream is a fakeStream that takes inputs; while err is set, each
// input it is handed takes wait, then fails with it.
type inputStream struct {
	*fakeStream
	inputs []wire.HostScreenInput
	err    error
	wait   time.Duration
}

func (s *inputStream) Input(_ context.Context, in wire.HostScreenInput) error {
	s.inputs = append(s.inputs, in)
	if s.err != nil {
		time.Sleep(s.wait)
	}
	return s.err
}

type fakeKind struct {
	streams []*fakeStream
	err     error
	// input: streams take inputs, the last one's are in inputs.
	input  bool
	inputs *inputStream
}

func (k *fakeKind) Name() string                      { return wire.HostScreenBrowser }
func (k *fakeKind) List(context.Context) (any, error) { return "section", nil }
func (k *fakeKind) Open(context.Context, wire.HostScreenTarget) (Stream, error) {
	if k.err != nil {
		return nil, k.err
	}
	s := &fakeStream{}
	k.streams = append(k.streams, s)
	if k.input {
		k.inputs = &inputStream{fakeStream: s}
		return k.inputs, nil
	}
	return s, nil
}

// sent is what the server wrote to the app.
type sent struct {
	mu   sync.Mutex
	msgs []string // "frame:<seq>:<data>", "state:<state>", "reply"
}

func (o *sent) notify(method string, params any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch p := params.(type) {
	case wire.HostScreenFrame:
		b, _ := json.Marshal(p.Seq)
		o.msgs = append(o.msgs, "frame:"+string(b)+":"+p.Data)
	case wire.HostScreenState:
		o.msgs = append(o.msgs, "state:"+p.State)
	default:
		o.msgs = append(o.msgs, method)
	}
}

func (o *sent) take() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := o.msgs
	o.msgs = nil
	return m
}

// fakeTimer replaces Server.afterFunc.
type fakeTimer struct {
	mu sync.Mutex
	f  func()
}

func (t *fakeTimer) after(_ time.Duration, f func()) func() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.f = f
	return func() bool {
		t.mu.Lock()
		defer t.mu.Unlock()
		armed := t.f != nil
		t.f = nil
		return armed
	}
}

func (t *fakeTimer) fire() bool {
	t.mu.Lock()
	f := t.f
	t.f = nil
	t.mu.Unlock()
	if f != nil {
		f()
	}
	return f != nil
}

func newTestServer(t *testing.T) (*Server, *fakeKind, *sent, *fakeTimer) {
	t.Helper()
	k := &fakeKind{}
	out := &sent{}
	s := NewServer(out.notify, k)
	tm := &fakeTimer{}
	s.afterFunc = tm.after
	return s, k, out, tm
}

func start(t *testing.T, s *Server, out *sent) {
	t.Helper()
	err := s.Start(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "T"}, func() {
		out.notify("reply", nil)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAtMostTwoFramesInFlightAndTheNewestIsHeld(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.frame("a")
	st.frame("b")
	st.frame("c") // held
	st.frame("d") // replaces c
	if got, want := out.take(), []string{"reply", "frame:1:a", "frame:2:b"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if err := s.Ack(1); err != nil {
		t.Fatal(err)
	}
	if got, want := out.take(), []string{"frame:3:d"}; !slices.Equal(got, want) {
		t.Fatalf("after the ack: %q, want %q (the held frame at once)", got, want)
	}
	// Repeated and unknown acks free nothing.
	s.Ack(1)
	s.Ack(42)
	st.frame("e")
	if got := out.take(); len(got) != 0 {
		t.Fatalf("sent %q with two frames still in flight", got)
	}
	s.Ack(2)
	s.Ack(3)
	if got, want := out.take(), []string{"frame:4:e"}; !slices.Equal(got, want) {
		t.Fatalf("after two acks: %q, want %q", got, want)
	}
	st.frame("f")
	if got, want := out.take(), []string{"frame:5:f"}; !slices.Equal(got, want) {
		t.Fatalf("with a place free: %q, want %q", got, want)
	}
}

func TestStalledAppPausesAndItsAckResumes(t *testing.T) {
	s, k, out, tm := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.frame("a")
	st.frame("b")
	if !tm.fire() {
		t.Fatal("no stall timer with two frames in flight")
	}
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	s.Ack(1)
	if got, want := st.callList(), []string{"start", "pause", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls after the ack %q, want %q", got, want)
	}
	// An ack in time disarms the timer.
	st.frame("c") // seq 3, in flight with 2
	s.Ack(2)
	if tm.fire() {
		t.Fatal("stall timer still armed after an ack")
	}
}

func TestGoneEndsFramesButAcksStillAnswer(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	st.frame("a")
	st.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
	st.frame("b")
	if got, want := out.take(), []string{"reply", "state:live", "frame:1:a", "state:gone"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if err := s.Ack(1); err != nil {
		t.Fatalf("ack after gone: %v", err)
	}
}

func TestStartReplacesTheStreamAndStopEndsIt(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	var we *wire.Error
	if err := s.Ack(1); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("ack without a stream: %v, want bad_request", err)
	}
	start(t, s, out)
	start(t, s, out)
	if !k.streams[0].closed || k.streams[1].closed {
		t.Fatal("a second start must close the first stream only")
	}
	k.streams[0].ev.Frame(Frame{Data: "old"})
	k.streams[1].frame("new")
	if got, want := out.take(), []string{"reply", "reply", "frame:1:new"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	s.Stop()
	if !k.streams[1].closed {
		t.Fatal("stop left the stream open")
	}
	if err := s.Ack(1); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("ack after stop: %v, want bad_request", err)
	}
	s.Stop() // nothing to stop

	k.err = wire.Errorf(wire.ErrNotFound, "no page")
	if err := s.Start(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser}, func() { t.Fatal("answered a failed start") }); !errors.As(err, &we) || we.Code != wire.ErrNotFound {
		t.Fatalf("start of a missing target: %v", err)
	}
	for kind, code := range map[string]string{wire.HostScreenAndroid: wire.ErrUnsupported, "tv": wire.ErrBadRequest} {
		if err := s.Start(t.Context(), wire.HostScreenTarget{Kind: kind}, func() {}); !errors.As(err, &we) || we.Code != code {
			t.Errorf("start of kind %q: %v, want %s", kind, err, code)
		}
	}
}

// portsKind records the ports its section is asked with.
type portsKind struct {
	fakeKind
	ports []int
}

func (k *portsKind) ListPorts(_ context.Context, ports []int) (any, error) {
	k.ports = ports
	return "section", nil
}

func TestListHandsCDPPortsToTheBrowserKindAndRefusesOutOfRange(t *testing.T) {
	k := &portsKind{}
	s := NewServer(func(string, any) {}, k)
	res, err := s.List(t.Context(), wire.HostScreenListParams{CDPPorts: []int{9333, 1, 9333, 65535}})
	if err != nil {
		t.Fatal(err)
	}
	if res[wire.HostScreenBrowser] != "section" || !slices.Equal(k.ports, []int{9333, 1, 65535}) {
		t.Fatalf("list = %v with ports %v", res, k.ports)
	}
	for _, port := range []int{0, -1, 65536} {
		_, err := s.List(t.Context(), wire.HostScreenListParams{CDPPorts: []int{9333, port}})
		var we *wire.Error
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("cdp_ports %d: err = %v, want bad_request", port, err)
		}
	}
}

func TestInputGoesToTheStreamOrIsRefused(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	var we *wire.Error
	tap := wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}
	if err := s.Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("input without a stream: %v, want bad_request", err)
	}
	start(t, s, out)
	if err := s.Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrUnsupported {
		t.Fatalf("input to a stream without inputs: %v, want unsupported", err)
	}
	k.input = true
	start(t, s, out)
	text := wire.HostScreenInput{Type: wire.InputText, Text: "a"}
	for _, in := range []wire.HostScreenInput{tap, text} {
		if err := s.Input(t.Context(), in); err != nil {
			t.Fatal(err)
		}
	}
	if got := k.inputs.inputs; len(got) != 2 || got[0].Type != wire.InputTap || got[1].Type != wire.InputText {
		t.Fatalf("stream got %+v, want the tap then the text", got)
	}
	k.inputs.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
	if err := s.Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("input after gone: %v, want bad_request", err)
	}
	s.Stop()
	if err := s.Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("input after stop: %v, want bad_request", err)
	}
}

func TestInputsBehindOneTheTargetDidNotAnswerAreRefusedAtOnce(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	k.input = true
	start(t, s, out)
	var we *wire.Error
	tap := wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}
	// The page is held by alert(): the input waits out its timeout.
	k.inputs.err = wire.Errorf(wire.ErrUnavailable, "Input.dispatchMouseEvent: no answer")
	k.inputs.wait = 200 * time.Millisecond
	began := time.Now()
	for range 5 {
		if err := s.Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrUnavailable {
			t.Fatalf("input to a held page: %v, want unavailable", err)
		}
	}
	if n := len(k.inputs.inputs); n != 1 {
		t.Fatalf("the held page was handed %d inputs, want 1: the rest must not wait on it", n)
	}
	if d := time.Since(began); d > 600*time.Millisecond {
		t.Fatalf("5 inputs to a held page took %v; one timeout is 200ms", d)
	}
	// A frame says the page draws again.
	k.inputs.err = nil
	k.inputs.frame("again")
	if err := s.Input(t.Context(), tap); err != nil || len(k.inputs.inputs) != 2 {
		t.Fatalf("input after a frame: %v, handed %d", err, len(k.inputs.inputs))
	}
	// So does a new start.
	k.inputs.err = wire.Errorf(wire.ErrUnavailable, "no answer")
	s.Input(t.Context(), tap)
	k.inputs.err = nil
	start(t, s, out)
	if err := s.Input(t.Context(), tap); err != nil || len(k.inputs.inputs) != 1 {
		t.Fatalf("input to the new stream: %v, handed %d", err, len(k.inputs.inputs))
	}
}

func TestCheckInputRefusesShapesWrongForTheirType(t *testing.T) {
	pts := func(n int) []wire.HostScreenPoint { return make([]wire.HostScreenPoint, n) }
	for _, tc := range []struct {
		in wire.HostScreenInput
		ok bool
	}{
		{wire.HostScreenInput{Type: wire.InputTap, X: 0, Y: 1}, true},
		{wire.HostScreenInput{Type: wire.InputTap, X: 1.01, Y: 0.5}, false},
		{wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: -0.1}, false},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: pts(2), DurationMS: 300}, true},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: pts(1)}, false},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: pts(MaxDragPoints + 1)}, false},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 2, Y: 0}}}, false},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: pts(2), DurationMS: MaxDragDuration + 1}, false},
		{wire.HostScreenInput{Type: wire.InputDrag, Points: pts(2), DurationMS: -1}, false},
		{wire.HostScreenInput{Type: wire.InputScroll, X: 0.5, Y: 0.5, DX: -3, DY: 0.2}, true},
		{wire.HostScreenInput{Type: wire.InputScroll, X: 0.5, Y: 0.5, DY: MaxScroll + 1}, false},
		{wire.HostScreenInput{Type: wire.InputText, Text: "日本語"}, true},
		{wire.HostScreenInput{Type: wire.InputText, Enter: true}, true},
		{wire.HostScreenInput{Type: wire.InputText}, false},
		{wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyRecents}, true},
		{wire.HostScreenInput{Type: wire.InputKey, Key: "power"}, false},
		{wire.HostScreenInput{Type: wire.InputNavigate, Action: wire.NavigateForward}, true},
		{wire.HostScreenInput{Type: wire.InputNavigate, Action: wire.NavigateURL, URL: "https://example.com/"}, true},
		{wire.HostScreenInput{Type: wire.InputNavigate, Action: wire.NavigateURL}, false},
		{wire.HostScreenInput{Type: wire.InputNavigate, Action: "stop"}, false},
		{wire.HostScreenInput{Type: "pinch"}, false},
	} {
		err := CheckInput(tc.in)
		var we *wire.Error
		if tc.ok && err != nil || !tc.ok && (!errors.As(err, &we) || we.Code != wire.ErrBadRequest) {
			t.Errorf("CheckInput(%+v) = %v, want ok %v", tc.in, err, tc.ok)
		}
	}
}
