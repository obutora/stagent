package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// screenKind lists no browser and opens streams that go live and send one
// frame; Open waits for the gate while one is set. The last stream opened
// is stream.
type screenKind struct {
	gate   atomic.Pointer[chan struct{}]
	stream atomic.Pointer[screenStream]
}

func (k *screenKind) Name() string { return wire.HostScreenBrowser }
func (k *screenKind) List(context.Context) (any, error) {
	return wire.HostScreenSection[wire.HostScreenBrowserItem]{Items: []wire.HostScreenBrowserItem{}}, nil
}
func (k *screenKind) Open(ctx context.Context, t wire.HostScreenTarget) (hostscreen.Stream, error) {
	if g := k.gate.Load(); g != nil {
		<-*g
	}
	st := &screenStream{}
	k.stream.Store(st)
	return st, nil
}

// screenStream records its inputs' texts in order, and its pauses and
// resumes; an input waits for the input gate while one is set.
type screenStream struct {
	ev        hostscreen.Events
	inputGate atomic.Pointer[chan struct{}]
	mu        sync.Mutex
	inputs    []string
	calls     []string
}

func (s *screenStream) Start(ev hostscreen.Events) {
	s.ev = ev
	ev.State(wire.HostScreenState{State: wire.HostScreenLive, Title: "t"})
	ev.Frame(hostscreen.Frame{JPEG: []byte("jpg"), Width: 2, Height: 1})
}
func (s *screenStream) Pause()  { s.note("pause") }
func (s *screenStream) Resume() { s.note("resume") }
func (s *screenStream) note(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}
func (*screenStream) Close() {}
func (s *screenStream) Input(_ context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	if g := s.inputGate.Load(); g != nil {
		<-*g
	}
	s.mu.Lock()
	s.inputs = append(s.inputs, in.Text)
	s.mu.Unlock()
	return wire.HostScreenInputResult{Via: wire.InputViaKeys, App: in.Text}, nil
}

func withScreen(k *screenKind) func(*Bridge) {
	return func(b *Bridge) {
		b.hostScreen = hostscreen.NewServer(func(method string, params any) { b.out.Notify(method, params) }, k)
		// host_screen.* are served under a coding agent too (ADR 0004).
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "agent") }
	}
}

func TestHostScreenStartAnswersBeforeItsNotifications(t *testing.T) {
	k := &screenKind{}
	_, a := startBridge(t, withScreen(k))
	var list map[string]json.RawMessage
	if e := a.call(t, wire.MethodHostScreenList, nil, &list); e != nil {
		t.Fatal(e)
	}
	if _, ok := list[wire.HostScreenBrowser]; !ok || len(list) != 1 {
		t.Fatalf("host_screen.list = %v, want the browser section only", list)
	}
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if m := <-a.goCall(wire.MethodHostScreenStart, "start", target); m.Error != nil {
		t.Fatal(m.Error)
	}
	var frame wire.HostScreenFrame
	json.Unmarshal(a.nextNotify(t, wire.NotifyHostScreenFrame).Params, &frame)
	a.mu.Lock()
	log := slices.Clone(a.log)
	a.mu.Unlock()
	if want := []string{"resp:start", "notify:" + wire.NotifyHostScreenState, "notify:" + wire.NotifyHostScreenFrame}; !slices.Equal(log, want) {
		t.Fatalf("arrival order %q, want %q", log, want)
	}
	if frame.Seq != 1 || frame.Data != "anBn" || frame.Width != 2 {
		t.Fatalf("frame %+v", frame)
	}
	if e := a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: frame.Seq}, nil); e != nil {
		t.Fatal(e)
	}
	if e := a.call(t, wire.MethodHostScreenStop, nil, nil); e != nil {
		t.Fatal(e)
	}
	if e := a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: 1}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("ack without a stream: %v, want bad_request", e)
	}
	if e := a.call(t, wire.MethodHostScreenInput, map[string]any{"type": "tap", "x": 0.5, "y": 0.5}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("input without a stream: %v, want bad_request", e)
	}
}

