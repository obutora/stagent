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

// videoStart asks the companion for MJPEG at half the simulator's pixels
// (as for Android) and quality 0.5 (ADR 0008 "通信量と電池", #634), fps 0:
// a frame when the screen is redrawn only.
func videoStart() *idbpb.VideoStreamRequest {
	return &idbpb.VideoStreamRequest{Control: &idbpb.VideoStreamRequest_Start_{Start: &idbpb.VideoStreamRequest_Start{
		Format:             idbpb.VideoStreamRequest_MJPEG,
		Fps:                0,
		CompressionQuality: 0.5,
		ScaleFactor:        0.5,
	}}}
}

// stream shows one simulator through its companion's video_stream
// (MJPEG, #630): every payload is one complete JPEG, sent as is. With fps
// 0 the companion sends a frame when the screen is redrawn only. Pause
// closes the video stream and Resume opens another; the companion keeps
// running meanwhile.
type stream struct {
	comp *companion
	name string
	// The simulator's screen in points, portrait (describe).
	wPt, hPt int
	// hidc is the companion's hid stream, opened by the first input; only
	// Input uses it (see hid).
	hidc idbpb.CompanionService_HidClient
	// Only Input uses these (see text): the simulator's clipboard and
	// files, when no key may be sent before (a ⌘V's banner), the bundle
	// ids of the apps' pids, and the edit menu's labels of Paste.
	host        simHost
	keysAfter   time.Time
	bundles     map[uint64]string
	pasteLabels []string
	// first is the video stream openStream opened (receive's).
	first video

	ctx    context.Context // ends with Close or the stream
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ev      atomic.Pointer[hostscreen.Events] // set by Start
	closing atomic.Bool
	gone    sync.Once

	wake chan struct{} // wakes send
	kick chan struct{} // wakes receive after Resume

	// Guarded by mu, which is never held while calling Events: Pause and
	// Resume are called under the flow's lock, which Events takes.
	mu     sync.Mutex
	stopVS context.CancelFunc // closes the open video stream
	latest *hostscreen.Frame  // the newest frame received
	fresh  bool               // latest was not sent yet
	resend bool               // Resume asked for latest again
	paused bool
}

// video is one video stream; its ctx ends when Pause (or Close) closes
// it.
type video struct {
	vs  idbpb.CompanionService_VideoStreamClient
	ctx context.Context
}

// keepAliveEvery is how often a paused stream asks its companion
// something: well within idleShutdown, after which a companion without a
// request ends by itself.
const keepAliveEvery = idleShutdown * time.Second / 3

// openStream starts the companion's MJPEG video stream; frames wait until
// Start.
func openStream(comp *companion, name string, wPt, hPt int) (*stream, error) {
	s := &stream{comp: comp, name: name, wPt: wPt, hPt: hPt, wake: make(chan struct{}, 1), kick: make(chan struct{}, 1)}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	v, err := s.openVideo()
	if err != nil {
		s.cancel()
		return nil, err
	}
	s.first = v
	return s, nil
}

// openVideo opens a video stream, which Pause closes; while paused, it
// opens none (a zero video).
func (s *stream) openVideo() (video, error) {
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return video{}, nil
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.stopVS = cancel
	s.mu.Unlock()
	vs, err := s.comp.client.VideoStream(ctx)
	if err == nil {
		err = vs.Send(videoStart())
	}
	if err != nil {
		cancel()
		return video{}, err
	}
	return video{vs, ctx}, nil
}

func (s *stream) Start(ev hostscreen.Events) {
	s.ev.Store(&ev)
	s.wg.Go(s.send)
	s.wg.Go(s.receive)
}

func (s *stream) Pause() {
	s.mu.Lock()
	s.paused = true
	stop := s.stopVS
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// Resume sends the newest frame again at once (with fps 0 a still screen
// sends none by itself), then opens another video stream.
func (s *stream) Resume() {
	s.mu.Lock()
	s.paused = false
	s.resend = true
	s.mu.Unlock()
	poke(s.wake)
	poke(s.kick)
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
// down in the simulator. The stream is taken before the wait, as s.hidc
// may change after it (see flushHID).
func (s *stream) closeHID() {
	c := s.hidc
	if c == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		c.CloseAndRecv()
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
// started with --terminate-offline, exits). Between a Pause and a Resume
// it keeps the companion from shutting down idle.
func (s *stream) receive() {
	v := s.first
	for {
		if v.vs != nil {
			s.receiveVideo(v.vs)
			if s.ctx.Err() != nil || v.ctx.Err() == nil {
				s.end()
				return
			}
		}
		// Pause closed it.
		if !s.waitResume() {
			s.end()
			return
		}
		var err error
		if v, err = s.openVideo(); err != nil && !s.isPaused() {
			s.end()
			return
		}
	}
}

// receiveVideo receives the frames of one video stream until it ends.
func (s *stream) receiveVideo(vs idbpb.CompanionService_VideoStreamClient) {
	for {
		r, err := vs.Recv()
		if err != nil {
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

// waitResume waits while paused, asking the companion something every
// keepAliveEvery; false once the stream ends or the companion exits.
func (s *stream) waitResume() bool {
	t := time.NewTicker(keepAliveEvery)
	defer t.Stop()
	for {
		if !s.isPaused() {
			return true
		}
		select {
		case <-s.ctx.Done():
			return false
		case <-s.comp.exited:
			return false
		case <-s.kick:
		case <-t.C:
			ctx, cancel := context.WithTimeout(s.ctx, keepAliveEvery)
			s.comp.client.Describe(ctx, &idbpb.TargetDescriptionRequest{})
			cancel()
		}
	}
}

func (s *stream) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
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
