package ios

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/hostscreen/ios/idbpb"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// pasteBlock is how long keys wait after a ⌘V pasted: iOS shows
	// "<App> pasted from CoreSimulatorBridge" for about 2.65 s and drops
	// every key (and the next ⌘V) meanwhile; taps go through (#649).
	pasteBlock = 3 * time.Second
	// menuWait is how long after a tap the edit menu's Paste is looked for
	// (it shows 0.37–0.41 s after the tap on the caret, #649), every
	// menuPoll.
	menuWait = time.Second
	menuPoll = 50 * time.Millisecond
	// menuSettle: a menu item tapped as soon as it stopped moving may not
	// take the tap (#649).
	menuSettle = 150 * time.Millisecond
	// refuseSettle: how long after the companion performed a ⌘V iOS
	// refuses that the text is copied again (see paste).
	refuseSettle = 300 * time.Millisecond
	// pasteSettle: the edit menu's Paste inserts within 0.44 s of its tap
	// (#649); an enter's Return waits for it.
	pasteSettle = 500 * time.Millisecond
	// axWait bounds one accessibility_info or list_apps (the first after
	// the companion starts takes about 2.7 s).
	axWait = 5 * time.Second
)

// text puts a text into the field with the focus and presses Return after
// it for enter (#649). The text goes on the simulator's clipboard and is
// pasted, as a pasted text skips the Japanese input a ja simulator types
// ASCII keys through, by what the app in front may do:
//
//   - allowed to paste from other apps: ⌘V, at the caret. Keys wait
//     pasteBlock after it (see keys).
//   - an app that never asked gets a ⌘V first, which iOS refuses but
//     answers with the app's item in Settings; then as not allowed.
//   - not allowed, the text all ASCII: no paste, the 英数 key (LANG2) and
//     the text's keys.
//   - not allowed: a tap at the cursor (in.X, in.Y) opens the edit menu
//     (a second one when the first only moved the focus or the caret) and
//     its Paste is tapped. Without the menu's Paste the text stays on the
//     clipboard: paste_by_hand.
//
// The result tells the way and the app's name.
func (s *stream) text(ctx context.Context, in wire.HostScreenInput) (wire.HostScreenInputResult, error) {
	var res wire.HostScreenInputResult
	if in.Text != "" {
		app := s.frontApp(ctx)
		res.App = app.name
		via, err := s.paste(ctx, in, app)
		if err != nil {
			return res, err
		}
		res.Via = via
	}
	if in.Enter {
		if res.Via == wire.InputViaEditMenu {
			if err := s.sleep(ctx, pasteSettle); err != nil {
				return res, err
			}
		}
		if err := s.keys(ctx, keyStroke(hidReturn, false)...); err != nil {
			return res, err
		}
	}
	return res, nil
}

// paste puts in.Text into app (see text) and returns the way.
func (s *stream) paste(ctx context.Context, in wire.HostScreenInput, app frontApp) (string, error) {
	auth := pasteRefused
	if app.bundle != "" {
		auth = s.host.pasteAuth(ctx, app.bundle)
	}
	if auth == pasteAllowed {
		if err := s.pbcopy(ctx, in.Text); err != nil {
			return "", err
		}
		if err := s.keys(ctx, cmdV()...); err != nil {
			return "", err
		}
		s.keysAfter = time.Now().Add(pasteBlock)
		return wire.InputViaCmdV, nil
	}
	if auth == pasteNoRow {
		// The ⌘V iOS refuses adds the app's row, whatever the text (#649).
		// The edit menu does not paste what iOS just refused the app (on
		// iOS 27.0 it inserts nothing): once the companion performed the
		// ⌘V and iOS refused it, the text is copied anew.
		if err := s.pbcopy(ctx, in.Text); err != nil {
			return "", err
		}
		if err := s.keys(ctx, cmdV()...); err != nil {
			return "", err
		}
		s.flushHID()
		if err := s.sleep(ctx, refuseSettle); err != nil {
			return "", err
		}
	}
	if keys, ok := typeText(in.Text); ok {
		if err := s.keys(ctx, append(keyStroke(hidLang2, false), keys...)...); err != nil {
			return "", err
		}
		return wire.InputViaKeys, nil
	}
	if err := s.pbcopy(ctx, in.Text); err != nil {
		return "", err
	}
	return wire.InputViaEditMenu, s.editMenu(ctx, in, app)
}

// flushHID waits until the companion performed the events sent (at most
// hidCloseWait): it ends the hid stream, and the next event opens another.
func (s *stream) flushHID() {
	s.closeHID()
	s.hidc = nil
}

func (s *stream) pbcopy(ctx context.Context, text string) error {
	if err := s.host.pbcopy(ctx, text); err != nil {
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: %v", err)
	}
	return nil
}

// cmdV is ⌘V.
func cmdV() []*idbpb.HIDEvent {
	return []*idbpb.HIDEvent{
		key(hidLeftGUI, idbpb.HIDEvent_DOWN), key(hidV, idbpb.HIDEvent_DOWN),
		key(hidV, idbpb.HIDEvent_UP), key(hidLeftGUI, idbpb.HIDEvent_UP),
	}
}

