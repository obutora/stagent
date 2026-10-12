package ios

import (
	"context"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// scrollMoves and scrollStep shape a scroll's swipe: the touch moves
	// from the cursor to its end in scrollMoves steps scrollStep apart (as
	// a drag's every 16 ms in #630), then lifts there.
	scrollMoves = 10
	scrollStep  = 16 * time.Millisecond
)

// hidCloseWait bounds how long Close waits for the companion to perform
// the events sent.
const hidCloseWait = time.Second

// HID usages (USB HID Usage Tables, keyboard page) stagent types with.
const (
	hidV         = 0x19
	hidReturn    = 0x28
	hidLang2     = 0x91 // 英数: leaves the Japanese input of a ja simulator (#649)
	hidLeftShift = 0xe1
	hidLeftGUI   = 0xe3 // ⌘
)

// toPt turns frame fractions into the simulator's points: the frame is
// size (w × h pt, turned like the picture).
func toPt(size [2]int, fx, fy float64) *idbpb.Point {
	return &idbpb.Point{X: fx * float64(size[0]), Y: fy * float64(size[1])}
}

// Input performs in through the companion's hid stream, kept open for the
// stream (#630): a touch, the home button or keys; text is pasted (see
// text). Its result is empty but for a text.
func (s *stream) Input(ctx context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	if in.Type == wire.InputText {
		return s.text(ctx, in)
	}
	return wire.HostScreenInputResult{}, s.perform(ctx, in)
}

// perform performs an input but text. An iOS simulator has no back or
// recents button and no navigation.
func (s *stream) perform(ctx context.Context, in wire.HostScreenInput) error {
	size := s.frameSize()
	switch in.Type {
	case wire.InputTap:
		p := toPt(size, in.X, in.Y)
		return s.hid(touch(p, idbpb.HIDEvent_DOWN), touch(p, idbpb.HIDEvent_UP))
	case wire.InputDrag:
		pts := make([]*idbpb.Point, len(in.Points))
		for i, p := range in.Points {
			pts[i] = toPt(size, p.X, p.Y)
		}
		return s.touchPath(ctx, pts, time.Duration(in.DurationMS)*time.Millisecond)
	case wire.InputScroll:
		return s.touchPath(ctx, swipePath(size, in), scrollMoves*scrollStep)
	case wire.InputKey:
		if in.Key == wire.KeyHome {
			return s.hid(button(idbpb.HIDEvent_HOME, idbpb.HIDEvent_DOWN), button(idbpb.HIDEvent_HOME, idbpb.HIDEvent_UP))
		}
		return wire.Errorf(wire.ErrUnsupported, "host_screen.input: an iOS simulator has no %s button", in.Key)
	}
	return wire.Errorf(wire.ErrUnsupported, "host_screen.input: an iOS simulator takes no %s", in.Type)
}

// touchPath puts a finger down at the first point, moves it through the
// others spread over d (a touch down at each, #630) and lifts it at the
// last. Should it stop half way (the tab closing), the finger is lifted
// where it is, so no touch stays down.
func (s *stream) touchPath(ctx context.Context, pts []*idbpb.Point, d time.Duration) error {
	step := d / time.Duration(len(pts)-1)
	at := pts[0]
	if err := s.hid(touch(at, idbpb.HIDEvent_DOWN)); err != nil {
		return err
	}
	for _, p := range pts[1:] {
		err := s.sleep(ctx, step)
		if err == nil {
			err = s.hid(touch(p, idbpb.HIDEvent_DOWN))
		}
		if err != nil {
			s.hid(touch(at, idbpb.HIDEvent_UP))
			return err
		}
		at = p
	}
	return s.hid(touch(at, idbpb.HIDEvent_UP))
}

// swipePath is a scroll's swipe: from the cursor, as far as the fingers
// moved (fractions of the frame), the end kept on the screen.
func swipePath(size [2]int, in wire.HostScreenInput) []*idbpb.Point {
	x1, y1 := clamp01(in.X+in.DX), clamp01(in.Y+in.DY)
	pts := make([]*idbpb.Point, scrollMoves+1)
	for i := range pts {
		f := float64(i) / scrollMoves
		pts[i] = toPt(size, in.X+(x1-in.X)*f, in.Y+(y1-in.Y)*f)
	}
	return pts
}

func clamp01(f float64) float64 { return min(max(f, 0), 1) }

