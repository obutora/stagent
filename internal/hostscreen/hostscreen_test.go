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
	// clk, when set, is the server's clock: frame moves it on FrameEvery.
	clk *fakeClock
}

func (s *fakeStream) Start(ev Events) { s.mu.Lock(); s.ev = ev; s.mu.Unlock(); s.note("start") }
func (s *fakeStream) Pause()          { s.note("pause") }
func (s *fakeStream) Resume()         { s.note("resume") }
func (s *fakeStream) Close()          { s.note("close"); s.mu.Lock(); s.closed = true; s.mu.Unlock() }
func (s *fakeStream) note(c string)   { s.mu.Lock(); s.calls = append(s.calls, c); s.mu.Unlock() }

// frame is a frame captured FrameEvery after the previous one (on the
// server's clock, when the stream has it).
func (s *fakeStream) frame(data string) {
	if s.clk != nil {
		s.clk.advance(FrameEvery)
	}
	s.frameNow(data)
}

// frameNow is a frame captured now.
func (s *fakeStream) frameNow(data string) {
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
	clk     *fakeClock
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
	s := &fakeStream{clk: k.clk}
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

// fakeClock replaces Server.now and Server.afterFunc: time moves only
// when the test moves it, and the timers due then run.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at time.Time
	d  time.Duration
	f  func()
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) after(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	tm := &fakeTimer{at: c.now.Add(d), d: d, f: f}
	c.timers = append(c.timers, tm)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		i := slices.Index(c.timers, tm)
		if i >= 0 {
			c.timers = slices.Delete(c.timers, i, i+1)
		}
		return i >= 0
	}
}

// advance moves the clock on d, running each timer due on the way at its
// time.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	end := c.now.Add(d)
	c.mu.Unlock()
	c.runUntil(end)
}

func (c *fakeClock) runUntil(end time.Time) {
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, tm := range c.timers {
			if !tm.at.After(end) && (next == nil || tm.at.Before(next.at)) {
				next = tm
			}
		}
		if next == nil {
			if end.After(c.now) {
				c.now = end
			}
			c.mu.Unlock()
			return
		}
		c.timers = slices.DeleteFunc(c.timers, func(tm *fakeTimer) bool { return tm == next })
		if next.at.After(c.now) {
			c.now = next.at
		}
		c.mu.Unlock()
		next.f()
	}
}

// fire moves the clock to the stall timer's time, when one is armed, and
// tells whether one was.
func (c *fakeClock) fire() bool {
	c.mu.Lock()
	var stall *fakeTimer
	for _, tm := range c.timers {
		if tm.d == StallAfter {
			stall = tm
		}
	}
	c.mu.Unlock()
	if stall == nil {
		return false
	}
	c.runUntil(stall.at)
	return true
}

func newTestServer(t *testing.T) (*Server, *fakeKind, *sent, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	k := &fakeKind{clk: clk}
	out := &sent{}
	s := NewServer(out.notify, k)
	s.now, s.afterFunc = clk.Now, clk.after
	return s, k, out, clk
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

func TestLiveAfterGoneStartsTheFramesOver(t *testing.T) {
	s, k, out, tm := newTestServer(t)
	k.input = true
	start(t, s, out)
	st := k.inputs
	st.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	st.frame("a")
	st.frame("b")
	tm.fire() // the app holds both: paused
	st.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
	st.frame("lost")
	// The emulator started again: the frames in flight before it went are
	// no longer waited for, and inputs go again.
	st.ev.State(wire.HostScreenState{State: wire.HostScreenLive, Title: "Pixel"})
	st.frame("c")
	if got, want := out.take(), []string{"reply", "state:live", "frame:1:a", "frame:2:b", "state:gone", "state:live", "frame:3:c"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}); err != nil {
		t.Fatalf("input after live again: %v", err)
	}
	// An ack of a frame from before gone resumes nothing.
	s.Ack(1)
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
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

func TestPauseStopsTheSourceAndResumeSendsAFrameAtOnce(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.frame("a")
	st.frame("b")
	st.frame("c") // held: two in flight
	if err := s.Pause(); err != nil {
		t.Fatal(err)
	}
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	// What the source still sends is dropped, as is the held frame; acks
	// are taken.
	st.frame("late")
	for _, seq := range []int64{1, 2} {
		if err := s.Ack(seq); err != nil {
			t.Fatalf("ack while paused: %v", err)
		}
	}
	if got, want := out.take(), []string{"reply", "frame:1:a", "frame:2:b"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if got, want := st.callList(), []string{"start", "pause", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	st.frame("full")
	st.frame("next")
	if got, want := out.take(), []string{"frame:3:full", "frame:4:next"}; !slices.Equal(got, want) {
		t.Fatalf("after resume %q, want %q", got, want)
	}
}

func TestPauseAndResumeNeedAStreamAndRepeatsDoNothing(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	var we *wire.Error
	if err := s.Pause(); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("pause without a stream: %v, want bad_request", err)
	}
	if err := s.Resume(); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("resume without a stream: %v, want bad_request", err)
	}
	start(t, s, out)
	st := k.streams[0]
	for range 2 {
		if err := s.Resume(); err != nil {
			t.Fatalf("resume of a flowing stream: %v", err)
		}
	}
	for range 2 {
		if err := s.Pause(); err != nil {
			t.Fatalf("pause: %v", err)
		}
	}
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	s.Stop()
	if err := s.Pause(); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("pause after stop: %v, want bad_request", err)
	}
	if err := s.Resume(); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("resume after stop: %v, want bad_request", err)
	}
}

