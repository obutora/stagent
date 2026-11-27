package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// callWait bounds one CDP call of the stream.
	callWait = 10 * time.Second
	// infoEvery bounds how often a screencast frame makes the stream ask
	// for the page's title, which a script may change without a
	// navigation (Chrome sends no event for it to a page session).
	infoEvery = time.Second
)

// stream shows one page target: a captureScreenshot first, then the
// screencast. Chrome's screencast frames are acked at once; pacing to the
// app is hostscreen's.
type stream struct {
	c      *cdpConn
	ctx    context.Context // ends with Close or the page
	cancel context.CancelFunc
	wg     sync.WaitGroup

	ev      atomic.Pointer[hostscreen.Events] // set by Start
	closing atomic.Bool
	gone    sync.Once

	mu      sync.Mutex
	paused  bool
	resume  int                // count of Resume calls
	geom    geometry           // of the latest frame, for inputs
	q       hostscreen.Quality // of the frames (SetQuality)
	settles int                // count of SetQuality settles not paused

	kick chan struct{} // wakes run after Pause, Resume
	info chan struct{} // asks run for the title and URL
	// The title and URL last sent with State live (run only).
	sentTitle, sentURL string

	lastInfo time.Time // read goroutine only
}

// openStream connects to a page's WebSocket and enables the events the
// stream follows.
func openStream(ctx context.Context, wsURL string) (*stream, error) {
	s := &stream{kick: make(chan struct{}, 1), info: make(chan struct{}, 1), q: hostscreen.Normal}
	c, err := dialCDP(ctx, wsURL, s.onEvent)
	if err != nil {
		return nil, err
	}
	s.c = c
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.wg.Go(func() {
		c.readLoop()
		s.end()
	})
	for _, m := range []string{"Page.enable", "Inspector.enable"} {
		if err := c.call(ctx, m, nil, nil); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *stream) Start(ev hostscreen.Events) {
	s.ev.Store(&ev)
	// end cancels before it loads ev, so a page that closed before the
	// Store is seen here; both may send gone, and flow drops the second.
	if s.ctx.Err() != nil && !s.closing.Load() {
		ev.State(wire.HostScreenState{State: wire.HostScreenGone})
		return
	}
	s.wg.Go(s.run)
}

func (s *stream) Pause() {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
	poke(s.kick)
}

func (s *stream) Resume() {
	s.mu.Lock()
	s.paused = false
	s.resume++
	s.mu.Unlock()
	poke(s.kick)
}

// SetQuality restarts the screencast at q; a settle also takes a
// screenshot at q (see run).
func (s *stream) SetQuality(q hostscreen.Quality, settle bool) {
	s.mu.Lock()
	s.q = q
	if settle && !s.paused {
		s.settles++
	}
	s.mu.Unlock()
	poke(s.kick)
}

func (s *stream) Close() {
	s.closing.Store(true)
	s.cancel()
	s.c.close()
	s.wg.Wait()
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// end reports the page gone, once, unless the stream is being closed.
func (s *stream) end() {
	s.gone.Do(func() {
		s.cancel()
		if s.closing.Load() {
			return
		}
		if ev := s.ev.Load(); ev != nil {
			(*ev).State(wire.HostScreenState{State: wire.HostScreenGone})
		}
		s.c.close()
	})
}

// run captures: State live, a full frame, the screencast; after Pause it
// stops the screencast and after Resume starts over with a full frame.
// A new quality restarts the screencast at it (Chrome refuses
// startScreencast while one runs: "Screencast is already active"); a
// settle then takes a full frame at it, after the restart, so no frame of
// the old quality follows it but one Chrome encoded already.
func (s *stream) run() {
	s.sendInfo(true)
	running := false
	var cast hostscreen.Quality // of the running screencast
	fullW, fullH := 0, 0        // the last screenshot's image, in px
	seen, settled := 0, 0       // Resume calls and settles handled
	for {
		s.mu.Lock()
		paused, resume, settles, q := s.paused, s.resume, s.settles, s.q
		s.mu.Unlock()
		switch {
		case paused && running:
			s.call("Page.stopScreencast", nil, nil)
			running = false
		case !paused:
			shot := !running || resume != seen || settles != settled
			seen, settled = resume, settles
			if running && cast != q {
				s.call("Page.stopScreencast", nil, nil)
				s.startScreencast(q, fullW, fullH)
				cast = q
			}
			if shot {
				if w, h, ok := s.capture(q); ok {
					fullW, fullH = w, h
				}
			}
			if !running {
				s.startScreencast(q, fullW, fullH)
				cast, running = q, true
			}
		}
		select {
		case <-s.ctx.Done():
			return
		case <-s.kick:
		case <-s.info:
			s.sendInfo(false)
		}
	}
}

func (s *stream) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(s.ctx, callWait)
	defer cancel()
	return s.c.call(ctx, method, params, result)
}

// startScreencast starts the screencast at q: a half scale bounds its
// frames to half of w×h (the last screenshot's image), when known.
func (s *stream) startScreencast(q hostscreen.Quality, w, h int) {
	p := map[string]any{"format": "jpeg", "quality": q.JPEG}
	if q.Scale < 1 && w > 0 && h > 0 {
		p["maxWidth"], p["maxHeight"] = round(float64(w)*q.Scale), round(float64(h)*q.Scale)
	}
	s.call("Page.startScreencast", p, nil)
}

// capture sends one screenshot of the viewport at q's JPEG quality (in
// full: Scale applies to the screencast) and returns its image's size in
// px, when it has one.
func (s *stream) capture(q hostscreen.Quality) (w, h int, ok bool) {
	var m struct {
		CSSVisualViewport, VisualViewport *struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
			Scale        float64 `json:"scale"`
		}
	}
	if s.call("Page.getLayoutMetrics", nil, &m) != nil {
		return 0, 0, false
	}
	vp := m.CSSVisualViewport
	if vp == nil {
		vp = m.VisualViewport
	}
	if vp == nil {
		return 0, 0, false
	}
	var shot struct {
		Data []byte `json:"data"` // base64 in CDP's JSON
	}
	if s.call("Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": q.JPEG}, &shot) != nil {
		return 0, 0, false
	}
	// The visual viewport in CSS px: Scale DIP per px.
	scale := vp.Scale
	if scale <= 0 {
		scale = 1
	}
	s.setGeometry(geometry{Width: vp.ClientWidth * scale, Height: vp.ClientHeight * scale, Scale: scale})
	s.emit(hostscreen.Frame{JPEG: shot.Data, Width: round(vp.ClientWidth), Height: round(vp.ClientHeight)})
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(shot.Data))
	return cfg.Width, cfg.Height, err == nil
}

