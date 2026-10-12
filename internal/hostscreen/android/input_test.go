package android

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/hostscreen/android/emupb"
	"github.com/obutora/stagent/internal/wire"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestToPxFollowsRotation(t *testing.T) {
	// A 1080×2424 portrait display; the frame point is (0.25, 0.75) of the
	// frame as shown. Checked against a Pixel 9 emulator turned each way.
	cases := []struct {
		rot  emupb.Rotation_SkinRotation
		x, y int32
	}{
		{emupb.Rotation_PORTRAIT, 270, 1818},
		{emupb.Rotation_REVERSE_LANDSCAPE, 810, 1818},
		{emupb.Rotation_REVERSE_PORTRAIT, 810, 606},
		{emupb.Rotation_LANDSCAPE, 270, 606},
	}
	for _, c := range cases {
		x, y := toPx(0.25, 0.75, 1080, 2424, c.rot)
		if x != c.x || y != c.y {
			t.Errorf("%v: (%d, %d), want (%d, %d)", c.rot, x, y, c.x, c.y)
		}
	}
	if x, y := toPx(1, 1, 1080, 2424, emupb.Rotation_PORTRAIT); x != 1079 || y != 2423 {
		t.Errorf("the far edge: (%d, %d), want the last px (1079, 2423)", x, y)
	}
}

// adbLog records the device shell commands a stream ran.
type adbLog struct {
	mu   sync.Mutex
	cmds []string
}

func (l *adbLog) run(_ context.Context, args ...string) ([]byte, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(args) != 4 || args[0] != "-s" || args[1] != "emulator-5554" || args[2] != "shell" {
		return nil, errors.New("unexpected adb " + strings.Join(args, " "))
	}
	l.cmds = append(l.cmds, args[3])
	return nil, nil
}

func (l *adbLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.cmds
	l.cmds = nil
	return c
}

// inputEmulator also records touches and the clipboard.
type inputEmulator struct {
	*fakeEmulator
	mu      sync.Mutex
	touches []*emupb.Touch
	clip    []string
}

func (f *inputEmulator) SendTouch(_ context.Context, in *emupb.TouchEvent) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.touches = append(f.touches, in.GetTouches()...)
	f.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func (f *inputEmulator) SetClipboard(_ context.Context, in *emupb.ClipData) (*emptypb.Empty, error) {
	f.mu.Lock()
	f.clip = append(f.clip, in.GetText())
	f.mu.Unlock()
	return &emptypb.Empty{}, nil
}