// A stall and a pause both hold the source: it captures again only once
// neither does.
func TestAStallAndAPauseResumeTheSourceOnlyWhenBothEnded(t *testing.T) {
	s, k, out, clk := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.frame("a")
	st.frame("b")
	s.Pause()
	clk.fire() // stalled while paused: the source is already held
	s.Ack(1)   // the stall ends, the pause holds
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	s.Resume()
	if got, want := st.callList(), []string{"start", "pause", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}

	// Stalled first: resume leaves it to the ack.
	st.frame("c") // seq 3, in flight with 2
	clk.fire()
	s.Pause()
	s.Resume()
	if got, want := st.callList(), []string{"start", "pause", "resume", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	s.Ack(2)
	if got, want := st.callList(), []string{"start", "pause", "resume", "pause", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls after the ack %q, want %q", got, want)
	}
}

// A target that comes back while the app paused starts paused too.
func TestLiveAfterGoneWhilePausedPausesTheNewStream(t *testing.T) {
	s, k, out, _ := newTestServer(t)
	start(t, s, out)
	st := k.streams[0]
	st.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
	s.Pause()
	st.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	st.frame("a")
	if got, want := out.take(), []string{"reply", "state:gone", "state:live"}; !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}
	if got, want := st.callList(), []string{"start", "pause"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
	s.Resume()
	st.frame("b")
	if got, want := out.take(), []string{"frame:1:b"}; !slices.Equal(got, want) {
		t.Fatalf("after resume %q, want %q", got, want)
	}
}

// A source at 30 fps is sent at 10: a frame captured within FrameEvery of
// the last one sent waits in the newest-one place.
func TestFramesAreCappedAtTenASecondAndTheNewestIsSent(t *testing.T) {
	clk := newFakeClock()
	k := &fakeKind{clk: clk}
	var mu sync.Mutex
	latest := ""
	var sentData []string
	var seqs []int64
	var stale []string
	s := NewServer(func(method string, params any) {
		fr, ok := params.(wire.HostScreenFrame)
		if !ok {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if fr.Data != latest {
			stale = append(stale, fr.Data)
		}
		sentData = append(sentData, fr.Data)
		seqs = append(seqs, fr.Seq)
	}, k)
	s.now, s.afterFunc = clk.Now, clk.after
	start(t, s, &sent{})
	st := k.streams[0]
	began := clk.Now()
	for i := range 30 {
		data := string(rune('A' + i))
		mu.Lock()
		latest = data
		mu.Unlock()
		st.frameNow(data)
		// The app acks at once.
		mu.Lock()
		acks := seqs
		seqs = nil
		mu.Unlock()
		for _, seq := range acks {
			s.Ack(seq)
		}
		clk.advance(time.Second / 30)
	}
	mu.Lock()
	n, gotStale := len(sentData), stale
	mu.Unlock()
	if d := clk.Now().Sub(began); d > time.Second+time.Millisecond {
		t.Fatalf("test ran %v of fake time", d)
	}
	if n > 10 || n < 9 {
		t.Fatalf("sent %d frames in a second of a 30 fps source, want at most 10 (and not far below)", n)
	}
	if len(gotStale) != 0 {
		t.Fatalf("sent %q while a newer frame was captured", gotStale)
	}
	// The last frame captured is not lost.
	clk.advance(FrameEvery)
	mu.Lock()
	last := sentData[len(sentData)-1]
	mu.Unlock()
	if last != string(rune('A'+29)) {
		t.Fatalf("last sent %q, want the newest %q", last, string(rune('A'+29)))
	}
}
