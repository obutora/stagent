package holder

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/screen"
	"github.com/obutora/stagent/internal/wire"
)

// Attach limits (PROTOCOL.md).
const (
	rawQueueLimit = 256 << 10
	defaultFPS    = 15
	maxFPS        = 30
)

func (h *Holder) serve(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			cs := &connState{h: h}
			rpc.Serve(ctx, c, cs.handle)
			cs.detach()
			h.dropFocus(cs)
		}()
	}
}

// connState is one client connection (normally the bridge, one per
// session it talks to). Its handler runs sequentially, so att needs no lock.
type connState struct {
	h   *Holder
	att *attachment
}

func (cs *connState) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	h := cs.h
	if m.ID == nil {
		return nil, nil // no notifications are defined towards a holder
	}
	switch m.Method {
	case wire.MethodSessionInfo:
		var p wire.SessionRef
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkID(p.ID); err != nil {
			return nil, err
		}
		return h.session(), nil

	case wire.MethodSessionAttach:
		var p wire.AttachParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkID(p.ID); err != nil {
			return nil, err
		}
		return cs.attach(c, m, p)

	case wire.MethodSessionDetach:
		var p wire.SessionRef
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkID(p.ID); err != nil {
			return nil, err
		}
		cs.detach()
		return struct{}{}, nil

	case wire.MethodSessionInput:
		var p wire.InputParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkLive(p.ID); err != nil {
			return nil, err
		}
		req, err := composeInput(p, h.scr.BracketedPaste(), h.scr.AppCursorKeys(), h.scr.Win32InputMode())
		if err != nil {
			return nil, err
		}
		h.det.Input()
		if p.Local {
			h.localInput(cs, []byte(p.Text))
		}
		if err := h.in.push(ctx, req); err != nil {
			return nil, wire.Errorf(wire.ErrSessionEnded, "%v", err)
		}
		return struct{}{}, nil

	case wire.MethodSessionResize:
		var p wire.ResizeParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkLive(p.ID); err != nil {
			return nil, err
		}
		if p.Cols <= 0 || p.Rows <= 0 {
			return nil, wire.Errorf(wire.ErrBadRequest, "invalid size %dx%d", p.Cols, p.Rows)
		}
		if !p.Force && h.localOwnsSize() {
			return nil, wire.Errorf(wire.ErrNotSizeOwner, "the local terminal owns the size of a passthrough session")
		}
		h.applySize(p.Cols, p.Rows)
		return struct{}{}, nil

	case wire.MethodSessionScrollback:
		var p wire.ScrollbackParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkID(p.ID); err != nil {
			return nil, err
		}
		if h.sb == nil {
			return nil, wire.Errorf(wire.ErrUnavailable, "scrollback is not available for this session")
		}
		data, start, end, first, err := h.sb.Read(p.Before, p.MaxBytes)
		if err != nil {
			return nil, err
		}
		return wire.ScrollbackResult{Data: data, Start: start, End: end, First: first}, nil

	case wire.MethodSessionSignal:
		var p wire.SignalParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if err := h.checkLive(p.ID); err != nil {
			return nil, err
		}
		switch p.Signal {
		case wire.SignalInterrupt:
			// Through the input queue so it stays ordered with input.
			if err := h.in.push(ctx, []inputChunk{{data: []byte{0x03}}}); err != nil {
				return nil, wire.Errorf(wire.ErrSessionEnded, "%v", err)
			}
		case wire.SignalTerminate, wire.SignalKill, wire.SignalHangup:
			if p.Signal == wire.SignalHangup {
				h.mu.Lock()
				h.hungUp = true
				h.mu.Unlock()
			}
			if err := h.pty.Signal(p.Signal); err != nil {
				return nil, err
			}
		default:
			return nil, wire.Errorf(wire.ErrBadRequest, "unknown signal %q", p.Signal)
		}
		return struct{}{}, nil
	}
	return nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method)
}

// checkID accepts the holder's own id (or none: the connection already
// names the session).
func (h *Holder) checkID(id string) error {
	if id != "" && id != h.id {
		return wire.Errorf(wire.ErrNotFound, "session %s is not held here", id)
	}
	return nil
}

func (h *Holder) checkLive(id string) error {
	if err := h.checkID(id); err != nil {
		return err
	}
	if h.isEnded() {
		return wire.Errorf(wire.ErrSessionEnded, "session %s has ended", h.id)
	}
	return nil
}

// maxResume bounds the output replayed to a resuming client; past it a
// snapshot is cheaper and just as good.
const maxResume = 1 << 20

