package android

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/android/emupb"
	"github.com/obutora/stagent/internal/wire"
)

// liveADB is adb on a host whose emulators come and go: devices is what
// `adb devices -l` lists, shells the shell commands run.
type liveADB struct {
	mu      sync.Mutex
	devices []string
	shells  []string
}

func (a *liveADB) set(serials ...string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.devices = serials
}

func (a *liveADB) run(_ context.Context, _ string, _ []string, args ...string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case slices.Equal(args, []string{"devices", "-l"}):
		out := "List of devices attached\n"
		for _, s := range a.devices {
			out += s + "          device transport_id:1\n"
		}
		return []byte(out), nil
	case len(args) == 4 && args[0] == "-s" && args[2] == "shell":
		a.shells = append(a.shells, args[1]+": "+args[3])
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected adb %s", strings.Join(args, " "))
}

// watchedKind is an Android kind on a fake Mac that looks for gone
// emulators every few milliseconds.
func watchedKind(t *testing.T) (*Kind, host, *liveADB) {
	t.Helper()
	home := t.TempDir()
	h := host{goos: "darwin", home: home, alive: everyAlive}
	executable(t, filepath.Join(home, "Library", "Android", "sdk", "platform-tools"), "adb")
	a := &liveADB{}
	return &Kind{host: func() host { return h }, adb: a.run, every: 5 * time.Millisecond}, h, a
}

// advertise writes the discovery file of an emulator of avd on serial
// port, its gRPC on grpcPort.
func advertise(t *testing.T, h host, pid string, serialPort int, avd string, grpcPort int) {
	t.Helper()
	writeDiscovery(t, h.runningDirs()[0], pid, fmt.Sprintf("port.serial=%d\navd.name=%s\ngrpc.port=%d\ngrpc.token=tok\n", serialPort, avd, grpcPort))
}

func TestGoneEmulatorIsShownAgainWhenItsAVDRunsOnAnySerial(t *testing.T) {
	k, h, adb := watchedKind(t)
	pixel := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, stop := serve(t, pixel)
	advertise(t, h, "1", 5554, "Pixel", port)
	adb.set("emulator-5554")
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: "emulator-5554", AVD: "Pixel"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	if s := ev.state(t); s.State != wire.HostScreenLive {
		t.Fatalf("first state %+v, want live", s)
	}

	// The emulator is closed.
	stop()
	os.Remove(filepath.Join(h.runningDirs()[0], "pid_1.ini"))
	adb.set()
	if s := ev.state(t); s.State != wire.HostScreenGone {
		t.Fatalf("state after the emulator stopped: %+v, want gone", s)
	}

	// Another AVD starts on the serial: not the one shown.
	other := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	otherPort, _ := serve(t, other)
	advertise(t, h, "2", 5554, "Other", otherPort)
	adb.set("emulator-5554")
	select {
	case s := <-ev.states:
		t.Fatalf("state %+v for another AVD", s)
	case <-time.After(200 * time.Millisecond):
	}

	// The AVD starts again, on another serial.
	again := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	againPort, _ := serve(t, again)
	advertise(t, h, "3", 5556, "Pixel", againPort)
	adb.set("emulator-5554", "emulator-5556")
	if s := ev.state(t); s.State != wire.HostScreenLive || s.Title != "Pixel" {
		t.Fatalf("state after the AVD started again: %+v, want live", s)
	}
	again.images <- image888(20, 10)
	ev.frame(t)
	// Keys go to the emulator by its new serial.
	if _, err := st.(hostscreen.Inputter).Input(context.Background(), wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyHome}); err != nil {
		t.Fatal(err)
	}
	adb.mu.Lock()
	shells := slices.Clone(adb.shells)
	adb.mu.Unlock()
	if want := []string{"emulator-5556: input keyevent KEYCODE_HOME"}; !slices.Equal(shells, want) {
		t.Fatalf("adb shell %q, want %q", shells, want)
	}
}

func TestStartOfAStoppedAVDIsGoneUntilItRuns(t *testing.T) {
	k, h, adb := watchedKind(t)
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: "emulator-5554", AVD: "Pixel"})
	if err != nil {
		t.Fatalf("start of a stopped AVD: %v, want {}", err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	if s := ev.state(t); s.State != wire.HostScreenGone {
		t.Fatalf("first state %+v, want gone", s)
	}
	pixel := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, pixel)
	advertise(t, h, "1", 5560, "Pixel", port)
	adb.set("emulator-5560")
	if s := ev.state(t); s.State != wire.HostScreenLive {
		t.Fatalf("state after the AVD started: %+v, want live", s)
	}
}

// A discovery file of a running process whose gRPC does not answer, on a
// serial adb does not list: the emulator is ending; its AVD is gone until
// it runs again.
func TestStartOfAnEndingEmulatorIsGoneUntilItsAVDRuns(t *testing.T) {
	k, h, adb := watchedKind(t)
	advertise(t, h, "1", 5554, "Pixel", closedPort(t))
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: "emulator-5554", AVD: "Pixel"})
	if err != nil {
		t.Fatalf("start of an ending emulator: %v, want {}", err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	if s := ev.state(t); s.State != wire.HostScreenGone {
		t.Fatalf("first state %+v, want gone", s)
	}
	pixel := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, pixel)
	os.Remove(filepath.Join(h.runningDirs()[0], "pid_1.ini"))
	advertise(t, h, "2", 5554, "Pixel", port)
	adb.set("emulator-5554")
	if s := ev.state(t); s.State != wire.HostScreenLive {
		t.Fatalf("state after the AVD started again: %+v, want live", s)
	}
}

func TestStartOfARunningAVDOnAnotherSerialShowsIt(t *testing.T) {
	k, h, adb := watchedKind(t)
	pixel := &fakeEmulator{token: "tok", images: make(chan *emupb.Image, 4)}
	port, _ := serve(t, pixel)
	advertise(t, h, "1", 5558, "Pixel", port)
	adb.set("emulator-5558")
	st, err := k.Open(context.Background(), wire.HostScreenTarget{Kind: wire.HostScreenAndroid, Serial: "emulator-5554", AVD: "Pixel"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ev := newEvents()
	st.Start(ev)
	if s := ev.state(t); s.State != wire.HostScreenLive {
		t.Fatalf("first state %+v, want live", s)
	}
}