func openInputStream(t *testing.T) (*stream, *inputEmulator, *adbLog) {
	t.Helper()
	f := &inputEmulator{fakeEmulator: &fakeEmulator{token: "tok", images: make(chan *emupb.Image)}}
	port, _ := serve(t, f)
	st, err := openStream(context.Background(), emulator{serialPort: 5554, grpcPort: port, token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	log := &adbLog{}
	st.serial, st.adb = "emulator-5554", log.run
	return st, f, log
}

func TestInputTouches(t *testing.T) {
	st, f, log := openInputStream(t)
	ctx := context.Background()
	// The fake's display is 40×20.
	if _, err := st.Input(ctx, wire.HostScreenInput{Type: wire.InputTap, X: 0.5, Y: 0.25}); err != nil {
		t.Fatal(err)
	}
	drag := wire.HostScreenInput{Type: wire.InputDrag, Points: []wire.HostScreenPoint{{X: 0, Y: 0}, {X: 0.5, Y: 0.5}, {X: 0.75, Y: 0.5}}}
	if _, err := st.Input(ctx, drag); err != nil {
		t.Fatal(err)
	}
	type touch struct{ x, y, p int32 }
	got := func() []touch {
		f.mu.Lock()
		defer f.mu.Unlock()
		var ts []touch
		for _, tc := range f.touches {
			ts = append(ts, touch{tc.GetX(), tc.GetY(), tc.GetPressure()})
		}
		f.touches = nil
		return ts
	}
	want := []touch{
		{20, 5, touchPressure}, {20, 5, 0},
		{0, 0, touchPressure}, {20, 10, touchPressure}, {30, 10, touchPressure}, {30, 10, 0},
	}
	if g := got(); !slices.Equal(g, want) {
		t.Errorf("tap and drag: %v, want %v", g, want)
	}
	// A scroll is one swipe from the cursor, kept on the display.
	if _, err := st.Input(ctx, wire.HostScreenInput{Type: wire.InputScroll, X: 0.5, Y: 0.5, DY: -2}); err != nil {
		t.Fatal(err)
	}
	g := got()
	if len(g) < 3 || g[0] != (touch{20, 10, touchPressure}) || g[len(g)-1] != (touch{20, 0, 0}) {
		t.Errorf("scroll: %v, want a swipe from (20, 10) up to (20, 0)", g)
	}
	if c := log.take(); len(c) != 0 {
		t.Errorf("touches ran adb: %v", c)
	}
}

func TestInputKeysAndText(t *testing.T) {
	st, f, log := openInputStream(t)
	ctx := context.Background()
	cases := []struct {
		in   wire.HostScreenInput
		cmds []string
		clip string
	}{
		{wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyBack}, []string{"input keyevent KEYCODE_BACK"}, ""},
		{wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyHome}, []string{"input keyevent KEYCODE_HOME"}, ""},
		{wire.HostScreenInput{Type: wire.InputKey, Key: wire.KeyRecents}, []string{"input keyevent KEYCODE_APP_SWITCH"}, ""},
		// Every text is pasted from the clipboard, ASCII too: `input text`
		// goes through the IME, which a Japanese Gboard turns into kana.
		{wire.HostScreenInput{Type: wire.InputText, Text: "wifi test"}, []string{"input keyevent KEYCODE_PASTE"}, "wifi test"},
		{wire.HostScreenInput{Type: wire.InputText, Text: `it's "a"&b; 100%s \`}, []string{"input keyevent KEYCODE_PASTE"}, `it's "a"&b; 100%s \`},
		{wire.HostScreenInput{Type: wire.InputText, Text: "go", Enter: true}, []string{"input keyevent KEYCODE_PASTE && input keyevent KEYCODE_ENTER"}, "go"},
		// Enter alone leaves the clipboard as it was.
		{wire.HostScreenInput{Type: wire.InputText, Enter: true}, []string{"input keyevent KEYCODE_ENTER"}, ""},
		{wire.HostScreenInput{Type: wire.InputText, Text: "日本語 abc"}, []string{"input keyevent KEYCODE_PASTE"}, "日本語 abc"},
		{wire.HostScreenInput{Type: wire.InputText, Text: "東京", Enter: true}, []string{"input keyevent KEYCODE_PASTE && input keyevent KEYCODE_ENTER"}, "東京"},
		{wire.HostScreenInput{Type: wire.InputText, Text: "a\nb"}, []string{"input keyevent KEYCODE_PASTE"}, "a\nb"},
	}
	for _, c := range cases {
		if _, err := st.Input(ctx, c.in); err != nil {
			t.Fatalf("%+v: %v", c.in, err)
		}
		if got := log.take(); !slices.Equal(got, c.cmds) {
			t.Errorf("%+v ran %q, want %q", c.in, got, c.cmds)
		}
		f.mu.Lock()
		clip := strings.Join(f.clip, "|")
		f.clip = nil
		f.mu.Unlock()
		if clip != c.clip {
			t.Errorf("%+v set the clipboard to %q, want %q", c.in, clip, c.clip)
		}
	}
}

func TestInputUnsupported(t *testing.T) {
	st, _, log := openInputStream(t)
	for _, in := range []wire.HostScreenInput{
		{Type: wire.InputNavigate, Action: wire.NavigateBack},
		{Type: wire.InputKey, Key: "power"},
	} {
		var we *wire.Error
		if _, err := st.Input(context.Background(), in); !errors.As(err, &we) || we.Code != wire.ErrUnsupported {
			t.Errorf("%+v: %v, want unsupported", in, err)
		}
	}
	if c := log.take(); len(c) != 0 {
		t.Errorf("ran adb: %v", c)
	}
}