// missedLocked returns the output a raw client that consumed the stream up
// to since has missed, if it can be replayed exactly: the bytes are still in
// the scrollback and were laid out for the current size. Read under h.mu,
// so the range ends exactly where queued live output will continue.
func (h *Holder) missedLocked(since *int64) ([]byte, bool) {
	if since == nil || h.sb == nil {
		return nil, false
	}
	s := *since
	if s < 0 || s > h.pos || h.pos-s > maxResume || h.sizePos >= s {
		return nil, false
	}
	return h.sb.Range(s, h.pos)
}

func (cs *connState) attach(c *rpc.Conn, m *wire.Msg, p wire.AttachParams) (any, error) {
	h := cs.h
	mode := p.Mode
	if mode == "" {
		mode = wire.AttachRaw
	}
	if mode != wire.AttachRaw && mode != wire.AttachScreen {
		return nil, wire.Errorf(wire.ErrBadRequest, "unknown attach mode %q", p.Mode)
	}
	fps := p.FPS
	if fps <= 0 {
		fps = defaultFPS
	}
	fps = min(fps, maxFPS)

	// Re-attaching replaces the previous attachment. The session stays
	// attached across the swap: the old attachment leaves without
	// reporting attached=false.
	if old := cs.att; old != nil {
		h.mu.Lock()
		old.replaced = true
		h.mu.Unlock()
		cs.detach()
	}
	a := &attachment{
		h: h, conn: c, raw: mode == wire.AttachRaw, fps: fps,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	h.mu.Lock()
	if h.ended {
		changed := h.syncAttachedLocked()
		h.mu.Unlock()
		if changed {
			h.link.wake()
		}
		return nil, wire.Errorf(wire.ErrSessionEnded, "session %s has ended", h.id)
	}
	var first []byte
	resumed := false
	if a.raw {
		if first, resumed = h.missedLocked(p.Since); !resumed {
			first = h.scr.Snapshot()
		}
	} else {
		first, a.differ = h.scr.SnapshotDiffer()
	}
	switch {
	case !resumed:
		it := item{data: first, reset: true}
		if a.raw {
			it.end = h.pos
		}
		a.items = append(a.items, it)
	case len(first) > 0:
		// Not counted against rawQueueLimit: like a snapshot, it is the
		// starting point the queue limit protects.
		a.items = append(a.items, item{data: first, end: h.pos})
	}
	h.atts[a] = struct{}{}
	attachedChanged := h.syncAttachedLocked()
	res := wire.AttachResult{Cols: h.sess.Cols, Rows: h.sess.Rows, Mode: mode, Offset: h.pos, Resumed: resumed}
	h.mu.Unlock()
	if attachedChanged {
		h.link.wake()
	}
	cs.att = a

	// The result must precede the first `output`, so reply before the
	// sender starts.
	c.Reply(m.ID, res)
	a.signal()
	go a.run()
	return rpc.Async, nil
}

// syncAttachedLocked updates sess.Attached from the attachments (h.mu
// held) and reports whether it changed; the caller then wakes the daemon
// link.
func (h *Holder) syncAttachedLocked() bool {
	attached := len(h.atts) > 0
	if attached == h.sess.Attached {
		return false
	}
	h.sess.Attached = attached
	return true
}

func (cs *connState) detach() {
	if cs.att == nil {
		return
	}
	cs.att.detach()
	cs.att = nil
}

type item struct {
	data   []byte
	reset  bool
	end    int64 // raw: stream position after data (a snapshot: where it was taken)
	resize *wire.ResizeParams
}

// attachment streams one session to one client. Producers only queue and
// signal; the run goroutine alone writes to the connection, so a slow
// client never blocks output processing.
type attachment struct {
	h      *Holder
	conn   *rpc.Conn
	raw    bool
	fps    int
	differ *screen.Differ // screen mode

	mu     sync.Mutex
	items  []item
	queued int  // raw bytes queued
	resync bool // raw: overflowed; next output is a fresh snapshot
	dirty  bool // screen: changed since the last frame
	closed *int // exit code once the session ended
	// replaced (guarded by h.mu): a re-attach on the same connection
	// replaces this attachment, so leaving does not change sess.Attached.
	replaced bool

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

func (a *attachment) signal() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// pushOutput queues raw bytes ending at stream position end (h.mu held).
// Past rawQueueLimit the queued bytes are dropped and the client is
// resynchronized with a snapshot.
func (a *attachment) pushOutput(b []byte, end int64) {
	a.mu.Lock()
	if !a.resync {
		if a.queued+len(b) > rawQueueLimit {
			a.dropData()
			a.resync = true
		} else {
			a.items = append(a.items, item{data: b, end: end})
			a.queued += len(b)
		}
	}
	a.mu.Unlock()
	a.signal()
}

// dropData removes queued output but keeps resize notifications, which the
// client needs before the snapshot that replaces the output.
func (a *attachment) dropData() {
	kept := a.items[:0]
	for _, it := range a.items {
		if it.resize != nil {
			kept = append(kept, it)
		}
	}
	clear(a.items[len(kept):])
	a.items = kept
	a.queued = 0
}

func (a *attachment) markDirty() {
	a.mu.Lock()
	a.dirty = true
	a.mu.Unlock()
	a.signal()
}

// pushResize queues a resize notification (h.mu held).
func (a *attachment) pushResize(p *wire.ResizeParams) {
	a.mu.Lock()
	a.items = append(a.items, item{resize: p})
	a.dirty = true
	a.mu.Unlock()
	a.signal()
}

// closeWith queues the final `closed` notification.
func (a *attachment) closeWith(code int) {
	a.mu.Lock()
	a.closed = &code
	a.mu.Unlock()
	a.signal()
}

// detach stops the sender and waits for it, so nothing from this
// attachment follows a later reply on the same connection.
func (a *attachment) detach() {
	a.stopOnce.Do(func() { close(a.stop) })
	<-a.done
}

func (a *attachment) run() {
	defer close(a.done)
	defer func() {
		a.h.mu.Lock()
		delete(a.h.atts, a)
		changed := !a.replaced && a.h.syncAttachedLocked()
		a.h.mu.Unlock()
		if changed {
			a.h.link.wake()
		}
	}()
	interval := time.Second / time.Duration(a.fps)
	var lastFrame time.Time
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	armed := false
	for {
		var tick <-chan time.Time
		if armed {
			tick = timer.C
		}
		select {
		case <-a.stop:
			return
		case <-a.wake:
		case <-tick:
			armed = false
		}

		if a.raw {
			a.mu.Lock()
			resync := a.resync
			a.mu.Unlock()
			if resync {
				a.resnapshot()
			}
		}
		a.mu.Lock()
		items := a.items
		a.items, a.queued = nil, 0
		dirty := a.dirty
		closed := a.closed
		a.mu.Unlock()

		if !a.send(items) {
			return
		}
		if !a.raw {
			for _, it := range items {
				if it.reset { // the initial full frame counts towards the fps cap
					lastFrame = time.Now()
				}
			}
		}
		if !a.raw && dirty {
			if wait := interval - time.Since(lastFrame); wait > 0 && closed == nil {
				if !armed {
					timer.Reset(wait)
					armed = true
				}
			} else {
				a.mu.Lock()
				a.dirty = false
				a.mu.Unlock()
				if f := a.differ.Frame(); f != nil {
					if a.notifyOutput(f, false, 0) != nil {
						return
					}
				}
				lastFrame = time.Now()
			}
		}
		if closed != nil {
			a.mu.Lock()
			pending := len(a.items) > 0
			a.mu.Unlock()
			if pending {
				a.signal() // output raced in after the exit was queued
				continue
			}
			a.conn.Notify(wire.NotifyClosed, wire.ClosedParams{ID: a.h.id, ExitCode: *closed})
			return
		}
	}
}

// resnapshot replaces an overflowed raw queue with a reset snapshot taken
// under h.mu, i.e. exactly at the current position of the byte stream.
func (a *attachment) resnapshot() {
	a.h.mu.Lock()
	snap := a.h.scr.Snapshot()
	a.mu.Lock()
	a.dropData()
	a.items = append(a.items, item{data: snap, reset: true, end: a.h.pos})
	a.resync = false
	a.mu.Unlock()
	a.h.mu.Unlock()
}

// send writes queued items in order, merging consecutive raw chunks into
// one notification that reports the last chunk's end. It reports false when
// the connection failed.
func (a *attachment) send(items []item) bool {
	var buf []byte
	var end int64
	flush := func() bool {
		if len(buf) == 0 {
			return true
		}
		err := a.notifyOutput(buf, false, end)
		buf = nil
		return err == nil
	}
	for _, it := range items {
		switch {
		case it.resize != nil:
			if !flush() || a.conn.Notify(wire.NotifyResize, it.resize) != nil {
				return false
			}
		case it.reset:
			if !flush() || a.notifyOutput(it.data, true, it.end) != nil {
				return false
			}
		default:
			buf = append(buf, it.data...)
			end = it.end
		}
	}
	return flush()
}

func (a *attachment) notifyOutput(data []byte, reset bool, end int64) error {
	return a.conn.Notify(wire.NotifyOutput, wire.OutputParams{ID: a.h.id, Data: data, Reset: reset, End: end})
}