// sendInfo sends State live with the page's title and URL when they
// changed (or always when force).
func (s *stream) sendInfo(force bool) {
	var r struct {
		TargetInfo struct {
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"targetInfo"`
	}
	err := s.call("Target.getTargetInfo", nil, &r)
	if err != nil && !force {
		return
	}
	st := wire.HostScreenState{State: wire.HostScreenLive, Title: r.TargetInfo.Title, URL: r.TargetInfo.URL}
	changed := force || st.Title != s.sentTitle || st.URL != s.sentURL
	s.sentTitle, s.sentURL = st.Title, st.URL
	if changed {
		if ev := s.ev.Load(); ev != nil && s.ctx.Err() == nil {
			(*ev).State(st)
		}
	}
}

func (s *stream) emit(f hostscreen.Frame) {
	if ev := s.ev.Load(); ev != nil && s.ctx.Err() == nil {
		(*ev).Frame(f)
	}
}

// onEvent handles the page's CDP events on the read goroutine.
func (s *stream) onEvent(method string, params json.RawMessage) {
	switch method {
	case "Page.screencastFrame":
		var p struct {
			Data      []byte `json:"data"` // base64 in CDP's JSON
			SessionID int64  `json:"sessionId"`
			Metadata  struct {
				DeviceWidth     float64 `json:"deviceWidth"`
				DeviceHeight    float64 `json:"deviceHeight"`
				OffsetTop       float64 `json:"offsetTop"`
				PageScaleFactor float64 `json:"pageScaleFactor"`
			} `json:"metadata"`
		}
		if json.Unmarshal(params, &p) != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.ctx, callWait)
		s.c.send(ctx, "Page.screencastFrameAck", map[string]int64{"sessionId": p.SessionID})
		cancel()
		md := p.Metadata
		s.setGeometry(geometry{Width: md.DeviceWidth, Height: md.DeviceHeight, OffsetTop: md.OffsetTop, Scale: md.PageScaleFactor})
		s.emit(hostscreen.Frame{JPEG: p.Data, Width: round(md.DeviceWidth), Height: round(md.DeviceHeight)})
		if time.Since(s.lastInfo) >= infoEvery {
			s.lastInfo = time.Now()
			poke(s.info)
		}
	case "Page.frameNavigated", "Page.navigatedWithinDocument", "Page.domContentEventFired", "Page.loadEventFired":
		poke(s.info)
	case "Inspector.detached":
		s.end()
	}
}

func round(f float64) int { return int(math.Round(f)) }