// keys sends key events once a ⌘V's banner is gone (pasteBlock).
func (s *stream) keys(ctx context.Context, events ...*idbpb.HIDEvent) error {
	if err := s.sleep(ctx, time.Until(s.keysAfter)); err != nil {
		return err
	}
	return s.hid(events...)
}

// editMenu taps the cursor until the edit menu shows Paste, at most twice,
// and taps it.
func (s *stream) editMenu(ctx context.Context, in wire.HostScreenInput, app frontApp) error {
	if in.X != 0 || in.Y != 0 {
		if s.pasteLabels == nil {
			s.pasteLabels = s.host.pasteLabels(ctx)
		}
		at := toPt(s.frameSize(), in.X, in.Y)
		for range 2 {
			if err := s.hid(touch(at, idbpb.HIDEvent_DOWN), touch(at, idbpb.HIDEvent_UP)); err != nil {
				return err
			}
			p, err := s.waitPaste(ctx)
			if err != nil {
				return err
			}
			if p != nil {
				if err := s.sleep(ctx, menuSettle); err != nil {
					return err
				}
				return s.hid(touch(p, idbpb.HIDEvent_DOWN), touch(p, idbpb.HIDEvent_UP))
			}
		}
	}
	name := app.name
	if name == "" {
		name = "the app"
	}
	return wire.Errorf(wire.ErrPasteByHand, "host_screen.input: %s showed no edit menu with Paste at the cursor; the text is on the simulator's clipboard: long-press the field and choose Paste", name)
}

// waitPaste looks for the edit menu's Paste for menuWait and returns its
// middle once it stays put; nil when it does not show.
func (s *stream) waitPaste(ctx context.Context) (*idbpb.Point, error) {
	deadline := time.Now().Add(menuWait)
	var last *idbpb.Point
	for {
		p := s.findPaste(ctx)
		if p != nil && last != nil && p.X == last.X && p.Y == last.Y {
			return p, nil
		}
		last = p
		if time.Now().After(deadline) {
			return nil, nil
		}
		if err := s.sleep(ctx, menuPoll); err != nil {
			return nil, err
		}
	}
}

// axElement is an element of accessibility_info's NESTED JSON.
type axElement struct {
	Type     string      `json:"type"`
	Label    string      `json:"AXLabel"`
	PID      uint64      `json:"pid"`
	Traits   []string    `json:"traits"`
	Frame    axFrame     `json:"frame"`
	Children []axElement `json:"children"`
}

type axFrame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// axTree reads the screen's elements with keys only; nil when the
// companion cannot tell.
func (s *stream) axTree(ctx context.Context, depth uint32, keys ...string) []axElement {
	ctx, cancel := context.WithTimeout(ctx, axWait)
	defer cancel()
	r, err := s.comp.client.AccessibilityInfo(ctx, &idbpb.AccessibilityInfoRequest{
		Format: idbpb.AccessibilityInfoRequest_NESTED, Depth: depth, Keys: keys,
	})
	if err != nil {
		return nil
	}
	var roots []axElement
	if json.Unmarshal([]byte(r.GetJson()), &roots) != nil {
		return nil
	}
	return roots
}

// findPaste is the middle of the edit menu's Paste (traits MenuItem, a
// label of pasteLabels) on the screen; nil when there is none.
func (s *stream) findPaste(ctx context.Context) *idbpb.Point {
	var find func(es []axElement) *idbpb.Point
	find = func(es []axElement) *idbpb.Point {
		for _, e := range es {
			if menuItem(e) && slices.Contains(s.pasteLabels, e.Label) {
				return &idbpb.Point{X: e.Frame.X + e.Frame.Width/2, Y: e.Frame.Y + e.Frame.Height/2}
			}
			if p := find(e.Children); p != nil {
				return p
			}
		}
		return nil
	}
	return find(s.axTree(ctx, 0, "AXLabel", "traits", "frame"))
}

func menuItem(e axElement) bool {
	return slices.Contains(e.Traits, "MenuItem")
}

// frontApp is the app in front: its bundle id and name, both "" when not
// known (the home screen shows no app).
type frontApp struct{ bundle, name string }

// frontApp finds the app in front: the pid and name of the screen's root
// element, whose bundle id list_apps tells (#649).
func (s *stream) frontApp(ctx context.Context) frontApp {
	var app frontApp
	var pid uint64
	for _, e := range s.axTree(ctx, 1, "pid", "AXLabel", "type") {
		if e.Type == "Application" {
			pid, app.name = e.PID, strings.TrimSpace(e.Label)
			break
		}
	}
	if pid == 0 {
		return app
	}
	if b, ok := s.bundles[pid]; ok {
		app.bundle = b
		return app
	}
	ctx, cancel := context.WithTimeout(ctx, axWait)
	defer cancel()
	r, err := s.comp.client.ListApps(ctx, &idbpb.ListAppsRequest{})
	if err != nil {
		return app
	}
	if s.bundles == nil {
		s.bundles = map[uint64]string{}
	}
	for _, a := range r.GetApps() {
		if a.GetProcessState() == idbpb.InstalledAppInfo_RUNNING && a.GetProcessIdentifier() != 0 {
			s.bundles[a.GetProcessIdentifier()] = a.GetBundleId()
		}
	}
	app.bundle = s.bundles[pid]
	return app
}
