package browser

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// jpegQuality of screenshots and screencast frames (#624).
	jpegQuality = 70
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

	mu     sync.Mutex
	paused bool
	resume int      // count of Resume calls
	geom   geometry // of the latest frame, for inputs

	kick chan struct{} // wakes run after Pause, Resume
	info chan struct{} // asks run for the title and URL
	// The title and URL last sent with State live (run only).
	sentTitle, sentURL string

	lastInfo time.Time // read goroutine only
}

// openStream connects to a page's WebSocket and enables the events the
// stream follows.
func openStream(ctx context.Context, wsURL string) (*stream, error) {
	s := &stream{kick: make(chan struct{}, 1), info: make(chan struct{}, 1)}
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
func (s *stream) run() {
	s.sendInfo(true)
	running := false
	seen := 0 // Resume calls handled
	for {
		s.mu.Lock()
		paused, resume := s.paused, s.resume
		s.mu.Unlock()
		switch {
		case paused && running:
			s.call("Page.stopScreencast", nil, nil)
			running = false
		case !paused && (!running || resume != seen):
			seen = resume
			s.capture()
			if !running {
				s.call("Page.startScreencast", map[string]any{"format": "jpeg", "quality": jpegQuality}, nil)
				running = true
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

// capture sends one screenshot of the viewport.
func (s *stream) capture() {
	var m struct {
		CSSVisualViewport, VisualViewport *struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
			Scale        float64 `json:"scale"`
		}
	}
	if s.call("Page.getLayoutMetrics", nil, &m) != nil {
		return
	}
	vp := m.CSSVisualViewport
	if vp == nil {
		vp = m.VisualViewport
	}
	if vp == nil {
		return
	}
	var shot struct {
		Data string `json:"data"`
	}
	if s.call("Page.captureScreenshot", map[string]any{"format": "jpeg", "quality": jpegQuality}, &shot) != nil {
		return
	}
	// The visual viewport in CSS px: Scale DIP per px.
	scale := vp.Scale
	if scale <= 0 {
		scale = 1
	}
	s.setGeometry(geometry{Width: vp.ClientWidth * scale, Height: vp.ClientHeight * scale, Scale: scale})
	s.emit(hostscreen.Frame{Data: shot.Data, Width: round(vp.ClientWidth), Height: round(vp.ClientHeight)})
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
			Data      string `json:"data"`
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
		s.emit(hostscreen.Frame{Data: p.Data, Width: round(md.DeviceWidth), Height: round(md.DeviceHeight)})
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
