package ios

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

// envFakeHID names the file the fake companion writes the HID events it
// performs to, one line each: the time (ms) and the event.
const envFakeHID = "STAGENT_TEST_FAKE_HID"

var hidBroken atomic.Bool

// Hid performs events by writing them down; in mode "hidbreak" the first
// event fails, which ends the stream as a companion does, and in mode
// "hidslow" the end of a stream answers only after hidCloseWait.
func (f *fakeService) Hid(hs grpc.ClientStreamingServer[idbpb.HIDEvent, idbpb.HIDResponse]) error {
	out, err := os.OpenFile(os.Getenv(envFakeHID), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	for {
		e, err := hs.Recv()
		if err == io.EOF {
			if f.mode == "hidslow" {
				time.Sleep(hidCloseWait + 500*time.Millisecond)
			}
			return hs.SendAndClose(&idbpb.HIDResponse{})
		}
		if err != nil {
			return err
		}
		if f.mode == "hidbreak" && hidBroken.CompareAndSwap(false, true) {
			return status.Error(codes.Internal, "the touch failed")
		}
		fmt.Fprintf(out, "%d %s\n", time.Now().UnixMilli(), hidString(e))
	}
}

func hidString(e *idbpb.HIDEvent) string {
	p := e.GetPress()
	dir := strings.ToLower(p.GetDirection().String())
	switch a := p.GetAction(); {
	case a.GetTouch() != nil:
		pt := a.GetTouch().GetPoint()
		return fmt.Sprintf("touch %s %g,%g", dir, pt.GetX(), pt.GetY())
	case a.GetKey() != nil:
		return fmt.Sprintf("key %s %d", dir, a.GetKey().GetKeycode())
	case a.GetButton() != nil:
		return fmt.Sprintf("button %s %s", dir, a.GetButton().GetButton())
	}
	return "other " + e.String()
}

// hidEvent is one event the fake companion performed.
type hidEvent struct {
	at   time.Time
	what string
}

// inputStream opens simulator U1 (402 × 874 pt) shown by a fake companion
// in mode, which writes the HID events it performs to the returned file.
func inputStream(t *testing.T, mode string) (*stream, string) {
	t.Helper()
	t.Parallel() // a companion takes a second to answer
	log := filepath.Join(t.TempDir(), "hid")
	s, _ := openFake(t, mode, envFakeHID+"="+log)
	return s, log
}

// performed waits until the fake companion performed n events and returns
// them; it fails on more.
func performed(t *testing.T, log string, n int) []hidEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(log)
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(b) == 0 {
			lines = nil
		}
		if len(lines) >= n || time.Now().After(deadline) {
			time.Sleep(100 * time.Millisecond) // and no more
			b, _ = os.ReadFile(log)
			lines = strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(b) == 0 {
				lines = nil
			}
			if len(lines) != n {
				t.Fatalf("companion performed %d events, want %d:\n%s", len(lines), n, b)
			}
			var evs []hidEvent
			for _, l := range lines {
				ms, what, _ := strings.Cut(l, " ")
				v, _ := strconv.ParseInt(ms, 10, 64)
				evs = append(evs, hidEvent{time.UnixMilli(v), what})
			}
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func whats(evs []hidEvent) []string {
	var w []string
	for _, e := range evs {
		w = append(w, e.what)
	}
	return w
}

func TestTapTouchesThePointOfTheFrame(t *testing.T) {
	s, log := inputStream(t, "still")
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.25}); err != nil {
		t.Fatal(err)
	}
	// Fractions of the 402 × 874 pt frame.
	want := []string{"touch down 201,218.5", "touch up 201,218.5"}
	if got := whats(performed(t, log, 2)); !slices.Equal(got, want) {
		t.Fatalf("tap performed %q, want %q", got, want)
	}
}

func TestDragMovesTheTouchDownOverItsDurationThenLifts(t *testing.T) {
	s, log := inputStream(t, "still")
	in := wire.HostScreenInput{Type: wire.InputDrag, DurationMS: 300, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 0.5, Y: 0.5}, {X: 1, Y: 1}}}
	if _, err := s.Input(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	evs := performed(t, log, 4)
	want := []string{"touch down 0,0", "touch down 201,437", "touch down 402,874", "touch up 402,874"}
	if got := whats(evs); !slices.Equal(got, want) {
		t.Fatalf("drag performed %q, want %q", got, want)
	}
	if took := evs[2].at.Sub(evs[0].at); took < 250*time.Millisecond {
		t.Fatalf("drag moved in %v, want its 300 ms", took)
	}
}

