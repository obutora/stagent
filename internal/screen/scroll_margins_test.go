package screen

import (
	"fmt"
	"testing"
)

func TestScrollMarginsAfterResize(t *testing.T) {
	for _, alt := range []bool{false, true} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("alt=%v/split=%v", alt, split), func(t *testing.T) {
				s := New(80, 40, nil)
				defer s.Close()
				if alt {
					s.Write([]byte("\x1b[?1049h"))
				}
				s.Resize(80, 24)
				s.Write([]byte("\x1b[Hmarker"))
				// Output prepared for the old height can arrive after the resize.
				p := []byte("\x1b[1;40r\x1b[H\x1bM")
				if split {
					for _, b := range p {
						s.Write([]byte{b})
					}
				} else {
					s.Write(p)
				}
				// Reverse index must still scroll within the current screen.
				if c := s.emu.CellAt(0, 1); c == nil || c.Content != "m" {
					t.Fatalf("marker did not scroll to row 2: %v", c)
				}
			})
		}
	}
}

func TestScrollMarginsBounds(t *testing.T) {
	for _, tc := range []struct {
		name, margins string
		minY, maxY    int
		ignored       bool
	}{
		{"oversized bottom", "1;40", 1, 20, true},
		{"oversized top and bottom", "30;40", 1, 20, true},
		{"oversized top with default bottom", "30", 1, 20, true},
		{"valid custom region", "3;18", 2, 18, false},
		{"full screen", "1;24", 0, 24, false},
		{"default", "", 0, 24, false},
		{"zero defaults", "0;0", 0, 24, false},
		{"default top", ";18", 0, 18, false},
		{"default bottom", "3", 2, 24, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(80, 24, nil)
			defer s.Close()
			s.Write([]byte("\x1b[2;20r\x1b[5;6H"))
			before := s.emu.CursorPosition()
			s.Write([]byte("\x1b[" + tc.margins + "r"))
			r := s.in.active().ScrollRegion()
			if r.Min.Y != tc.minY || r.Max.Y != tc.maxY {
				t.Fatalf("scroll region %v, want Y [%d,%d)", r, tc.minY, tc.maxY)
			}
			if tc.ignored && s.emu.CursorPosition() != before {
				t.Fatal("ignored margins changed the cursor position")
			}
			// Exercise reverse index, insert line and delete line after the request.
			s.Write([]byte(fmt.Sprintf("\x1b[%d;1H\x1bM\x1b[L\x1b[M", r.Min.Y+1)))
		})
	}
}
