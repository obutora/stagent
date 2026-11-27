package browser

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// fakePage is the WebSocket of page P1: it answers the stream's calls,
// sends a screencast frame (with metadata, when set) once the screencast
// starts and records the calls it got. Like Chrome, it refuses to start a
// screencast already started.
type fakePage struct {
	mu       sync.Mutex
	methods  []string
	calls    []call
	metadata string
	shot     string               // captureScreenshot's data; "c2hvdA==" when empty
	conn     chan *websocket.Conn // the accepted connection
}

// call is one CDP request a fakePage got.
type call struct {
	Method string
	Params map[string]any
}

func (p *fakePage) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(maxCDPMessage)
	p.conn <- c
	ctx := context.Background()
	casting := false
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			ID     int64          `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		json.Unmarshal(b, &m)
		p.mu.Lock()
		p.methods = append(p.methods, m.Method)
		p.calls = append(p.calls, call{m.Method, m.Params})
		metadata, shot := p.metadata, p.shot
		p.mu.Unlock()
		if shot == "" {
			shot = "c2hvdA=="
		}
		if metadata == "" {
			metadata = `{"deviceWidth":800,"deviceHeight":600,"offsetTop":0,"pageScaleFactor":1}`
		}
		result := `{}`
		switch m.Method {
		case "Target.getTargetInfo":
			result = `{"targetInfo":{"targetId":"P1","type":"page","title":"Example","url":"https://example.com/"}}`
		case "Page.getLayoutMetrics":
			result = `{"cssVisualViewport":{"clientWidth":800,"clientHeight":600.4}}`
		case "Page.captureScreenshot":
			result = `{"data":"` + shot + `"}`
		case "Page.getNavigationHistory":
			result = `{"currentIndex":1,"entries":[{"id":10,"url":"a"},{"id":11,"url":"b"},{"id":12,"url":"c"}]}`
		case "Page.navigate":
			result = `{"frameId":"F"}`
		}
		if m.Method == "Page.startScreencast" && casting {
			c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa64(m.ID)+`,"error":{"code":-32000,"message":"Screencast is already active"}}`))
			continue
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa64(m.ID)+`,"result":`+result+`}`))
		switch m.Method {
		case "Page.stopScreencast":
			casting = false
		case "Page.startScreencast":
			casting = true
			c.Write(ctx, websocket.MessageText, []byte(`{"method":"Page.screencastFrame","params":{"data":"Y2FzdA==","sessionId":7,"metadata":`+metadata+`}}`))
		}
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (p *fakePage) got(method string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.methods {
		if m == method {
			return true
		}
	}
	return false
}

// events records what a stream reports.
type events struct{ ch chan string }

// Frame records the JPEG in base64, as CDP sent it.
func (e events) Frame(f hostscreen.Frame) {
	e.ch <- "frame:" + base64.StdEncoding.EncodeToString(f.JPEG) + ":" + itoa64(int64(f.Width)) + "x" + itoa64(int64(f.Height))
}
func (e events) State(s wire.HostScreenState) {
	e.ch <- "state:" + s.State + ":" + s.Title + ":" + s.URL
}

func next(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return ""
	}
}

func startPage(t *testing.T) (*Kind, int, *fakePage) {
	t.Helper()
	page := &fakePage{conn: make(chan *websocket.Conn, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/devtools/page/P1", page.serve)
	return New(), cdpServer(t, mux), page
}

func TestStreamSendsLiveAFullFrameThenTheScreencastAndGoneWhenThePageCloses(t *testing.T) {
	k, port, page := startPage(t)
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := events{make(chan string, 16)}
	st.Start(ev)
	for _, want := range []string{
		"state:live:Example:https://example.com/",
		"frame:c2hvdA==:800x600", // captureScreenshot, CSS px
		"frame:Y2FzdA==:800x600", // the screencast
	} {
		if got := next(t, ev.ch); got != want {
			t.Fatalf("event %q, want %q", got, want)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for !page.got("Page.screencastFrameAck") {
		if time.Now().After(deadline) {
			t.Fatal("Chrome's screencast frame was not acked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The page target closes: Chrome detaches and drops the WebSocket.
	c := <-page.conn
	c.Write(context.Background(), websocket.MessageText, []byte(`{"method":"Inspector.detached","params":{"reason":"target_closed"}}`))
	c.CloseNow()
	if got := next(t, ev.ch); got != "state:gone::" {
		t.Fatalf("after the page closed: %q, want gone", got)
	}
}

func TestPageClosedBeforeStartIsReportedGone(t *testing.T) {
	k, port, page := startPage(t)
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The page closes after Open and before Start.
	(<-page.conn).CloseNow()
	select {
	case <-st.(*stream).ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not see the page close")
	}
	ev := events{make(chan string, 16)}
	st.Start(ev)
	if got := next(t, ev.ch); got != "state:gone::" {
		t.Fatalf("Start after the page closed: %q, want gone", got)
	}
}

func TestClosedStreamReportsNothing(t *testing.T) {
	k, port, page := startPage(t)
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	ev := events{make(chan string, 16)}
	st.Start(ev)
	next(t, ev.ch) // live
	st.Close()
	(<-page.conn).CloseNow()
	for {
		select {
		case got := <-ev.ch:
			if got == "state:gone::" {
				t.Fatal("gone after Close")
			}
		case <-time.After(200 * time.Millisecond):
			return
		}
	}
}

func TestOpenFindsOnlyPageTargets(t *testing.T) {
	k, port, _ := startPage(t)
	var we *wire.Error
	for _, tc := range []struct {
		target wire.HostScreenTarget
		code   string
	}{
		{wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "W1"}, wire.ErrNotFound}, // a worker
		{wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "nope"}, wire.ErrNotFound},
		{wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: 70000, TargetID: "P1"}, wire.ErrBadRequest},
		{wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port}, wire.ErrBadRequest},
	} {
		if _, err := k.Open(t.Context(), tc.target); !errors.As(err, &we) || we.Code != tc.code {
			t.Errorf("open %+v: %v, want %s", tc.target, err, tc.code)
		}
	}
}

// methodsSince is the methods the page got after its first n.
func (p *fakePage) methodsSince(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.methods[n:]...)
}

func (p *fakePage) count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.methods) }

// waitMethods waits until the capture methods the page got after its
// first n are want, in order.
func (p *fakePage) waitMethods(t *testing.T, n int, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var got []string
		for _, m := range p.methodsSince(n) {
			if m == "Page.stopScreencast" || m == "Page.captureScreenshot" || m == "Page.startScreencast" {
				got = append(got, m)
			}
		}
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("page got %q, want %q", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPauseStopsTheScreencastAndResumeTakesAShotThenStartsIt(t *testing.T) {
	k, port, page := startPage(t)
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := events{make(chan string, 16)}
	st.Start(ev)
	for range 3 { // live, the shot, the screencast's frame
		next(t, ev.ch)
	}
	n := page.count()
	st.Pause()
	page.waitMethods(t, n, "Page.stopScreencast")
	n = page.count()
	st.Resume()
	page.waitMethods(t, n, "Page.captureScreenshot", "Page.startScreencast")
	for _, want := range []string{"frame:c2hvdA==:800x600", "frame:Y2FzdA==:800x600"} {
		if got := next(t, ev.ch); got != want {
			t.Fatalf("after resume %q, want %q", got, want)
		}
	}
}

// lastCall is the params of the last call of method the page got.
func (p *fakePage) lastCall(method string) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.calls) - 1; i >= 0; i-- {
		if p.calls[i].Method == method {
			return p.calls[i].Params
		}
	}
	return nil
}

// A quality asked restarts the screencast at it (at most half the size of
// the last screenshot, each way, for a half scale); a settle restarts it
// at normal quality, then takes a screenshot at normal quality.
func TestQualityRestartsTheScreencastAndSettleTakesANormalShot(t *testing.T) {
	k, port, page := startPage(t)
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1600, 1200)), nil)
	shot := base64.StdEncoding.EncodeToString(buf.Bytes())
	page.shot = shot
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := events{make(chan string, 16)}
	st.Start(ev)
	for range 3 { // live, the shot, the screencast's frame
		next(t, ev.ch)
	}
	if q := page.lastCall("Page.captureScreenshot")["quality"]; q != 70.0 {
		t.Fatalf("first shot at quality %v, want 70", q)
	}
	qs := st.(hostscreen.QualitySetter)
	n := page.count()
	qs.SetQuality(hostscreen.Quality{JPEG: 30, Scale: 0.5}, false)
	page.waitMethods(t, n, "Page.stopScreencast", "Page.startScreencast")
	if got := next(t, ev.ch); got != "frame:Y2FzdA==:800x600" {
		t.Fatalf("after the quality %q, want the screencast's frame", got)
	}
	p := page.lastCall("Page.startScreencast")
	if p["quality"] != 30.0 || p["maxWidth"] != 800.0 || p["maxHeight"] != 600.0 {
		t.Fatalf("screencast restarted with %v, want quality 30 within 800x600", p)
	}
	n = page.count()
	qs.SetQuality(hostscreen.Normal, true)
	page.waitMethods(t, n, "Page.stopScreencast", "Page.startScreencast", "Page.captureScreenshot")
	for _, want := range []string{"frame:Y2FzdA==:800x600", "frame:" + shot + ":800x600"} {
		if got := next(t, ev.ch); got != want {
			t.Fatalf("after the settle %.40q, want %.40q", got, want)
		}
	}
	p = page.lastCall("Page.startScreencast")
	if _, ok := p["maxWidth"]; p["quality"] != 70.0 || ok {
		t.Fatalf("screencast restarted with %v, want quality 70 at full size", p)
	}
	if q := page.lastCall("Page.captureScreenshot")["quality"]; q != 70.0 {
		t.Fatalf("settle shot at quality %v, want 70", q)
	}
}

// A settle while paused takes no shot; Resume takes one at the quality.
func TestSettleWhilePausedWaitsForResume(t *testing.T) {
	k, port, page := startPage(t)
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := events{make(chan string, 16)}
	st.Start(ev)
	for range 3 {
		next(t, ev.ch)
	}
	qs := st.(hostscreen.QualitySetter)
	qs.SetQuality(hostscreen.Quality{JPEG: 40, Scale: 1}, false)
	next(t, ev.ch) // the screencast restarted
	n := page.count()
	st.Pause()
	qs.SetQuality(hostscreen.Normal, true)
	time.Sleep(100 * time.Millisecond)
	page.waitMethods(t, n, "Page.stopScreencast")
	st.Resume()
	page.waitMethods(t, n, "Page.stopScreencast", "Page.captureScreenshot", "Page.startScreencast")
	if q := page.lastCall("Page.captureScreenshot")["quality"]; q != 70.0 {
		t.Fatalf("shot after resume at quality %v, want 70", q)
	}
	if q := page.lastCall("Page.startScreencast")["quality"]; q != 70.0 {
		t.Fatalf("screencast after resume at quality %v, want 70", q)
	}
}