func TestScrollIsOneSwipeFromTheCursorKeptOnTheScreen(t *testing.T) {
	s, log := inputStream(t, "still")
	// The fingers moved up a quarter of the frame and far to the left.
	in := wire.HostScreenInput{Type: wire.InputScroll, X: 0.5, Y: 0.5, DX: -3, DY: -0.25}
	if _, err := s.Input(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	got := whats(performed(t, log, scrollMoves+2))
	first, last := got[0], got[len(got)-2:]
	if first != "touch down 201,437" || !slices.Equal(last, []string{"touch down 0,218.5", "touch up 0,218.5"}) {
		t.Fatalf("scroll performed %q, want a swipe from the cursor to its end on the screen", got)
	}
	for _, e := range got[:len(got)-1] {
		if !strings.HasPrefix(e, "touch down ") {
			t.Fatalf("scroll performed %q, want the touch down until it lifts", got)
		}
	}
}

func TestEveryPrintableASCIICharacterHasAKey(t *testing.T) {
	for c := rune(' '); c <= '~'; c++ {
		if evs, ok := typeText(string(c)); !ok || len(evs) == 0 {
			t.Errorf("%q has no key", c)
		}
	}
}

func TestHomePressesTheHomeButton(t *testing.T) {
	s, log := inputStream(t, "still")
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyHome}); err != nil {
		t.Fatal(err)
	}
	want := []string{"button down HOME", "button up HOME"}
	if got := whats(performed(t, log, 2)); !slices.Equal(got, want) {
		t.Fatalf("home performed %q, want %q", got, want)
	}
}

func TestInputsASimulatorDoesNotTakeAreUnsupported(t *testing.T) {
	s, log := inputStream(t, "still")
	for _, in := range []wire.HostScreenInput{
		{Type: wire.InputKey, Key: wire.KeyBack},
		{Type: wire.InputKey, Key: wire.KeyRecents},
		{Type: wire.InputNavigate, Action: wire.NavigateReload},
		{Type: wire.InputNavigate, Action: wire.NavigateURL, URL: "https://example.com"},
	} {
		var we *wire.Error
		if _, err := s.Input(t.Context(), in); !errors.As(err, &we) || we.Code != wire.ErrUnsupported {
			t.Errorf("Input(%+v) = %v, want unsupported", in, err)
		}
	}
	performed(t, log, 0)
}

func TestInputReopensTheHIDStreamTheCompanionEnded(t *testing.T) {
	s, log := inputStream(t, "hidbreak")
	// Its first event fails and ends the stream; the next may be lost with
	// it.
	s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyHome})
	time.Sleep(200 * time.Millisecond)
	if _, err := s.Input(t.Context(), wire.HostScreenInput{Type: wire.InputTap, X: 0, Y: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	b, _ := os.ReadFile(log)
	var evs []string
	for l := range strings.Lines(string(b)) {
		_, what, _ := strings.Cut(strings.TrimSpace(l), " ")
		evs = append(evs, what)
	}
	if len(evs) < 2 || !slices.Equal(evs[len(evs)-2:], []string{"touch down 0,874", "touch up 0,874"}) {
		t.Fatalf("after the stream ended the companion performed %q, want the tap", evs)
	}
}

func TestClosingDuringADragLiftsTheTouchAndEndsTheCompanion(t *testing.T) {
	s, log := inputStream(t, "still")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.Input(ctx, wire.HostScreenInput{Type: wire.InputDrag, DurationMS: 5000, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 1, Y: 1}}})
		done <- err
	}()
	performed(t, log, 1)
	// The tab closes: the bridge's context ends, then the stream.
	cancel()
	var we *wire.Error
	if err := <-done; !errors.As(err, &we) || we.Code != wire.ErrUnavailable {
		t.Fatalf("cut drag = %v, want unavailable", err)
	}
	s.Close()
	if !stopped(s) {
		t.Fatal("companion still running after Close")
	}
	want := []string{"touch down 0,0", "touch up 0,0"}
	if got := whats(performed(t, log, 2)); !slices.Equal(got, want) {
		t.Fatalf("cut drag performed %q, want the touch lifted where it was", got)
	}
}
