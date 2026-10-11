package browser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// fakePage is the WebSocket of page P1: it answers the stream's calls,
// sends a screencast frame (with metadata, when set) once the screencast
// starts and records the calls it got.
type fakePage struct {
	mu       sync.Mutex
	methods  []string
	calls    []call
	metadata string
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
		metadata := p.metadata
		p.mu.Unlock()
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
			result = `{"data":"c2hvdA=="}`
		case "Page.getNavigationHistory":
			result = `{"currentIndex":1,"entries":[{"id":10,"url":"a"},{"id":11,"url":"b"},{"id":12,"url":"c"}]}`
		case "Page.navigate":
			result = `{"frameId":"F"}`
		}
		c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa64(m.ID)+`,"result":`+result+`}`))
		if m.Method == "Page.startScreencast" {
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

func (e events) Frame(f hostscreen.Frame) {
	e.ch <- "frame:" + f.Data + ":" + itoa64(int64(f.Width)) + "x" + itoa64(int64(f.Height))
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