// typeText is the key strokes typing text on a US keyboard, Shift held
// for capitals and the upper symbols; false when text has a character no
// key types (anything but printable ASCII, tab and newline).
func typeText(text string) ([]*idbpb.HIDEvent, bool) {
	var events []*idbpb.HIDEvent
	for _, r := range text {
		if r >= 0x80 {
			return nil, false
		}
		k := asciiKeys[r]
		if k.code == 0 {
			return nil, false
		}
		events = append(events, keyStroke(k.code, k.shift)...)
	}
	return events, true
}

// keyStroke presses and releases code, with Shift around it when shift.
func keyStroke(code uint64, shift bool) []*idbpb.HIDEvent {
	press := []*idbpb.HIDEvent{key(code, idbpb.HIDEvent_DOWN), key(code, idbpb.HIDEvent_UP)}
	if !shift {
		return press
	}
	return append(append([]*idbpb.HIDEvent{key(hidLeftShift, idbpb.HIDEvent_DOWN)}, press...), key(hidLeftShift, idbpb.HIDEvent_UP))
}

type asciiKey struct {
	code  uint64
	shift bool
}

// asciiKeys maps the ASCII characters a US keyboard types to their keys.
var asciiKeys = func() [0x80]asciiKey {
	var m [0x80]asciiKey
	for i := range 26 {
		m['a'+i] = asciiKey{code: uint64(0x04 + i)}
		m['A'+i] = asciiKey{code: uint64(0x04 + i), shift: true}
	}
	for i, c := range "1234567890" {
		m[c] = asciiKey{code: uint64(0x1e + i)}
	}
	for i, c := range "!@#$%^&*()" {
		m[c] = asciiKey{code: uint64(0x1e + i), shift: true}
	}
	m['\n'] = asciiKey{code: hidReturn}
	m['\t'] = asciiKey{code: 0x2b}
	m[' '] = asciiKey{code: 0x2c}
	for _, k := range []struct {
		plain, shifted rune
		code           uint64
	}{
		{'-', '_', 0x2d}, {'=', '+', 0x2e}, {'[', '{', 0x2f}, {']', '}', 0x30},
		{'\\', '|', 0x31}, {';', ':', 0x33}, {'\'', '"', 0x34}, {'`', '~', 0x35},
		{',', '<', 0x36}, {'.', '>', 0x37}, {'/', '?', 0x38},
	} {
		m[k.plain] = asciiKey{code: k.code}
		m[k.shifted] = asciiKey{code: k.code, shift: true}
	}
	return m
}()

func touch(p *idbpb.Point, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return press(&idbpb.HIDEvent_HIDPressAction{Action: &idbpb.HIDEvent_HIDPressAction_Touch{Touch: &idbpb.HIDEvent_HIDTouch{Point: p}}}, dir)
}

func key(code uint64, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return press(&idbpb.HIDEvent_HIDPressAction{Action: &idbpb.HIDEvent_HIDPressAction_Key{Key: &idbpb.HIDEvent_HIDKey{Keycode: code}}}, dir)
}

func button(b idbpb.HIDEvent_HIDButtonType, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return press(&idbpb.HIDEvent_HIDPressAction{Action: &idbpb.HIDEvent_HIDPressAction_Button{Button: &idbpb.HIDEvent_HIDButton{Button: b}}}, dir)
}

func press(a *idbpb.HIDEvent_HIDPressAction, dir idbpb.HIDEvent_HIDDirection) *idbpb.HIDEvent {
	return &idbpb.HIDEvent{Event: &idbpb.HIDEvent_Press{Press: &idbpb.HIDEvent_HIDPress{Action: a, Direction: dir}}}
}

// hid sends events on the companion's hid stream, opening it the first
// time. The companion performs each as it arrives and ends the stream
// when one fails; an event that finds the stream ended opens another and
// goes again, once. Inputs come one at a time, so only Input uses it.
func (s *stream) hid(events ...*idbpb.HIDEvent) error {
	for _, e := range events {
		var err error
		for range 2 {
			if s.hidc == nil {
				if s.hidc, err = s.comp.client.Hid(s.ctx); err != nil {
					s.hidc = nil
					continue
				}
			}
			if err = s.hidc.Send(e); err == nil {
				break
			}
			// The status the companion ended it with, for the error.
			if _, rerr := s.hidc.CloseAndRecv(); rerr != nil {
				err = rerr
			}
			s.hidc = nil
		}
		if err != nil {
			if s.ctx.Err() != nil {
				return wire.Errorf(wire.ErrUnavailable, "host_screen.input: the simulator is gone")
			}
			return wire.Errorf(wire.ErrUnavailable, "host_screen.input: idb_companion: %v", err)
		}
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
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: the simulator is gone")
	}
}

var _ hostscreen.Inputter = (*stream)(nil)
