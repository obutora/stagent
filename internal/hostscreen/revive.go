package hostscreen

import (
	"context"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// WatchEvery is how often a target that is gone is looked for again
// (#633).
const WatchEvery = 3 * time.Second

// reviving is a Stream of a target that may stop and start again: an
// Android emulator, an iOS simulator.
type reviving struct {
	every  time.Duration
	reopen func(context.Context) Stream

	ctx    context.Context // ends with Close
	cancel context.CancelFunc
	ended  chan struct{} // wakes watch: the current stream is gone
	done   chan struct{} // closed when watch returns

	// inputMu is held by Input and while watch closes a gone stream: a
	// Stream is never closed during its Input.
	inputMu sync.Mutex

	mu   sync.Mutex // never held while calling a Stream or Events
	ev   Events
	cur  Stream // the stream started last; nil while none is open
	gone bool   // cur reported gone (or there is none)
}

// Reviving shows a target that may stop and start again (#633): first,
// then, once that reports gone, whatever reopen opens. While the target
// is gone, reopen is called every interval until it returns a stream (nil:
// not back yet), which is started: its live tells the app the target is
// back. With first nil the target is not running yet: Start reports gone
// and looks for it the same way. reopen ends when its ctx does; a stream
// it returns then is closed. Close stops looking and closes the stream.
func Reviving(first Stream, every time.Duration, reopen func(ctx context.Context) Stream) Stream {
	r := &reviving{
		every:  every,
		reopen: reopen,
		ended:  make(chan struct{}, 1),
		done:   make(chan struct{}),
		cur:    first,
		gone:   first == nil,
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	go r.watch()
	return r
}

func (r *reviving) Start(ev Events) {
	r.mu.Lock()
	r.ev = ev
	cur := r.cur
	r.mu.Unlock()
	if cur == nil {
		ev.State(wire.HostScreenState{State: wire.HostScreenGone})
		wake(r.ended)
		return
	}
	cur.Start(revived{r, cur})
}

func (r *reviving) live() Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gone {
		return nil
	}
	return r.cur
}

func (r *reviving) Pause() {
	if s := r.live(); s != nil {
		s.Pause()
	}
}

func (r *reviving) Resume() {
	if s := r.live(); s != nil {
		s.Resume()
	}
}

func (r *reviving) Input(ctx context.Context, in wire.HostScreenInput) error {
	r.inputMu.Lock()
	defer r.inputMu.Unlock()
	s := r.live()
	if s == nil {
		return wire.Errorf(wire.ErrBadRequest, "host_screen.input: the target is gone")
	}
	is, ok := s.(Inputter)
	if !ok {
		return wire.Errorf(wire.ErrUnsupported, "host_screen.input: this kind of target takes no input")
	}
	return is.Input(ctx, in)
}

func (r *reviving) Close() {
	r.cancel()
	<-r.done
	r.mu.Lock()
	cur := r.cur
	r.cur = nil
	r.mu.Unlock()
	if cur != nil {
		cur.Close()
	}
}

// watch closes the stream that went gone, then looks for the target every
// interval and starts the stream reopen opens, until Close.
func (r *reviving) watch() {
	defer close(r.done)
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-r.ended:
		}
		r.mu.Lock()
		old := r.cur
		r.cur = nil
		r.mu.Unlock()
		if old != nil {
			r.inputMu.Lock()
			old.Close()
			r.inputMu.Unlock()
		}
		next := r.look()
		if next == nil {
			return
		}
		r.mu.Lock()
		r.cur, r.gone = next, false
		r.mu.Unlock()
		next.Start(revived{r, next})
	}
}

// look calls reopen every interval until it opens a stream; nil once
// closed.
func (r *reviving) look() Stream {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		if s := r.reopen(r.ctx); s != nil {
			if r.ctx.Err() != nil {
				s.Close()
				return nil
			}
			return s
		}
		select {
		case <-r.ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// revived is the Events of one stream of a reviving: what it reports
// passes while it is the current stream and not gone.
type revived struct {
	r *reviving
	s Stream
}

func (e revived) events() Events {
	e.r.mu.Lock()
	defer e.r.mu.Unlock()
	if e.r.cur != e.s || e.r.gone {
		return nil
	}
	return e.r.ev
}

func (e revived) Frame(f Frame) {
	if ev := e.events(); ev != nil {
		ev.Frame(f)
	}
}

func (e revived) State(st wire.HostScreenState) {
	ev := e.events()
	if ev == nil {
		return
	}
	if st.State == wire.HostScreenGone {
		e.r.mu.Lock()
		e.r.gone = true
		e.r.mu.Unlock()
		ev.State(st)
		wake(e.r.ended)
		return
	}
	ev.State(st)
}

var _ Inputter = (*reviving)(nil)
