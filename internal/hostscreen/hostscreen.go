// Package hostscreen serves the host_screen.* methods of one bridge
// connection (ホストの画面, ADR 0008): it lists what can be shown, runs at
// most one stream and paces its frames to the app's acks. Each kind of
// target (browser, Android emulator, iOS simulator) is a Kind in its own
// package.
package hostscreen

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image"
	"io"
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
	// FrameEvery is the least time between two frames sent: at most 10
	// a second (ADR 0008 "通信量と電池", #634).
	FrameEvery = 100 * time.Millisecond
)

// Frame is one picture of a target.
type Frame struct {
	JPEG []byte // one JPEG, not changed once handed over
	// Raster, when set instead of JPEG, is the picture as pixels: encoded
	// only when sent, whole or as the tiles changed (tiles.go).
	Raster        *Raster
	Width, Height int // the target's size in its own coordinates
}

// Events receives what a Stream produces. A Stream calls it from its own
// goroutines; the calls need not be serialized.
type Events interface {
	Frame(Frame)
	// State reports wire.HostScreenLive (with the target's title and URL
	// when it has them) or wire.HostScreenGone, after which no call
	// follows, but for a target that comes back (see Reviving): live
	// again, then frames as after Start, the stream not paused.
	State(wire.HostScreenState)
}

// Stream is one target being shown. Its methods do not block on the
// target, except Close.
type Stream interface {
	// Start begins delivery: State live first, then a full frame, then a
	// frame whenever the target changes.
	Start(Events)
	// Pause stops capturing: while the app is stalled, or while it does
	// not show the stream (host_screen.pause).
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
	// the target took it, with host_screen.input's result (empty but for
	// an iOS simulator's text). Inputs come one at a time, in the order the
	// app sent them; Input may block on the target, never on the app's
	// acks. Errors are *wire.Error: unsupported for an input the kind does
	// not take.
	Input(ctx context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error)
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
	// now and afterFunc (which runs f after d unless stopped) are the
	// clock, spawn runs a frame's sender on a goroutine of its own;
	// replaceable in tests.
	now       func() time.Time
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	spawn     func(f func())

	seq atomic.Int64 // last frame seq of this connection

	runMu sync.Mutex // serializes Start and Stop
	mu    sync.Mutex // guards cur, never held while waiting on a target
	cur   *flow

	sinkMu sync.Mutex
	sink   io.WriteCloser // the binary frame channel, nil without one

	tiles atomic.Bool // the app takes "tiles" records (SetTiles)
}

// SetTiles tells whether the app takes "tiles" records on the binary
// channel (it announced wire.CapHostScreenTiles).
func (s *Server) SetTiles(on bool) { s.tiles.Store(on) }

