package browser

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// liveStream opens P1 of a page whose screencast frame has metadata and
// waits for that frame, so inputs use its geometry.
func liveStream(t *testing.T, metadata string) (*stream, *fakePage) {
	t.Helper()
	k, port, page := startPage(t)
	page.metadata = metadata
	st, err := k.Open(t.Context(), wire.HostScreenTarget{Kind: wire.HostScreenBrowser, Port: port, TargetID: "P1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	ev := events{make(chan string, 64)}
	st.Start(ev)
	for !strings.HasPrefix(next(t, ev.ch), "frame:Y2FzdA==") {
	}
	return st.(*stream), page
}

// streamOwn are the calls a stream makes to show the page, apart from
// inputs.
var streamOwn = map[string]bool{
	"Page.enable": true, "Inspector.enable": true, "Target.getTargetInfo": true,
	"Page.getLayoutMetrics": true, "Page.captureScreenshot": true,
	"Page.startScreencast": true, "Page.stopScreencast": true, "Page.screencastFrameAck": true,
}

// inputCalls are the calls of inputs the page got since the first from.
func (p *fakePage) inputCalls(from int) []call {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []call
	for _, c := range p.calls[from:] {
		if !streamOwn[c.Method] {
			out = append(out, c)
		}
	}
	return out
}

func (p *fakePage) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

// describe is a call as "Method type x,y" (fields that are set).
func describe(c call) string {
	s := c.Method
	for _, k := range []string{"type", "x", "y", "button", "buttons", "clickCount", "deltaX", "deltaY", "text", "key", "entryId", "url"} {
		if v, ok := c.Params[k]; ok {
			s += fmt.Sprintf(" %s=%v", k, v)
		}
	}
	return s
}

func TestInputTurnsFractionsIntoCSSPxWithTheScreencastMetadata(t *testing.T) {
	// A 1000 × 800 DIP frame, 40 DIP of top controls, pinch-zoomed 2×.
	st, page := liveStream(t, `{"deviceWidth":1000,"deviceHeight":800,"offsetTop":40,"pageScaleFactor":2}`)
	from := page.callCount()
	if _, err := st.Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.25}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range page.inputCalls(from) {
		got = append(got, describe(c))
	}
	// x = 0.5 × 1000 / 2; y = (0.25 × 800 − 40) / 2.
	want := []string{
		"Input.dispatchMouseEvent type=mousePressed x=250 y=80 button=left buttons=1 clickCount=1",
		"Input.dispatchMouseEvent type=mouseReleased x=250 y=80 button=left buttons=0 clickCount=1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("tap sent\n%q\nwant\n%q", got, want)
	}
}

func TestGeometryBeforeTheScreencastIsTheScreenshotsViewport(t *testing.T) {
	// The full frame of captureScreenshot: the visual viewport in CSS px.
	g := geometry{Width: 400 * 2, Height: 300 * 2, Scale: 2}
	if x, y := g.toCSS(1, 0.5); x != 400 || y != 150 {
		t.Fatalf("toCSS(1, 0.5) = %v, %v, want 400, 150", x, y)
	}
	if x, y := (geometry{Width: 800, Height: 600}).toCSS(0.5, 0.5); x != 400 || y != 300 {
		t.Fatalf("without a scale: %v, %v, want 400, 300", x, y)
	}
}

// inputAllowed are the only methods an input may send (ADR 0008).
var inputAllowed = map[string]bool{
	"Input.dispatchMouseEvent": true, "Input.insertText": true, "Input.dispatchKeyEvent": true,
	"Page.getNavigationHistory": true, "Page.navigateToHistoryEntry": true,
	"Page.reload": true, "Page.navigate": true,
}

func TestEveryInputSendsOnlyTheAllowedMethodsInOrder(t *testing.T) {
	st, page := liveStream(t, `{"deviceWidth":800,"deviceHeight":600,"offsetTop":0,"pageScaleFactor":1}`)
	from := page.callCount()
	for _, in := range []wire.HostScreenInput{
		{Type: wire.InputTap, X: 0.25, Y: 0.5},
		{Type: wire.InputDrag, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 0.5, Y: 0.5}, {X: 1, Y: 1}}, DurationMS: 20},
		{Type: wire.InputScroll, X: 0.5, Y: 0.5, DX: 0, DY: -0.1},
		{Type: wire.InputText, Text: "おぎの", Enter: true},
		{Type: wire.InputText, Text: "abc"},
		{Type: wire.InputNavigate, Action: wire.NavigateBack},
		{Type: wire.InputNavigate, Action: wire.NavigateForward},
		{Type: wire.InputNavigate, Action: wire.NavigateReload},
		{Type: wire.InputNavigate, Action: wire.NavigateURL, URL: "https://example.com/"},
	} {
		if _, err := st.Input(t.Context(), in); err != nil {
			t.Fatalf("%+v: %v", in, err)
		}
	}
	var got []string
	for _, c := range page.inputCalls(from) {
		if !inputAllowed[c.Method] {
			t.Errorf("an input sent %s", c.Method)
		}
		got = append(got, describe(c))
	}
	want := []string{
		"Input.dispatchMouseEvent type=mousePressed x=200 y=300 button=left buttons=1 clickCount=1",
		"Input.dispatchMouseEvent type=mouseReleased x=200 y=300 button=left buttons=0 clickCount=1",
		"Input.dispatchMouseEvent type=mousePressed x=0 y=0 button=left buttons=1 clickCount=1",
		"Input.dispatchMouseEvent type=mouseMoved x=400 y=300 button=left buttons=1 clickCount=1",
		"Input.dispatchMouseEvent type=mouseMoved x=800 y=600 button=left buttons=1 clickCount=1",
		"Input.dispatchMouseEvent type=mouseReleased x=800 y=600 button=left buttons=0 clickCount=1",
		// Two fingers up 10% of the frame: the wheel scrolls the page down.
		"Input.dispatchMouseEvent type=mouseWheel x=400 y=300 deltaX=0 deltaY=60",
		"Input.insertText text=おぎの",
		"Input.dispatchKeyEvent type=keyDown text=\r key=Enter",
		"Input.dispatchKeyEvent type=keyUp key=Enter",
		"Input.insertText text=abc",
		"Page.getNavigationHistory",
		"Page.navigateToHistoryEntry entryId=10",
		"Page.getNavigationHistory",
		"Page.navigateToHistoryEntry entryId=12",
		"Page.reload",
		"Page.navigate url=https://example.com/",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("inputs sent\n%q\nwant\n%q", got, want)
	}
}

func TestDragSpreadsItsMovesOverItsDuration(t *testing.T) {
	st, _ := liveStream(t, "")
	began := time.Now()
	in := wire.HostScreenInput{Type: wire.InputDrag, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 0.5, Y: 0.5}, {X: 1, Y: 1}}, DurationMS: 200}
	if _, err := st.Input(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 200*time.Millisecond {
		t.Fatalf("a 200 ms drag took %v", d)
	}
}

func TestBrowserTakesNoKey(t *testing.T) {
	st, page := liveStream(t, "")
	from := page.callCount()
	var we *wire.Error
	_, err := st.Input(t.Context(), wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyBack})
	if !errors.As(err, &we) || we.Code != wire.ErrUnsupported {
		t.Fatalf("key: %v, want unsupported", err)
	}
	if c := page.inputCalls(from); len(c) != 0 {
		t.Fatalf("key sent %v", c)
	}
}
