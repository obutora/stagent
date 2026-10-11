package browser

import (
	"context"
	"errors"
	"time"

	"github.com/obutora/stagent/internal/hostscreen"
	"github.com/obutora/stagent/internal/wire"
)

// geometry is how a frame maps to the page (Page.screencastFrame's
// metadata): the frame is Width × Height DIP, its viewport starts
// OffsetTop DIP down (a mobile browser's top controls) and the page is
// pinch-zoomed by Scale.
type geometry struct {
	Width, Height, OffsetTop, Scale float64
}

// toCSS turns frame fractions into Input.dispatchMouseEvent's x and y: CSS
// px from the visual viewport's top left.
func (g geometry) toCSS(fx, fy float64) (x, y float64) {
	s := g.scale()
	return fx * g.Width / s, (fy*g.Height - g.OffsetTop) / s
}

// deltaToCSS turns a move of frame fractions into CSS px.
func (g geometry) deltaToCSS(dx, dy float64) (x, y float64) {
	s := g.scale()
	return dx * g.Width / s, dy * g.Height / s
}

func (g geometry) scale() float64 {
	if g.Scale <= 0 {
		return 1
	}
	return g.Scale
}

func (s *stream) setGeometry(g geometry) {
	s.mu.Lock()
	s.geom = g
	s.mu.Unlock()
}

func (s *stream) geometry() geometry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.geom
}

// Input performs in on the page with Input.* and Page.* navigation only
// (ADR 0008): no Emulation.*, no new targets, no dialogs answered.
func (s *stream) Input(ctx context.Context, in wire.HostScreenInput) error {
	switch in.Type {
	case wire.InputTap:
		x, y := s.geometry().toCSS(in.X, in.Y)
		if err := s.mouse(ctx, "mousePressed", x, y, 1); err != nil {
			return err
		}
		return s.mouse(ctx, "mouseReleased", x, y, 0)
	case wire.InputDrag:
		return s.drag(ctx, in)
	case wire.InputScroll:
		g := s.geometry()
		x, y := g.toCSS(in.X, in.Y)
		// The fingers move the page: the wheel turns the other way (0 −
		// keeps a still axis +0, not -0).
		dx, dy := g.deltaToCSS(in.DX, in.DY)
		return s.input(ctx, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y, "deltaX": 0 - dx, "deltaY": 0 - dy,
		}, nil)
	case wire.InputText:
		if in.Text != "" {
			if err := s.input(ctx, "Input.insertText", map[string]any{"text": in.Text}, nil); err != nil {
				return err
			}
		}
		if !in.Enter {
			return nil
		}
		enter := map[string]any{"type": "keyDown", "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13, "text": "\r", "unmodifiedText": "\r"}
		if err := s.input(ctx, "Input.dispatchKeyEvent", enter, nil); err != nil {
			return err
		}
		return s.input(ctx, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": "Enter", "code": "Enter",
			"windowsVirtualKeyCode": 13, "nativeVirtualKeyCode": 13}, nil)
	case wire.InputNavigate:
		return s.navigate(ctx, in)
	}
	return wire.Errorf(wire.ErrUnsupported, "host_screen.input: a browser takes no %s", in.Type)
}

// mouse sends a left-button event at CSS px x, y; buttons is 1 while the
// button is down.
func (s *stream) mouse(ctx context.Context, typ string, x, y float64, buttons int) error {
	return s.input(ctx, "Input.dispatchMouseEvent", map[string]any{
		"type": typ, "x": x, "y": y, "button": "left", "buttons": buttons, "clickCount": 1,
	}, nil)
}

// drag presses at the first point, moves through the others spread over
// DurationMS and releases at the last.
func (s *stream) drag(ctx context.Context, in wire.HostScreenInput) error {
	g := s.geometry()
	step := time.Duration(in.DurationMS) * time.Millisecond / time.Duration(len(in.Points)-1)
	x, y := g.toCSS(in.Points[0].X, in.Points[0].Y)
	if err := s.mouse(ctx, "mousePressed", x, y, 1); err != nil {
		return err
	}
	for _, p := range in.Points[1:] {
		if err := s.sleep(ctx, step); err != nil {
			return err
		}
		x, y = g.toCSS(p.X, p.Y)
		if err := s.mouse(ctx, "mouseMoved", x, y, 1); err != nil {
			return err
		}
	}
	return s.mouse(ctx, "mouseReleased", x, y, 0)
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
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: the page is gone")
	}
}

// navigate goes back or forward in the page's history (nothing when there
// is no such entry), reloads it or opens a URL.
func (s *stream) navigate(ctx context.Context, in wire.HostScreenInput) error {
	switch in.Action {
	case wire.NavigateReload:
		return s.input(ctx, "Page.reload", nil, nil)
	case wire.NavigateURL:
		var r struct {
			ErrorText string `json:"errorText"`
		}
		if err := s.input(ctx, "Page.navigate", map[string]any{"url": in.URL}, &r); err != nil {
			return err
		}
		if r.ErrorText != "" {
			return wire.Errorf(wire.ErrUnavailable, "host_screen.input: navigate to %s: %s", in.URL, r.ErrorText)
		}
		return nil
	}
	var h struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int64 `json:"id"`
		} `json:"entries"`
	}
	if err := s.input(ctx, "Page.getNavigationHistory", nil, &h); err != nil {
		return err
	}
	i := h.CurrentIndex - 1
	if in.Action == wire.NavigateForward {
		i = h.CurrentIndex + 1
	}
	if i < 0 || i >= len(h.Entries) {
		return nil
	}
	return s.input(ctx, "Page.navigateToHistoryEntry", map[string]any{"entryId": h.Entries[i].ID}, nil)
}

// input makes one CDP call of an input. Chrome refusing it is
// bad_request; the page not answering (gone, a dialog open) is
// unavailable.
func (s *stream) input(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, callWait)
	defer cancel()
	err := s.c.call(ctx, method, params, result)
	var ce *cdpError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ce):
		return wire.Errorf(wire.ErrBadRequest, "host_screen.input: %s: %s", method, ce.Message)
	default:
		return wire.Errorf(wire.ErrUnavailable, "host_screen.input: %s: %v", method, err)
	}
}

var _ hostscreen.Inputter = (*stream)(nil)
