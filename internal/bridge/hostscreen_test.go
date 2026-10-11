package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
	ev.Frame(hostscreen.Frame{Data: "anBn", Width: 2, Height: 1})
}
func (s *screenStream) Pause()  { s.note("pause") }
func (s *screenStream) Resume() { s.note("resume") }
func (s *screenStream) note(c string) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}
func (*screenStream) Close() {}
func (s *screenStream) Input(_ context.Context, in wire.HostScreenInput) error {
	if g := s.inputGate.Load(); g != nil {
		<-*g
	}
	s.mu.Lock()
	s.inputs = append(s.inputs, in.Text)
	s.mu.Unlock()
	return nil
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
	st.ev.Frame(hostscreen.Frame{Data: "Mg==", Width: 2, Height: 1}) // seq 2: two in flight
	a.nextNotify(t, wire.NotifyHostScreenFrame)
	st.ev.Frame(hostscreen.Frame{Data: "Mw==", Width: 2, Height: 1}) // held

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
		if m := <-ch; m.Error != nil {
			t.Fatalf("input %d: %v", i, m.Error)
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
