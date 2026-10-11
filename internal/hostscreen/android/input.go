package android

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/android/emupb"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// touchPressure is the pressure of a finger down (0 lifts it).
	touchPressure = 1024
	// scrollDuration spreads a scroll's swipe; short enough to fling a
	// list a little, long enough not to be a tap.
	scrollDuration = 150 * time.Millisecond
	// scrollSteps is how many moves a scroll's swipe makes.
	scrollSteps = 6
)

// keycodes are the keyevents of HostScreenInput keys (ADR 0008: adb, not
// gRPC sendKey, which an AVD with hw.keyboard=no drops).
var keycodes = map[string]string{
	wire.KeyBack:    "KEYCODE_BACK",
	wire.KeyHome:    "KEYCODE_HOME",
	wire.KeyRecents: "KEYCODE_APP_SWITCH",
}

// toPx turns frame fractions into the px sendTouch takes: the display's
// in its natural orientation (w × h), whatever way the device is turned.
// The frame shows the display turned by rot; a point is turned back.
func toPx(fx, fy float64, w, h int, rot emupb.Rotation_SkinRotation) (x, y int32) {
	switch rot {
	case emupb.Rotation_REVERSE_LANDSCAPE:
		fx, fy = fy, 1-fx
	case emupb.Rotation_REVERSE_PORTRAIT:
		fx, fy = 1-fx, 1-fy
	case emupb.Rotation_LANDSCAPE:
		fx, fy = 1-fy, fx
	}
	return clampPx(fx, w), clampPx(fy, h)
}

func clampPx(f float64, n int) int32 {
	v := int32(math.Round(f * float64(n)))
	return min(max(v, 0), int32(max(n-1, 0)))
}

// rotation is how the last frame showed the display turned.
func (s *stream) rotation() emupb.Rotation_SkinRotation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rot
}

// Input performs in on the emulator: touches through gRPC sendTouch, keys
// and text through `adb shell input`, text that `input text` cannot type
// through the clipboard.
func (s *stream) Input(ctx context.Context, in wire.HostScreenInput) error {
	switch in.Type {
	case wire.InputTap:
		return s.drag(ctx, []wire.HostScreenPoint{{X: in.X, Y: in.Y}}, 0)
	case wire.InputDrag:
		return s.drag(ctx, in.Points, time.Duration(in.DurationMS)*time.Millisecond)
	case wire.InputScroll:
		return s.drag(ctx, swipe(in), scrollDuration)
	case wire.InputKey:
		code, ok := keycodes[in.Key]
		if !ok {
			return wire.Errorf(wire.ErrUnsupported, "host_screen.input: an Android emulator has no key %q", in.Key)
		}
		return s.shell(ctx, "input keyevent "+code)
	case wire.InputText:
		return s.text(ctx, in.Text, in.Enter)
	}
	return wire.Errorf(wire.ErrUnsupported, "host_screen.input: an Android emulator takes no %s", in.Type)
}

// swipe is a scroll as a finger: from the cursor, DX and DY further,
// kept on the frame.
func swipe(in wire.HostScreenInput) []wire.HostScreenPoint {
	pts := make([]wire.HostScreenPoint, 0, scrollSteps+1)
	for i := range scrollSteps + 1 {
		f := float64(i) / scrollSteps
		pts = append(pts, wire.HostScreenPoint{
			X: min(max(in.X+in.DX*f, 0), 1),
			Y: min(max(in.Y+in.DY*f, 0), 1),
		})
	}
	return pts
}

// drag puts a finger down at the first point, moves it through the others
// spread over d and lifts it at the last.
func (s *stream) drag(ctx context.Context, pts []wire.HostScreenPoint, d time.Duration) error {
	rot := s.rotation()
	var step time.Duration
	if len(pts) > 1 {
		step = d / time.Duration(len(pts)-1)
	}
	var x, y int32
	for i, p := range pts {
		if i > 0 {
			if err := s.sleep(ctx, step); err != nil {
				s.touch(context.WithoutCancel(ctx), x, y, 0)
				return err
			}
		}
		x, y = toPx(p.X, p.Y, s.lcdW, s.lcdH, rot)
		if err := s.touch(ctx, x, y, touchPressure); err != nil {
			return err
		}
	}
	return s.touch(ctx, x, y, 0)
}

func (s *stream) touch(ctx context.Context, x, y, pressure int32) error {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	_, err := s.client.SendTouch(ctx, &emupb.TouchEvent{Touches: []*emupb.Touch{{X: x, Y: y, Pressure: pressure}}})
	if err != nil {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: sendTouch: %v", err)
	}
	return nil
}

func (s *stream) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: %v", ctx.Err())
	case <-s.ctx.Done():
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: the emulator is gone")
	}
}

// text types t into the focused field, then Enter when enter is set. Text
// `input text` types as is goes so; any other text (non-ASCII, say a
// confirmed Japanese word) is put on the clipboard and pasted.
func (s *stream) text(ctx context.Context, t string, enter bool) error {
	var cmds []string
	switch {
	case t == "":
	case typable(t):
		cmds = append(cmds, "input text "+inputTextArg(t))
	default:
		if err := s.setClipboard(ctx, t); err != nil {
			return err
		}
		cmds = append(cmds, "input keyevent KEYCODE_PASTE")
	}
	if enter {
		cmds = append(cmds, "input keyevent KEYCODE_ENTER")
	}
	if len(cmds) == 0 {
		return nil
	}
	return s.shell(ctx, strings.Join(cmds, " && "))
}

// typable tells whether `input text` types t as is: printable ASCII only,
// and no "%s" (which it turns into a space).
func typable(t string) bool {
	for i := range len(t) {
		if t[i] < 0x20 || t[i] > 0x7e {
			return false
		}
	}
	return !strings.Contains(t, "%s")
}

// inputTextArg is t as the argument of `input text` in the device's
// shell: spaces as %s (`input text` joins its arguments without them),
// the whole single-quoted so the shell leaves every other character
// alone.
func inputTextArg(t string) string {
	t = strings.ReplaceAll(t, " ", "%s")
	return "'" + strings.ReplaceAll(t, "'", `'\''`) + "'"
}

func (s *stream) setClipboard(ctx context.Context, t string) error {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	if _, err := s.client.SetClipboard(ctx, &emupb.ClipData{Text: t}); err != nil {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: setClipboard: %v", err)
	}
	return nil
}

// shell runs cmd in the device's shell (`adb -s <serial> shell cmd`).
func (s *stream) shell(ctx context.Context, cmd string) error {
	if s.adb == nil {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: adb is not found")
	}
	if out, err := s.adb(ctx, "-s", s.serial, "shell", cmd); err != nil {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: adb shell %s: %v %s", cmd, err, strings.TrimSpace(string(out)))
	}
	return nil
}

var _ hostscreen.Inputter = (*stream)(nil)
