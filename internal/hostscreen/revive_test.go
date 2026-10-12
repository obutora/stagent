package hostscreen

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// recorder is the Events a reviving stream reports to.
type recorder struct{ c chan string }

func newRecorder() *recorder { return &recorder{c: make(chan string, 16)} }

func (r *recorder) Frame(f Frame)                { r.c <- "frame:" + f.Data }
func (r *recorder) State(s wire.HostScreenState) { r.c <- "state:" + s.State }

func (r *recorder) next(t *testing.T) string {
	t.Helper()
	select {
	case e := <-r.c:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return ""
}

func (r *recorder) none(t *testing.T) {
	t.Helper()
	select {
	case e := <-r.c:
		t.Fatalf("unexpected %s", e)
	case <-time.After(50 * time.Millisecond):
	}
}

// eventually waits until cond holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(time.Millisecond)
	}
}

// target is what reopen finds: nil while the target is stopped.
type target struct {
	mu     sync.Mutex
	next   *fakeStream
	looked atomic.Int32
}

func (tg *target) reopen(context.Context) Stream {
	tg.looked.Add(1)
	tg.mu.Lock()
	defer tg.mu.Unlock()
	if tg.next == nil {
		return nil
	}
	s := tg.next
	tg.next = nil
	return s
}

func (tg *target) start(s *fakeStream) {
	tg.mu.Lock()
	tg.next = s
	tg.mu.Unlock()
}

func (s *fakeStream) isClosed() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.closed }
func (s *fakeStream) events() Events { s.mu.Lock(); defer s.mu.Unlock(); return s.ev }

func TestRevivingShowsTheTargetAgainWhenItComesBack(t *testing.T) {
	first := &fakeStream{}
	tg := &target{}
	r := Reviving(first, time.Millisecond, tg.reopen)
	rec := newRecorder()
	r.Start(rec)
	first.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	first.frame("a")
	if got := []string{rec.next(t), rec.next(t)}; !slices.Equal(got, []string{"state:live", "frame:a"}) {
		t.Fatalf("got %q", got)
	}
	if tg.looked.Load() != 0 {
		t.Fatal("looked for a target that is live")
	}
	first.ev.State(wire.HostScreenState{State: wire.HostScreenGone})
	if got := rec.next(t); got != "state:gone" {
		t.Fatalf("got %s, want gone", got)
	}
	eventually(t, "the gone stream was not closed", first.isClosed)
	eventually(t, "not looked for again", func() bool { return tg.looked.Load() >= 3 })
	first.frame("stale")
	rec.none(t)

	second := &fakeStream{}
	tg.start(second)
	eventually(t, "the target found again was not started", func() bool { return second.events() != nil })
	second.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	second.frame("b")
	if got := []string{rec.next(t), rec.next(t)}; !slices.Equal(got, []string{"state:live", "frame:b"}) {
		t.Fatalf("got %q", got)
	}
	looked := tg.looked.Load()
	r.Close()
	if !second.isClosed() {
		t.Fatal("Close left the stream open")
	}
	time.Sleep(20 * time.Millisecond)
	if tg.looked.Load() != looked {
		t.Fatal("looked for the target while it was live or after Close")
	}
}

func TestRevivingWithoutTheTargetStartsGone(t *testing.T) {
	tg := &target{}
	r := Reviving(nil, time.Millisecond, tg.reopen)
	defer r.Close()
	rec := newRecorder()
	r.Start(rec)
	if got := rec.next(t); got != "state:gone" {
		t.Fatalf("got %s, want gone at once", got)
	}
	var we *wire.Error
	if _, err := r.(Inputter).Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap}); !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
		t.Fatalf("input while gone: %v, want bad_request", err)
	}
	s := &fakeStream{}
	tg.start(s)
	eventually(t, "the target was not started", func() bool { return s.events() != nil })
	s.ev.State(wire.HostScreenState{State: wire.HostScreenLive})
	if got := rec.next(t); got != "state:live" {
		t.Fatalf("got %s, want live", got)
	}
	r.Pause()
	r.Resume()
	if got, want := s.callList(), []string{"start", "pause", "resume"}; !slices.Equal(got, want) {
		t.Fatalf("stream calls %q, want %q", got, want)
	}
}

func TestRevivingInputGoesToTheCurrentStream(t *testing.T) {
	in := &inputStream{fakeStream: &fakeStream{}}
	r := Reviving(in, time.Millisecond, func(context.Context) Stream { return nil })
	defer r.Close()
	r.Start(newRecorder())
	tap := wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.5}
	if _, err := r.(Inputter).Input(t.Context(), tap); err != nil || len(in.inputs) != 1 {
		t.Fatalf("input: %v, handed %d", err, len(in.inputs))
	}
	plain := &fakeStream{}
	r2 := Reviving(plain, time.Millisecond, func(context.Context) Stream { return nil })
	defer r2.Close()
	r2.Start(newRecorder())
	var we *wire.Error
	if _, err := r2.(Inputter).Input(t.Context(), tap); !errors.As(err, &we) || we.Code != wire.ErrUnsupported {
		t.Fatalf("input to a stream without inputs: %v, want unsupported", err)
	}
}

func TestRevivingCloseWhileGoneStopsLooking(t *testing.T) {
	found := &fakeStream{}
	var looked atomic.Int32
	// The target comes back just as the stream is closed.
	r := Reviving(nil, time.Millisecond, func(ctx context.Context) Stream {
		looked.Add(1)
		<-ctx.Done()
		return found
	})
	r.Start(newRecorder())
	eventually(t, "not looked for", func() bool { return looked.Load() == 1 })
	r.Close()
	if !found.isClosed() {
		t.Fatal("a stream opened as Close came was left open")
	}
	time.Sleep(20 * time.Millisecond)
	if n := looked.Load(); n != 1 {
		t.Fatalf("looked %d times, want once: no looking after Close", n)
	}
}

func TestRevivingClosedBeforeStart(t *testing.T) {
	s := &fakeStream{}
	Reviving(s, time.Millisecond, func(context.Context) Stream { return nil }).Close()
	if !s.isClosed() {
		t.Fatal("Close left the stream open")
	}
}
