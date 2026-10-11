// Package hostscreen serves the host_screen.* methods of one bridge
// connection (ホストの画面, ADR 0008): it lists what can be shown, runs at
// most one stream and paces its frames to the app's acks. Each kind of
// target (browser, Android emulator, iOS simulator) is a Kind in its own
// package.
package hostscreen

import (
	"context"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const (
	// MaxInFlight is how many frames may wait for the app's ack.
	MaxInFlight = 2
	// StallAfter is how long the app may hold MaxInFlight frames before
	// the stream is paused; its next ack resumes it with a full frame.
	StallAfter = 2 * time.Second
)

// Frame is one picture of a target.
type Frame struct {
	Data          string // base64 of one JPEG
	Width, Height int    // the target's size in its own coordinates
}

// Events receives what a Stream produces. A Stream calls it from its own
// goroutines; the calls need not be serialized.
type Events interface {
	Frame(Frame)
	// State reports wire.HostScreenLive (with the target's title and URL
	// when it has them) or wire.HostScreenGone, after which no call
	// follows.
	State(wire.HostScreenState)
}

// Stream is one target being shown. Its methods do not block on the
// target, except Close.
type Stream interface {
	// Start begins delivery: State live first, then a full frame, then a
	// frame whenever the target changes.
	Start(Events)
	// Pause stops capturing while the app is stalled.
	Pause()
	// Resume captures again, beginning with a full frame (the target may
	// not change by itself).
	Resume()
	// Close stops the stream and waits for its goroutines: no Events call
	// follows its return.
	Close()
}

// Inputter is a Stream that takes host_screen.input; a Stream without it
// answers every input unsupported.
type Inputter interface {
	// Input performs in on the target (already checked by CheckInput:
	// points within the frame, the fields its type needs) and returns once
	// the target took it. Inputs come one at a time, in the order the app
	// sent them; Input may block on the target, never on the app's acks.
	// Errors are *wire.Error: unsupported for an input the kind does not
	// take.
	Input(ctx context.Context, in wire.HostScreenInput) error
}

// Kind is one kind of target: its section of host_screen.list and its
// streams.
type Kind interface {
	// Name is the target kind and the key of its section.
	Name() string
	// List returns the section (a wire.HostScreenSection).
	List(ctx context.Context) (any, error)
	// Open connects to t (t.Kind is Name()). Errors are *wire.Error:
	// bad_request, not_found or unavailable.
	Open(ctx context.Context, t wire.HostScreenTarget) (Stream, error)
}

// PortsKind is a Kind whose section also lists the CDP ports the app named
// by hand (host_screen.list's cdp_ports): the browser kind.
type PortsKind interface {
	Kind
	// ListPorts is List plus ports (each 1–65535, without repeats).
	ListPorts(ctx context.Context, ports []int) (any, error)
}

// Server is the host_screen.* state of one bridge connection.
type Server struct {
	kinds  []Kind
	notify func(method string, params any)
	// afterFunc runs f after d unless stopped; replaceable in tests.
	afterFunc func(d time.Duration, f func()) (stop func() bool)

	seq atomic.Int64 // last frame seq of this connection

	runMu sync.Mutex // serializes Start and Stop
	mu    sync.Mutex // guards cur, never held while waiting on a target
	cur   *flow
}

// NewServer serves kinds, writing notifications with notify (safe for
// concurrent use).
func NewServer(notify func(method string, params any), kinds ...Kind) *Server {
	return &Server{
		kinds:  kinds,
		notify: notify,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
	}
}

// Serves tells whether kind is one of the server's kinds.
func (s *Server) Serves(kind string) bool {
	for _, k := range s.kinds {
		if k.Name() == kind {
			return true
		}
	}
	return false
}

// List answers host_screen.list: one section per kind. A port of
// p.CDPPorts outside 1–65535 is bad_request.
func (s *Server) List(ctx context.Context, p wire.HostScreenListParams) (map[string]any, error) {
	var ports []int
	for _, port := range p.CDPPorts {
		if port < 1 || port > 65535 {
			return nil, wire.Errorf(wire.ErrBadRequest, "host_screen.list: cdp_ports: %d out of range", port)
		}
		if !slices.Contains(ports, port) {
			ports = append(ports, port)
		}
	}
	res := make(map[string]any, len(s.kinds))
	for _, k := range s.kinds {
		var sec any
		var err error
		if pk, ok := k.(PortsKind); ok {
			sec, err = pk.ListPorts(ctx, ports)
		} else {
			sec, err = k.List(ctx)
		}
		if err != nil {
			return nil, err
		}
		res[k.Name()] = sec
	}
	return res, nil
}

// Start answers host_screen.start: it stops the current stream, opens t
// and, once opened, calls started (which answers the app) before the
// stream's first notification.
func (s *Server) Start(ctx context.Context, t wire.HostScreenTarget, started func()) error {
	var kind Kind
	for _, k := range s.kinds {
		if k.Name() == t.Kind {
			kind = k
		}
	}
	if kind == nil {
		switch t.Kind {
		case wire.HostScreenBrowser, wire.HostScreenAndroid, wire.HostScreenIOS:
			return wire.Errorf(wire.ErrUnsupported, "host_screen.start: kind %q is not served by this stagent", t.Kind)
		}
		return wire.Errorf(wire.ErrBadRequest, "host_screen.start: unknown kind %q", t.Kind)
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	// The old stream stays current while the new one opens: the app's
	// acks for its frames are ignored, not refused.
	s.mu.Lock()
	old := s.cur
	s.mu.Unlock()
	if old != nil {
		old.close()
	}
	st, err := kind.Open(ctx, t)
	var f *flow
	if err == nil {
		f = &flow{s: s, st: st}
	}
	s.mu.Lock()
	s.cur = f
	s.mu.Unlock()
	if err != nil {
		return err
	}
	started()
	st.Start(f)
	return nil
}

// Ack answers host_screen.ack. It never waits for the stream's target.
func (s *Server) Ack(seq int64) error {
	s.mu.Lock()
	f := s.cur
	s.mu.Unlock()
	if f == nil {
		return wire.Errorf(wire.ErrBadRequest, "host_screen.ack: no stream")
	}
	f.ack(seq)
	return nil
}

// Input answers host_screen.input: it checks in, then has the current
// stream perform it. The caller runs inputs one at a time in the app's
// order (with Start and Stop); acks never wait for them.
func (s *Server) Input(ctx context.Context, in wire.HostScreenInput) error {
	if err := CheckInput(in); err != nil {
		return err
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.mu.Lock()
	f := s.cur
	s.mu.Unlock()
	if f == nil {
		return wire.Errorf(wire.ErrBadRequest, "host_screen.input: no stream")
	}
	f.mu.Lock()
	gone, unresponsive := f.gone, f.unresponsive
	f.mu.Unlock()
	if gone {
		return wire.Errorf(wire.ErrBadRequest, "host_screen.input: the target is gone")
	}
	is, ok := f.st.(Inputter)
	if !ok {
		return wire.Errorf(wire.ErrUnsupported, "host_screen.input: this kind of target takes no input")
	}
	// A target that did not answer an input (a page held by alert()) would
	// hold every input queued behind it, and the start or stop after them,
	// for as long again: they are refused until it sends a frame.
	if unresponsive {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: the target did not answer the previous input")
	}
	err := is.Input(ctx, in)
	var we *wire.Error
	if errors.As(err, &we) && we.Code == wire.ErrUnavailable {
		f.mu.Lock()
		f.unresponsive = true
		f.mu.Unlock()
	}
	return err
}

// Bounds of an input, so one request cannot hold the input queue long.
const (
	MaxDragPoints   = 1000
	MaxDragDuration = 10_000 // ms
	// MaxScroll bounds dx and dy: the fingers may cross a fitted frame
	// several times over.
	MaxScroll = 10
)

// CheckInput refuses with bad_request an input whose shape is wrong for
// its type: a point outside the frame (0–1), a drag of fewer than two
// points, a text without text or enter, an unknown key or action, a
// navigate to an empty url. Whether the target takes it is the stream's
// to say.
func CheckInput(in wire.HostScreenInput) error {
	bad := func(format string, a ...any) error {
		return wire.Errorf(wire.ErrBadRequest, "host_screen.input: "+format, a...)
	}
	switch in.Type {
	case wire.InputTap:
		if !inFrame(in.X, in.Y) {
			return bad("tap (%v, %v) is outside the frame", in.X, in.Y)
		}
	case wire.InputDrag:
		if len(in.Points) < 2 || len(in.Points) > MaxDragPoints {
			return bad("a drag has 2 to %d points, not %d", MaxDragPoints, len(in.Points))
		}
		for _, p := range in.Points {
			if !inFrame(p.X, p.Y) {
				return bad("drag point (%v, %v) is outside the frame", p.X, p.Y)
			}
		}
		if in.DurationMS < 0 || in.DurationMS > MaxDragDuration {
			return bad("duration_ms %d is outside 0–%d", in.DurationMS, MaxDragDuration)
		}
	case wire.InputScroll:
		if !inFrame(in.X, in.Y) {
			return bad("scroll (%v, %v) is outside the frame", in.X, in.Y)
		}
		if !finite(in.DX) || !finite(in.DY) || math.Abs(in.DX) > MaxScroll || math.Abs(in.DY) > MaxScroll {
			return bad("scroll (%v, %v) is outside ±%d", in.DX, in.DY, MaxScroll)
		}
	case wire.InputText:
		if in.Text == "" && !in.Enter {
			return bad("text without text or enter")
		}
	case wire.InputKey:
		switch in.Key {
		case wire.KeyBack, wire.KeyHome, wire.KeyRecents:
		default:
			return bad("unknown key %q", in.Key)
		}
	case wire.InputNavigate:
		switch in.Action {
		case wire.NavigateBack, wire.NavigateForward, wire.NavigateReload:
		case wire.NavigateURL:
			if in.URL == "" {
				return bad("navigate url without url")
			}
		default:
			return bad("unknown action %q", in.Action)
		}
	default:
		return bad("unknown type %q", in.Type)
	}
	return nil
}

func inFrame(x, y float64) bool { return x >= 0 && x <= 1 && y >= 0 && y <= 1 }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// Stop answers host_screen.stop; without a stream it does nothing.
func (s *Server) Stop() {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.stopRunning()
}

func (s *Server) stopRunning() {
	s.mu.Lock()
	f := s.cur
	s.cur = nil
	s.mu.Unlock()
	if f != nil {
		f.close()
	}
}

// flow paces one stream's frames: at most MaxInFlight wait for an ack;
// of the frames captured meanwhile only the newest is kept, and sent as
// soon as an ack frees a place.
type flow struct {
	s  *Server
	st Stream

	mu       sync.Mutex
	inFlight []int64
	held     *Frame
	closed   bool
	gone     bool
	// unresponsive: an input ended unavailable and no frame came since.
	unresponsive bool
	stalled      bool
	stopWait     func() bool // the stall timer while MaxInFlight are out
}

func (f *flow) Frame(fr Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unresponsive = false
	if f.closed || f.gone {
		return
	}
	if len(f.inFlight) >= MaxInFlight {
		f.held = &fr
		return
	}
	f.sendLocked(fr)
}

func (f *flow) State(st wire.HostScreenState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.gone {
		return
	}
	if st.State == wire.HostScreenGone {
		f.gone = true
		f.held = nil
		f.stopTimerLocked()
	}
	f.s.notify(wire.NotifyHostScreenState, st)
}

func (f *flow) sendLocked(fr Frame) {
	seq := f.s.seq.Add(1)
	f.inFlight = append(f.inFlight, seq)
	f.s.notify(wire.NotifyHostScreenFrame, wire.HostScreenFrame{Seq: seq, Data: fr.Data, Width: fr.Width, Height: fr.Height})
	if len(f.inFlight) >= MaxInFlight && f.stopWait == nil {
		f.stopWait = f.s.afterFunc(StallAfter, f.stall)
	}
}

// stall pauses the stream while the app still holds MaxInFlight frames.
// Pause and Resume are called under f.mu so they reach the stream in
// order.
func (f *flow) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.gone || f.stalled || len(f.inFlight) < MaxInFlight {
		return
	}
	f.stalled = true
	f.stopWait = nil
	f.st.Pause()
}

func (f *flow) stopTimerLocked() {
	if f.stopWait != nil {
		f.stopWait()
		f.stopWait = nil
	}
}

// ack frees seq's place. An unknown seq (a replaced stream's, a repeat)
// changes nothing.
func (f *flow) ack(seq int64) {
	f.mu.Lock()
	i := -1
	for j, s := range f.inFlight {
		if s == seq {
			i = j
			break
		}
	}
	if i < 0 || f.closed {
		f.mu.Unlock()
		return
	}
	f.inFlight = append(f.inFlight[:i], f.inFlight[i+1:]...)
	f.stopTimerLocked()
	if f.held != nil && !f.gone {
		fr := *f.held
		f.held = nil
		f.sendLocked(fr)
	}
	if f.stalled && !f.gone {
		f.st.Resume()
	}
	f.stalled = false
	f.mu.Unlock()
}

func (f *flow) close() {
	f.mu.Lock()
	f.closed = true
	f.held = nil
	f.stopTimerLocked()
	f.mu.Unlock()
	f.st.Close()
}