// NewServer serves kinds, writing notifications with notify (safe for
// concurrent use).
func NewServer(notify func(method string, params any), kinds ...Kind) *Server {
	return &Server{
		kinds:  kinds,
		notify: notify,
		now:    time.Now,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
		spawn: func(f func()) { go f() },
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
		f = &flow{s: s, st: st, op: operation{quality: Normal}}
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
	f, err := s.current("host_screen.ack")
	if err != nil {
		return err
	}
	f.ack(seq)
	return nil
}

// Pause answers host_screen.pause: the app does not show the stream, so
// its target is no longer captured and the frame held is dropped. Like
// Ack, it never waits for the target. Pausing a paused stream does
// nothing.
func (s *Server) Pause() error {
	f, err := s.current("host_screen.pause")
	if err != nil {
		return err
	}
	f.pause()
	return nil
}

// Resume answers host_screen.resume: the target is captured again,
// beginning with a full frame. Resuming a stream not paused does nothing.
func (s *Server) Resume() error {
	f, err := s.current("host_screen.resume")
	if err != nil {
		return err
	}
	f.resume()
	return nil
}

// Keyframe answers host_screen.keyframe: the app could not apply a
// "tiles" record, so the next frame goes whole; when none is captured, the
// picture sent last goes again, whole. Like Ack, it never waits for the
// target.
func (s *Server) Keyframe() error {
	f, err := s.current("host_screen.keyframe")
	if err != nil {
		return err
	}
	f.keyframe()
	return nil
}

// current is the stream method's flow; bad_request without one.
func (s *Server) current(method string) (*flow, error) {
	s.mu.Lock()
	f := s.cur
	s.mu.Unlock()
	if f == nil {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: no stream", method)
	}
	return f, nil
}

// Input answers host_screen.input: it checks in, then has the current
// stream perform it. The caller runs inputs one at a time in the app's
// order (with Start and Stop); acks never wait for them.
func (s *Server) Input(ctx context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	var none wire.HostScreenInputResult
	if err := CheckInput(in); err != nil {
		return none, err
	}
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.mu.Lock()
	f := s.cur
	s.mu.Unlock()
	if f == nil {
		return none, wire.Errorf(wire.ErrBadRequest, "host_screen.input: no stream")
	}
	f.mu.Lock()
	gone, unresponsive := f.gone, f.unresponsive
	f.mu.Unlock()
	if gone {
		return none, wire.Errorf(wire.ErrBadRequest, "host_screen.input: the target is gone")
	}
	is, ok := f.st.(Inputter)
	if !ok {
		return none, wire.Errorf(wire.ErrUnsupported, "host_screen.input: this kind of target takes no input")
	}
	// A target that did not answer an input (a page held by alert()) would
	// hold every input queued behind it, and the start or stop after them,
	// for as long again: they are refused until it sends a frame.
	if unresponsive {
		return none, wire.Errorf(wire.ErrUnavailable, "host_screen.input: the target did not answer the previous input")
	}
	f.operate()
	res, err := is.Input(ctx, in)
	f.operated()
	var we *wire.Error
	if errors.As(err, &we) && we.Code == wire.ErrUnavailable {
		f.mu.Lock()
		f.unresponsive = true
		f.mu.Unlock()
	}
	return res, err
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
		if !inFrame(in.X, in.Y) {
			return bad("text cursor (%v, %v) is outside the frame", in.X, in.Y)
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

// Attach makes w, a binary frame channel, carry the frames from the next
// one on as records (WriteRecord) instead of host_screen.frame, beginning
// with a full one; a channel attached before is closed. detach (when w
// goes away) closes w and puts the frames back on host_screen.frame, as a
// write to w that fails does; the frames w carried are then no longer
// waited for, since their acks may never come. detach after w was
// replaced or failed does nothing.
func (s *Server) Attach(w io.WriteCloser) (detach func()) {
	s.sinkMu.Lock()
	old := s.sink
	s.sink = w
	s.sinkMu.Unlock()
	if old != nil {
		old.Close()
	}
	if f := s.curFlow(); f != nil {
		f.wantFull()
	}
	return func() {
		if !s.dropSink(w) {
			return
		}
		if f := s.curFlow(); f != nil {
			f.channelLost()
		}
	}
}

// curFlow is the current stream's flow, nil without one.
func (s *Server) curFlow() *flow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// channel is the binary frame channel attached, nil without one.
func (s *Server) channel() io.WriteCloser {
	s.sinkMu.Lock()
	defer s.sinkMu.Unlock()
	return s.sink
}

// dropSink closes w and tells whether it was the channel attached.
func (s *Server) dropSink(w io.WriteCloser) bool {
	s.sinkMu.Lock()
	cur := s.sink == w
	if cur {
		s.sink = nil
	}
	s.sinkMu.Unlock()
	if cur {
		w.Close()
	}
	return cur
}

// notifyFrame sends a frame, h (its seq and size) with jpg, as
// host_screen.frame and returns the bytes that took on the way.
func (s *Server) notifyFrame(h wire.HostScreenRecordHeader, jpg []byte) int {
	data := base64.StdEncoding.EncodeToString(jpg)
	s.notify(wire.NotifyHostScreenFrame, wire.HostScreenFrame{Seq: h.Seq, Data: data, Width: h.Width, Height: h.Height})
	return len(data)
}

// flow paces one stream's frames: at most MaxInFlight wait for an ack and
// they are sent at least FrameEvery apart; of the frames captured
// meanwhile only the newest is kept, and sent as soon as both allow.
//
// A frame let go is encoded by a sender of its own (send), one at a time,
// outside mu: encoding a raster takes milliseconds, which neither whoever
// let it go (the bridge's reader with an ack or a keyframe, the stream
// with a frame) nor the acks, pauses and inputs behind mu may wait for.
type flow struct {
	s  *Server
	st Stream

	mu       sync.Mutex
	inFlight []int64
	held     *Frame
	lastSent time.Time
	closed   bool
	gone     bool
	// unresponsive: an input ended unavailable and no frame came since.
	unresponsive bool
	// The stream is paused while stalled (the app holds MaxInFlight
	// frames) or paused (host_screen.pause), and resumed once neither.
	stalled  bool
	paused   bool
	stopWait func() bool // the stall timer while MaxInFlight are out
	stopPace func() bool // the timer sending held once FrameEvery passed
	// sending: a sender has a frame (until it sent or dropped it); next is
	// the frame let go, whose sender unlock starts.
	sending bool
	next    *Frame
	// The quality asked of the stream while operating (quality.go), with
	// the link's bandwidth estimated from the frames' acks.
	op   operation
	link linkRate
	// What the app's picture is built on (tiles.go): base is the frame
	// sent last, baseSeq its seq; full asks the next frame whole; lastFull
	// is when one went last, baseJPEG the lowest quality the app's picture
	// got since (a better one goes whole). enc and payload are the
	// sender's, reused.
	base     *Frame
	baseSeq  int64
	baseJPEG int
	lastFull time.Time
	full     bool
	enc      encoder
	payload  bytes.Buffer
}

// unlock releases f.mu and starts the sender of the frame flushLocked let
// go, if any. Every method that may flush ends with it.
func (f *flow) unlock() {
	next := f.next
	f.next = nil
	f.mu.Unlock()
	if next != nil {
		f.s.spawn(func() { f.send(next) })
	}
}

// wantFull has the next frame sent whole.
func (f *flow) wantFull() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.full = true
}

// keyframe has the next frame sent whole, and the picture sent last sent
// again when no frame is held or being sent (the target may not change by
// itself).
func (f *flow) keyframe() {
	f.mu.Lock()
	defer f.unlock()
	if f.closed {
		return
	}
	f.full = true
	if f.held == nil && !f.sending && f.base != nil && !f.gone && !f.paused {
		b := *f.base
		f.held = &b
	}
	f.flushLocked()
}

func (f *flow) Frame(fr Frame) {
	f.mu.Lock()
	defer f.unlock()
	f.unresponsive = false
	f.op.lastFrame = f.s.now()
	if f.closed || f.gone || f.paused {
		return
	}
	f.held = &fr
	f.flushLocked()
}

func (f *flow) State(st wire.HostScreenState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	switch {
	case st.State == wire.HostScreenGone && !f.gone:
		f.gone = true
		f.dropLocked()
		f.stopTimerLocked()
	case st.State == wire.HostScreenLive && f.gone:
		// The target came back: the frames sent before it went are no
		// longer waited for, and the stream starts unpaused, so is paused
		// again while the app does not show it.
		f.gone = false
		f.inFlight = nil
		f.link.forget()
		f.stalled = false
		f.unresponsive = false
		f.full = true
		if f.paused {
			f.st.Pause()
		}
	case f.gone:
		return
	}
	f.s.notify(wire.NotifyHostScreenState, st)
}

// flushLocked lets the held frame go once a place is free, no frame is
// being sent and FrameEvery passed since the last one sent, waiting for
// the latter on a timer. Its sender starts at unlock.
func (f *flow) flushLocked() {
	if f.held == nil || f.sending || f.closed || f.gone || f.paused || len(f.inFlight) >= MaxInFlight {
		return
	}
	if wait := f.lastSent.Add(FrameEvery).Sub(f.s.now()); wait > 0 {
		if f.stopPace == nil {
			f.stopPace = f.s.afterFunc(wait, f.pace)
		}
		return
	}
	f.sending, f.next, f.held = true, f.held, nil
}

func (f *flow) pace() {
	f.mu.Lock()
	defer f.unlock()
	f.stopPace = nil
	f.flushLocked()
}

// dropLocked forgets the held frame.
func (f *flow) dropLocked() {
	f.held = nil
	if f.stopPace != nil {
		f.stopPace()
		f.stopPace = nil
	}
}

// sendableLocked tells whether a frame may go out: as for a held frame,
// one let go is dropped once the stream closed, went or was paused.
func (f *flow) sendableLocked() bool { return !f.closed && !f.gone && !f.paused }

// send is fr's sender: it encodes fr outside f.mu (the tiles changed
// since the frame sent last, when the app can build on it, or the whole
// picture), then sends it under f.mu, where its seq, its place in flight
// and what tiles build on next change together.
func (f *flow) send(fr *Frame) {
	f.mu.Lock()
	if !f.sendableLocked() {
		f.sending = false
		f.mu.Unlock()
		return
	}
	w := f.s.channel()
	var base *Raster
	var baseSeq int64
	if w != nil && f.s.tiles.Load() && fr.Raster != nil && f.tileableLocked(*fr, f.s.now()) {
		base, baseSeq = f.base.Raster, f.baseSeq
	}
	// A full frame asked for is this one (tileableLocked said no); one
	// asked while it is encoded is the next.
	asked := f.full
	f.full = false
	f.mu.Unlock()

	h := wire.HostScreenRecordHeader{Kind: wire.HostScreenRecordFull, Width: fr.Width, Height: fr.Height}
	payload, err := f.encode(&h, fr, base, baseSeq)

	f.mu.Lock()
	defer f.unlock()
	f.sending = false
	switch {
	case err != nil || !f.sendableLocked():
		// Dropped: a picture that does not encode, or the stream stopped.
		f.full = f.full || asked
		f.flushLocked()
		return
	case h.Kind == wire.HostScreenRecordTiles && (f.full || f.s.channel() != w):
		// Asked whole meanwhile (a keyframe, a new channel): the tiles may
		// not build on the app's picture. It goes again, whole, unless a
		// newer one came.
		f.full = true
		if f.held == nil {
			f.held = fr
		}
		f.flushLocked()
		return
	}
	now := f.s.now()
	h.Seq = f.s.seq.Add(1)
	f.lastSent = now
	n, asJSON := 0, w == nil
	if w != nil {
		if n, err = WriteRecord(w, h, payload); err != nil {
			// The channel failed: what it carried may never arrive.
			f.s.dropSink(w)
			f.forgetInFlightLocked()
			asJSON = true
		}
	}
	if asJSON {
		jpg := payload
		if h.Kind == wire.HostScreenRecordTiles {
			// The tiles did not go: the whole picture does, encoded
			// outside f.mu as well (nothing else is sent meanwhile).
			f.sending = true
			f.mu.Unlock()
			f.payload.Reset()
			err = f.enc.encode(&f.payload, fr.Raster, image.Rect(0, 0, fr.Raster.W, fr.Raster.H))
			f.mu.Lock()
			f.sending = false
			if err != nil || !f.sendableLocked() {
				f.full = true
				return
			}
			jpg = f.payload.Bytes()
		}
		n = f.s.notifyFrame(h, jpg)
	}
	f.base, f.baseSeq = fr, h.Seq
	switch {
	case asJSON:
		// It may overtake the records before it, or they it.
		f.full = true
	case h.Kind == wire.HostScreenRecordFull:
		f.lastFull = now
		if fr.Raster != nil {
			f.baseJPEG = fr.Raster.JPEG
		}
	default:
		f.baseJPEG = min(f.baseJPEG, fr.Raster.JPEG)
	}
	f.inFlight = append(f.inFlight, h.Seq)
	f.link.sent(h.Seq, n, now)
	if len(f.inFlight) >= MaxInFlight && f.stopWait == nil {
		f.stopWait = f.s.afterFunc(StallAfter, f.stall)
	}
	f.flushLocked()
}

// encode returns the payload of fr's record and fills in h: the tiles
// changed since base (the raster of frame baseSeq; nil: nothing to build
// on) when they cost less than the whole (tilesUpTo, maxTileRects); else
// the whole picture. Only a sender calls it: f.enc and f.payload are its.
func (f *flow) encode(h *wire.HostScreenRecordHeader, fr *Frame, base *Raster, baseSeq int64) ([]byte, error) {
	r := fr.Raster
	if r == nil {
		return fr.JPEG, nil
	}
	if err := r.check(); err != nil {
		return nil, err
	}
	f.payload.Reset()
	if base != nil {
		rects, changed, cells := changedTiles(base, r)
		if float64(changed) <= tilesUpTo*float64(cells) && len(rects) <= maxTileRects {
			h.Kind, h.Prev, h.ImageWidth, h.ImageHeight = wire.HostScreenRecordTiles, baseSeq, r.W, r.H
			for _, rc := range rects {
				before := f.payload.Len()
				if err := f.enc.encode(&f.payload, r, rc); err != nil {
					return nil, err
				}
				h.Tiles = append(h.Tiles, wire.HostScreenTile{X: rc.Min.X, Y: rc.Min.Y, W: rc.Dx(), H: rc.Dy(), Len: f.payload.Len() - before})
			}
			return f.payload.Bytes(), nil
		}
	}
	h.ImageWidth, h.ImageHeight = r.W, r.H
	if err := f.enc.encode(&f.payload, r, image.Rect(0, 0, r.W, r.H)); err != nil {
		return nil, err
	}
	return f.payload.Bytes(), nil
}

// tileableLocked tells whether fr may go as tiles on the frame sent last:
// that frame is the picture the app has (nothing asked for a full one), of
// the same size and turn, at no lower a quality than fr's anywhere, and a
// full frame went less than FullEvery ago.
func (f *flow) tileableLocked(fr Frame, now time.Time) bool {
	b := f.base
	return !f.full && b != nil && b.Raster != nil &&
		b.Width == fr.Width && b.Height == fr.Height &&
		b.Raster.W == fr.Raster.W && b.Raster.H == fr.Raster.H &&
		fr.Raster.JPEG <= f.baseJPEG &&
		now.Sub(f.lastFull) < FullEvery
}

// forgetInFlightLocked stops waiting for the frames sent so far (a lost
// binary channel carried them): their acks, should they come, change
// nothing.
func (f *flow) forgetInFlightLocked() {
	f.inFlight = nil
	f.link.forget()
	f.stopTimerLocked()
}

// channelLost: the binary channel went away with the frames it carried.
// A stream stalled on them is resumed, the held frame sent (whole: the app
// may have missed what it would build on).
func (f *flow) channelLost() {
	f.mu.Lock()
	defer f.unlock()
	if f.closed {
		return
	}
	f.full = true
	f.forgetInFlightLocked()
	if f.stalled && !f.gone && !f.paused {
		f.st.Resume()
	}
	f.stalled = false
	f.flushLocked()
}

// stall pauses the stream while the app still holds MaxInFlight frames
// (it already is while paused). Pause and Resume are called under f.mu so
// they reach the stream in order.
func (f *flow) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.gone || f.stalled || len(f.inFlight) < MaxInFlight {
		return
	}
	f.stalled = true
	f.stopWait = nil
	if !f.paused {
		f.st.Pause()
	}
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
	defer f.unlock()
	i := slices.Index(f.inFlight, seq)
	if i < 0 || f.closed {
		return
	}
	f.inFlight = slices.Delete(f.inFlight, i, i+1)
	f.link.acked(seq, f.s.now())
	f.stopTimerLocked()
	if f.stalled {
		// Resumed with a full frame, like host_screen.resume.
		f.full = true
	}
	f.flushLocked()
	if f.stalled && !f.gone && !f.paused {
		f.st.Resume()
	}
	f.stalled = false
}

// pause stops capturing until resume; the held frame is dropped.
func (f *flow) pause() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.paused {
		return
	}
	f.paused = true
	f.dropLocked()
	if !f.stalled && !f.gone {
		f.st.Pause()
	}
}

// resume captures again unless stalled, which the next ack ends; the
// next frame goes whole.
func (f *flow) resume() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || !f.paused {
		return
	}
	f.paused = false
	f.full = true
	if !f.stalled && !f.gone {
		f.st.Resume()
	}
}

func (f *flow) close() {
	f.mu.Lock()
	f.closed = true
	f.dropLocked()
	f.stopTimerLocked()
	f.stopSettleLocked()
	f.mu.Unlock()
	f.st.Close()
}
