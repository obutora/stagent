package ios

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

const otherBootedJSON = `{"devices": {"com.apple.CoreSimulator.SimRuntime.iOS-26-2": [
  {"udid": "U1", "name": "iPhone 17", "state": "Shutdown"},
  {"udid": "U2", "name": "iPhone Air", "state": "Booted"}]}}`

// bootingTools is simctl on a Mac whose simulators boot and shut down.
type bootingTools struct {
	mu   sync.Mutex
	list string
}

func (b *bootingTools) set(list string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.list = list
}

func (b *bootingTools) run(ctx context.Context, env []string, path string, args ...string) ([]byte, error) {
	b.mu.Lock()
	list := b.list
	b.mu.Unlock()
	return fakeTools{list: list}.run(ctx, env, path, args...)
}

// watchedKind is an iOS kind whose idb_companion is the fake in mode and
// whose simctl lists what tools is set to.
func watchedKind(t *testing.T, mode string, tools *bootingTools) *Kind {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	k := testKind(t, fakeTools{}, []string{"xcrun", "idb_companion"}, self, envFakeCompanion+"="+mode)
	k.run = tools.run
	return k
}

func expect(t *testing.T, ev events, want any) {
	t.Helper()
	got := ev.next(t, 5*time.Second)
	if _, frame := want.(hostscreen.Frame); frame {
		if _, ok := got.(hostscreen.Frame); !ok {
			t.Fatalf("got %#v, want a frame", got)
		}
		return
	}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

var (
	live = wire.HostScreenState{State: wire.HostScreenLive, Title: "iPhone 17"}
	gone = wire.HostScreenState{State: wire.HostScreenGone}
)

func TestGoneSimulatorIsShownAgainWhenTheSameOneBoots(t *testing.T) {
	tools := &bootingTools{list: bootedJSON}
	k := watchedKind(t, "exit", tools)
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// U1 shuts down (its companion exits after the first frame) and
	// another simulator boots.
	tools.set(otherBootedJSON)
	ev := events{c: make(chan any, 16)}
	st.Start(ev)
	expect(t, ev, live)
	expect(t, ev, hostscreen.Frame{})
	expect(t, ev, gone)
	if got := ev.next(t, 300*time.Millisecond); got != nil {
		t.Fatalf("%#v while only another simulator is booted", got)
	}
	tools.set(bootedJSON)
	expect(t, ev, live)
	expect(t, ev, hostscreen.Frame{})
}

func TestStartOfAShutDownSimulatorIsGoneUntilItBoots(t *testing.T) {
	tools := &bootingTools{list: otherBootedJSON}
	// Skips on Windows, which has no /tmp.
	k := watchedKind(t, "turn", tools)
	// Where the companion's directory is made; short, for the socket's
	// path.
	tmp, err := os.MkdirTemp("/tmp", "s")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"})
	if err != nil {
		t.Fatalf("start of a shut down simulator: %v, want {}", err)
	}
	ev := events{c: make(chan any, 16)}
	st.Start(ev)
	expect(t, ev, gone)
	tools.set(bootedJSON)
	expect(t, ev, live)
	expect(t, ev, hostscreen.Frame{})
	companions := func() int {
		n := 0
		ents, _ := os.ReadDir(tmp)
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "stagent-ios-") {
				n++
			}
		}
		return n
	}
	if n := companions(); n != 1 {
		t.Fatalf("%d companions while live, want 1", n)
	}
	st.Close()
	if n := companions(); n != 0 {
		t.Fatalf("Close left %d companions", n)
	}
}

// simctl failing at start (a timeout, say) is not the simulator missing:
// gone until simctl lists it booted, as one shut down.
func TestStartWhileSimctlFailsIsGoneUntilItListsTheSimulator(t *testing.T) {
	tools := &bootingTools{list: "simctl printed no JSON"}
	k := watchedKind(t, "turn", tools)
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenIOS, UDID: "U1"})
	if err != nil {
		t.Fatalf("start while simctl fails: %v, want {}", err)
	}
	defer st.Close()
	ev := events{c: make(chan any, 16)}
	st.Start(ev)
	expect(t, ev, gone)
	tools.set(bootedJSON)
	expect(t, ev, live)
	expect(t, ev, hostscreen.Frame{})
}

func TestVideoStreamIsHalfScaleHalfQualityMJPEGOnRedraw(t *testing.T) {
	got := videoStart().GetStart()
	if got.GetFormat() != idbpb.VideoStreamRequest_MJPEG || got.GetScaleFactor() != 0.5 || got.GetCompressionQuality() != 0.5 || got.GetFps() != 0 {
		t.Fatalf("video stream start %v, want MJPEG at scale 0.5, quality 0.5, fps 0", got)
	}
}