func TestHostScreenAckIsNotQueuedBehindStart(t *testing.T) {
	k := &screenKind{}
	_, a := startBridge(t, withScreen(k))
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if e := a.call(t, wire.MethodHostScreenStart, target, nil); e != nil {
		t.Fatal(e)
	}
	a.nextNotify(t, wire.NotifyHostScreenFrame)
	gate := make(chan struct{})
	k.gate.Store(&gate)
	started := a.goCall(wire.MethodHostScreenStart, "start", target)
	acked := a.goCall(wire.MethodHostScreenAck, "ack", wire.HostScreenAckParams{Seq: 1})
	select {
	case m := <-acked:
		if m.Error != nil {
			t.Fatalf("ack during a start: %v", m.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ack waited for the start")
	}
	close(gate)
	if m := <-started; m.Error != nil {
		t.Fatal(m.Error)
	}
}

// iOS simulators are shown on macOS only, announced as host_screen_ios
// (ADR 0008): elsewhere list has no ios section and starting one is
// unsupported.
func TestHostScreenIOSIsServedOnMacOSOnly(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			if goos != "darwin" {
				// Listing asks for Android emulators: keep it from this
				// machine's adb (PATH, the SDK variables, the SDK under
				// the home directory).
				empty := t.TempDir()
				for _, k := range []string{"PATH", "HOME", "USERPROFILE", "LOCALAPPDATA"} {
					t.Setenv(k, empty)
				}
				t.Setenv("ANDROID_HOME", "")
				t.Setenv("ANDROID_SDK_ROOT", "")
			}
			_, a := startBridge(t, func(b *Bridge) {
				b.hostScreen = hostscreen.NewServer(func(method string, params any) { b.out.Notify(method, params) }, b.hostScreenKinds(goos)...)
			})
			onMac := goos == "darwin"
			if got := slices.Contains(helloCaps(t, a), wire.CapHostScreenIOS); got != onMac {
				t.Fatalf("hello announces %s: %v, want %v", wire.CapHostScreenIOS, got, onMac)
			}
			if onMac {
				return // listing would ask this machine's simctl
			}
			var list map[string]json.RawMessage
			if e := a.call(t, wire.MethodHostScreenList, nil, &list); e != nil {
				t.Fatal(e)
			}
			if _, ok := list[wire.HostScreenIOS]; ok {
				t.Fatalf("host_screen.list has an ios section: %v", list)
			}
			target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"}}
			if e := a.call(t, wire.MethodHostScreenStart, target, nil); e == nil || e.Code != wire.ErrUnsupported {
				t.Fatalf("start ios: %v, want unsupported", e)
			}
		})
	}
}

