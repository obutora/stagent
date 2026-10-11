package ios

import (
	"bytes"
	"context"
	"encoding/base64"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// scale and quality of the companion's MJPEG: half the simulator's
	// pixels (as for Android) at the provisional quality 70 (#624) until
	// #634 sets the limits.
	scale   = 0.5
	quality = 0.7
)

// stream shows one simulator through its companion's video_stream
// (MJPEG, #630): every payload is one complete JPEG, sent as is. With fps
// 0 the companion sends a frame when the screen is redrawn only.
type stream struct {
	comp *companion
	vs   idbpb.CompanionService_VideoStreamClient
	name string
	// The simulator's screen in points, portrait (describe).
	wPt, hPt int
	// hidc is the companion's hid stream, opened by the first input; only
	// Input uses it (see hid).
	hidc idbpb.CompanionService_HidClient

	ctx    context.Context // ends with Close or the stream
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ev      atomic.Pointer[hostscreen.Events] // set by Start
	closing atomic.Bool
	gone    sync.Once

	wake chan struct{} // wakes send

	// Guarded by mu, which is never held while calling Events: Pause and
	// Resume are called under the flow's lock, which Events takes.
	mu     sync.Mutex
	latest *hostscreen.Frame // the newest frame received
	fresh  bool              // latest was not sent yet
	resend bool              // Resume asked for latest again
	paused bool
}

// openStream starts the companion's MJPEG video stream; frames wait until
// Start.
func openStream(comp *companion, name string, wPt, hPt int) (*stream, error) {
	s := &stream{comp: comp, name: name, wPt: wPt, hPt: hPt, wake: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	vs, err := comp.client.VideoStream(s.ctx)
	if err == nil {
		err = vs.Send(&idbpb.VideoStreamRequest{Control: &idbpb.VideoStreamRequest_Start_{Start: &idbpb.VideoStreamRequest_Start{
			Format:             idbpb.VideoStreamRequest_MJPEG,
			CompressionQuality: quality,
			ScaleFactor:        scale,
		}}})
	}
	if err != nil {
		s.cancel()
		return nil, err
	}
	s.vs = vs
	return s, nil
}

func (s *stream) Start(ev hostscreen.Events) {
	s.ev.Store(&ev)
	s.wg.Go(s.send)
	s.wg.Go(s.receive)
}

func (s *stream) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
}

// Resume sends the newest frame again: with fps 0 a still screen sends
// none by itself. The video stream stays open while paused, which also
// keeps the companion from shutting down idle.
func (s *stream) Resume() {
	s.mu.Lock()
	s.paused = false
	s.resend = true
	s.mu.Unlock()
	poke(s.wake)
}

// Close ends the stream after the companion performed the inputs sent
// (see closeHID), then stops it. Never called during an Input.
func (s *stream) Close() {
	s.closing.Store(true)
	s.closeHID()
	s.cancel()
	s.comp.stop()
	s.wg.Wait()
}

// closeHID ends the hid stream and waits, at most hidCloseWait, until the
// companion performed every event sent on it: the lift of a drag cut
// short is not lost to stopping the companion, which would leave a touch
// down in the simulator.
func (s *stream) closeHID() {
	if s.hidc == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		s.hidc.CloseAndRecv()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hidCloseWait):
	}
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// receive turns the companion's payloads into frames until the stream
// ends: with Close, or when the simulator shuts down (the companion,
// started with --terminate-offline, exits).
func (s *stream) receive() {
	for {
		r, err := s.vs.Recv()
		if err != nil {
			s.end()
			return
		}
		data := r.GetPayload().GetData()
		if data == nil {
			continue // log_output
		}
		f, ok := frameOf(data, s.wPt, s.hPt)
		if !ok {
			continue
		}
		s.mu.Lock()
		s.latest, s.fresh = &f, true
		s.mu.Unlock()
		poke(s.wake)
	}
}

// send makes every Events call but the gone state: State live, then the
// frames in the order received.
func (s *stream) send() {
	ev := *s.ev.Load()
	ev.State(wire.HostScreenState{State: wire.HostScreenLive, Title: s.name})
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		s.mu.Lock()
		f := s.latest
		due := f != nil && !s.paused && (s.fresh || s.resend)
		if due {
			s.fresh, s.resend = false, false
		}
		s.mu.Unlock()
		if due && s.ctx.Err() == nil {
			ev.Frame(*f)
		}
	}
}

// end reports the simulator gone, once, unless the stream is being closed,
// and stops its companion.
func (s *stream) end() {
	s.gone.Do(func() {
		s.cancel()
		if s.closing.Load() {
			return
		}
		if ev := s.ev.Load(); ev != nil {
			(*ev).State(wire.HostScreenState{State: wire.HostScreenGone})
		}
		s.comp.stop()
	})
}

// frameSize is the size of the frame last received (pt, turned like the
// picture): inputs' fractions are of it.
func (s *stream) frameSize() [2]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil {
		return [2]int{s.wPt, s.hPt}
	}
	return [2]int{s.latest.Width, s.latest.Height}
}

// frameOf is the frame of one MJPEG payload. Its size is the simulator's
// screen in points (wPt × hPt, portrait), turned like the picture: the
// payload is scaled, and its pixels are not points.
func frameOf(jpg []byte, wPt, hPt int) (hostscreen.Frame, bool) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
	if err != nil || cfg.Width == 0 || cfg.Height == 0 {
		return hostscreen.Frame{}, false
	}
	w, h := wPt, hPt
	if (cfg.Width > cfg.Height) != (w > h) {
		w, h = h, w
	}
	return hostscreen.Frame{Data: base64.StdEncoding.EncodeToString(jpg), Width: w, Height: h}, true
}
