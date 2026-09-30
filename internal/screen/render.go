package screen

import (
	"bytes"
	"strconv"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

type cellSource interface {
	CellAt(x, y int) *uv.Cell
}

// Snapshot returns ANSI that reproduces the current screen on a cleared
// terminal of the same size, and leaves that terminal in the state the
// program's next bytes expect: cursor position and visibility, current pen,
// scroll region, autowrap. When the alternate screen is active, the main
// screen is drawn first and the alternate screen entered on top of it, so a
// later switch back shows the right content. Used for raw-mode resets.
func (s *Screen) Snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot(true)
}

func (s *Screen) snapshot(stateful bool) []byte {
	var b bytes.Buffer
	b.Grow(s.cols*s.rows + 256)
	// Known starting point: main screen, default pen, full scroll region,
	// autowrap on, cleared.
	b.WriteString("\x1b[?1049l\x1b[m\x1b[r\x1b[?7h\x1b[H\x1b[2J")
	if stateful && s.in.ok() && s.emu.IsAltScreen() {
		main := s.in.main()
		writeRows(&b, main, s.cols, s.rows)
		c := main.Cursor()
		cup(&b, c.Y, c.X)
		b.WriteString("\x1b[?1049h") // saves that cursor, enters a cleared alternate screen
	}
	writeRows(&b, s.emu, s.cols, s.rows)
	c := s.cursor()
	if stateful {
		if c.top != 0 || c.bottom != s.rows {
			b.WriteString(ansi.SetTopBottomMargins(c.top+1, c.bottom))
		}
		if s.noAutowrap {
			b.WriteString("\x1b[?7l")
		}
	}
	cup(&b, c.y, c.x)
	if stateful && !c.pen.IsZero() {
		b.WriteString(c.pen.String())
	}
	if c.hidden {
		b.WriteString("\x1b[?25l")
	} else {
		b.WriteString("\x1b[?25h")
	}
	return b.Bytes()
}

// writeRows draws the non-blank rows of src onto a cleared screen.
func writeRows(b *bytes.Buffer, src cellSource, cols, rows int) {
	for y := range rows {
		if lastCell(src, y, cols) < 0 {
			continue
		}
		cup(b, y, 0)
		writeRow(b, src, y, cols)
	}
}

// Differ renders screen-mode frames for one client.
type Differ struct {
	s          *Screen
	seen       uint64
	cols, rows int
	x, y       int
	hidden     bool
}

// SnapshotDiffer returns a full frame of the current screen (for a
// screen-mode client, sent with reset) and a Differ whose next Frame is
// relative to that snapshot. The frame carries content, cursor position and
// visibility only; screen-mode clients never receive pen or scroll-region
// state.
func (s *Screen) SnapshotDiffer() ([]byte, *Differ) {
	s.mu.Lock()
	defer s.mu.Unlock()
	frame := s.snapshot(false)
	c := s.cursor()
	return frame, &Differ{s: s, seen: s.ver, cols: s.cols, rows: s.rows, x: c.x, y: c.y, hidden: c.hidden}
}

// Frame returns ANSI redrawing the rows changed since the previous frame
// (every row after a size change), each followed by an erase to the end of
// line, then the cursor position and visibility. It returns nil when
// nothing visible changed.
func (d *Differ) Frame() []byte {
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	full := d.cols != s.cols || d.rows != s.rows
	c := s.cursor()
	var b bytes.Buffer
	drew := false
	for y := range s.rows {
		if !full && s.rowVer[y] <= d.seen {
			continue
		}
		if !drew {
			b.Grow(s.cols*4 + 64)
			b.WriteString("\x1b[?25l\x1b[m") // no cursor flicker while drawing
			drew = true
		}
		cup(&b, y, 0)
		if writeRow(&b, s.emu, y, s.cols) {
			b.WriteString(ansi.EraseLineRight)
		}
	}
	d.seen = s.ver
	d.cols, d.rows = s.cols, s.rows
	if !drew && c.x == d.x && c.y == d.y && c.hidden == d.hidden {
		return nil
	}
	cup(&b, c.y, c.x)
	switch {
	case !c.hidden:
		b.WriteString("\x1b[?25h")
	case !drew:
		b.WriteString("\x1b[?25l")
	}
	d.x, d.y, d.hidden = c.x, c.y, c.hidden
	return b.Bytes()
}

// lastCell returns the index of the last non-blank cell of row y, or -1.
func lastCell(src cellSource, y, cols int) int {
	for x := cols - 1; x >= 0; x-- {
		c := src.CellAt(x, y)
		if c != nil && !c.IsZero() && !c.Equal(&uv.EmptyCell) {
			return x
		}
	}
	return -1
}

// writeRow writes row y up to its last non-blank cell with minimal SGR and
// hyperlink changes, ending with the default pen. It reports whether the
// row ended before the last column (the caller erases the rest when the
// target line may hold old content). Writing stops short of the last column
// in that case, so an erase never hits a pending-wrap cursor.
func writeRow(b *bytes.Buffer, src cellSource, y, cols int) (trimmed bool) {
	end := lastCell(src, y, cols) + 1
	if end > 0 {
		if c := src.CellAt(end-1, y); c.Width > 1 {
			end = min(cols, end-1+c.Width)
		}
	}
	var pen uv.Style
	var link uv.Link
	for x := 0; x < end; {
		c := src.CellAt(x, y)
		if c == nil || c.IsZero() {
			// A placeholder without its wide cell: keep columns aligned.
			if !pen.IsZero() {
				b.WriteString(ansi.ResetStyle)
				pen = uv.Style{}
			}
			b.WriteByte(' ')
			x++
			continue
		}
		if !c.Style.Equal(&pen) {
			b.WriteString(uv.StyleDiff(&pen, &c.Style))
			pen = c.Style
		}
		if c.Link != link {
			if c.Link.URL == "" {
				b.WriteString(ansi.ResetHyperlink())
			} else {
				b.WriteString(ansi.SetHyperlink(c.Link.URL, c.Link.Params))
			}
			link = c.Link
		}
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
		x += max(c.Width, 1)
	}
	if link.URL != "" {
		b.WriteString(ansi.ResetHyperlink())
	}
	if !pen.IsZero() {
		b.WriteString(ansi.ResetStyle)
	}
	return end < cols
}

// cup writes a cursor position for 0-based row y, column x.
func cup(b *bytes.Buffer, y, x int) {
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(x + 1))
	b.WriteByte('H')
}