func TestHostScreenInputsKeepTheirOrderAndAcksPassASlowOne(t *testing.T) {
	k := &screenKind{}
	_, a := startBridge(t, withScreen(k))
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if e := a.call(t, wire.MethodHostScreenStart, target, nil); e != nil {
		t.Fatal(e)
	}
	a.nextNotify(t, wire.NotifyHostScreenFrame) // seq 1
	st := k.stream.Load()
	st.ev.Frame(hostscreen.Frame{JPEG: []byte("2"), Width: 2, Height: 1}) // seq 2: two in flight
	a.nextNotify(t, wire.NotifyHostScreenFrame)
	st.ev.Frame(hostscreen.Frame{JPEG: []byte("3"), Width: 2, Height: 1}) // held

	// A slow input (adb's BACK can take 0.43 s) holds the queue.
	gate := make(chan struct{})
	st.inputGate.Store(&gate)
	var inputs []<-chan *wire.Msg
	for i := range 5 {
		inputs = append(inputs, a.goCall(wire.MethodHostScreenInput, fmt.Sprint("input", i), wire.HostScreenInput{Type: wire.InputText, Text: fmt.Sprint(i)}))
	}
	acked := a.goCall(wire.MethodHostScreenAck, "ack", wire.HostScreenAckParams{Seq: 1})
	select {
	case m := <-acked:
		if m.Error != nil {
			t.Fatalf("ack behind an input: %v", m.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ack waited for the input")
	}
	// The ack freed a place: the held frame comes while the input waits.
	var frame wire.HostScreenFrame
	json.Unmarshal(a.nextNotify(t, wire.NotifyHostScreenFrame).Params, &frame)
	if frame.Seq != 3 || frame.Data != "Mw==" {
		t.Fatalf("frame after the ack %+v, want seq 3 Mw==", frame)
	}
	close(gate)
	for i, ch := range inputs {
		m := <-ch
		if m.Error != nil {
			t.Fatalf("input %d: %v", i, m.Error)
		}
		// The stream's result is the reply's.
		var res wire.HostScreenInputResult
		if err := json.Unmarshal(m.Result, &res); err != nil || res != (wire.HostScreenInputResult{Via: wire.InputViaKeys, App: fmt.Sprint(i)}) {
			t.Fatalf("input %d answered %s, want the stream's result", i, m.Result)
		}
	}
	st.mu.Lock()
	got := slices.Clone(st.inputs)
	st.mu.Unlock()
	if want := []string{"0", "1", "2", "3", "4"}; !slices.Equal(got, want) {
		t.Fatalf("inputs arrived %q, want %q", got, want)
	}
}

func TestHostScreenPauseAndResumePassASlowInput(t *testing.T) {
	k := &screenKind{}
	_, a := startBridge(t, withScreen(k))
	if e := a.call(t, wire.MethodHostScreenPause, nil, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("pause without a stream: %v, want bad_request", e)
	}
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if e := a.call(t, wire.MethodHostScreenStart, target, nil); e != nil {
		t.Fatal(e)
	}
	a.nextNotify(t, wire.NotifyHostScreenFrame)
	st := k.stream.Load()
	gate := make(chan struct{})
	st.inputGate.Store(&gate)
	input := a.goCall(wire.MethodHostScreenInput, "input", wire.HostScreenInput{Type: wire.InputText, Text: "slow"})
	for _, method := range []string{wire.MethodHostScreenPause, wire.MethodHostScreenResume} {
		select {
		case m := <-a.goCall(method, method, nil):
			if m.Error != nil {
				t.Fatalf("%s behind an input: %v", method, m.Error)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s waited for the input", method)
		}
	}
	st.mu.Lock()
	calls := slices.Clone(st.calls)
	st.mu.Unlock()
	if want := []string{"pause", "resume"}; !slices.Equal(calls, want) {
		t.Fatalf("stream calls %q, want %q", calls, want)
	}
	close(gate)
	if m := <-input; m.Error != nil {
		t.Fatal(m.Error)
	}
}

// rasterKind opens rasterStreams: pictures as pixels (like the Android
// emulator's), at the quality asked.
type rasterKind struct{ stream atomic.Pointer[rasterStream] }

func (k *rasterKind) Name() string { return wire.HostScreenBrowser }
func (k *rasterKind) List(context.Context) (any, error) {
	return wire.HostScreenSection[wire.HostScreenBrowserItem]{Items: []wire.HostScreenBrowserItem{}}, nil
}
func (k *rasterKind) Open(context.Context, wire.HostScreenTarget) (hostscreen.Stream, error) {
	st := &rasterStream{q: hostscreen.Normal}
	k.stream.Store(st)
	return st, nil
}

// rasterStream sends a 64×32 picture at its quality (half the px at half
// scale), and one afresh on a settle, from its own goroutine.
type rasterStream struct {
	ev hostscreen.Events
	mu sync.Mutex
	q  hostscreen.Quality
}

func (s *rasterStream) Start(ev hostscreen.Events) {
	s.ev = ev
	ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	s.frame()
}
func (*rasterStream) Pause()  {}
func (*rasterStream) Resume() {}
func (*rasterStream) Close()  {}
func (*rasterStream) Input(context.Context, wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	return wire.HostScreenInputResult{}, nil
}
func (s *rasterStream) SetQuality(q hostscreen.Quality, settle bool) {
	s.mu.Lock()
	s.q = q
	s.mu.Unlock()
	if settle {
		go s.frame()
	}
}

func (s *rasterStream) frame() {
	s.mu.Lock()
	q := s.q
	s.mu.Unlock()
	w, h := int(64*q.Scale), int(32*q.Scale)
	pix := make([]byte, w*h*3)
	for i := range pix {
		pix[i] = byte(i * 7)
	}
	s.ev.Frame(hostscreen.Frame{Raster: &hostscreen.Raster{Pix: pix, W: w, H: h, JPEG: q.JPEG}, Width: 64, Height: 32})
}

// shownFrame is a host_screen.frame's JPEG px and luma DC quantizer (the
// higher, the coarser).
type shownFrame struct {
	seq       int64
	w, h, dcQ int
}

func nextFrame(t *testing.T, a *app) shownFrame {
	t.Helper()
	m := a.nextNotify(t, wire.NotifyHostScreenFrame)
	var fr wire.HostScreenFrame
	if err := json.Unmarshal(m.Params, &fr); err != nil {
		t.Fatal(err)
	}
	jpg, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
	if err != nil {
		t.Fatalf("frame %d is no JPEG: %v", fr.Seq, err)
	}
	i := bytes.Index(jpg, []byte{0xff, 0xdb})
	if i < 0 || i+5 >= len(jpg) {
		t.Fatalf("frame %d has no quantization table", fr.Seq)
	}
	return shownFrame{seq: fr.Seq, w: cfg.Width, h: cfg.Height, dcQ: int(jpg[i+5])}
}

// An app that announced no capability (an app before #1974) gets every
// frame as host_screen.frame, those while operating too: smaller and
// coarser (half the px on a slow link), then the normal one once the
// screen rests.
func TestAnAppWithoutCapabilitiesGetsJSONFramesOfEveryQuality(t *testing.T) {
	k := &rasterKind{}
	_, a := startBridge(t, func(b *Bridge) {
		b.hostScreen = hostscreen.NewServer(func(method string, params any) { b.out.Notify(method, params) }, k)
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "agent") }
	})
	helloAs(t, a)
	target := wire.HostScreenStartParams{Target: wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 9222, TargetID: "P"}}
	if e := a.call(t, wire.MethodHostScreenStart, target, nil); e != nil {
		t.Fatal(e)
	}
	normal := nextFrame(t, a)
	if normal.w != 64 || normal.h != 32 {
		t.Fatalf("first frame %+v, want 64×32", normal)
	}
	// Acked slowly: a slow link.
	time.Sleep(300 * time.Millisecond)
	a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: normal.seq}, nil)

	if e := a.call(t, wire.MethodHostScreenInput, wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}, nil); e != nil {
		t.Fatal(e)
	}
	k.stream.Load().frame() // the tap changed the screen
	low := nextFrame(t, a)
	if low.w != 32 || low.h != 16 || low.dcQ <= normal.dcQ {
		t.Fatalf("frame while operating %+v, want 32×16 and coarser than %+v", low, normal)
	}
	a.call(t, wire.MethodHostScreenAck, wire.HostScreenAckParams{Seq: low.seq}, nil)

	settled := nextFrame(t, a)
	if settled.w != 64 || settled.h != 32 || settled.dcQ != normal.dcQ || settled.seq != low.seq+1 {
		t.Fatalf("frame once settled %+v, want 64×32 at %+v's quality", settled, normal)
	}
	a.mu.Lock()
	log := slices.Clone(a.log)
	a.mu.Unlock()
	for _, l := range log {
		if strings.HasPrefix(l, "notify:") && l != "notify:"+wire.NotifyHostScreenFrame && l != "notify:"+wire.NotifyHostScreenState {
			t.Fatalf("notification %q to an app without capabilities", l)
		}
	}
}
