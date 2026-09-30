package screen

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// client is a fresh emulator standing in for the app's terminal.
type client struct {
	emu *vt.Emulator
	in  internals
}

func newClient(t *testing.T, cols, rows int) *client {
	t.Helper()
	e := vt.NewEmulator(cols, rows)
	go func() { // a real terminal answers queries; drop them
		buf := make([]byte, 256)
		for {
			if _, err := e.Read(buf); err != nil {
				return
			}
		}
	}()
	in, ok := reach(e)
	if !ok {
		t.Fatal("vt internals not reachable")
	}
	return &client{emu: e, in: in}
}

func (c *client) write(p []byte) { c.emu.Write(p) }

// assertSame compares every cell and the cursor of the server screen with
// the client's, including which screen (main/alternate) is active.
func assertSame(t *testing.T, step string, s *Screen, c *client) {
	t.Helper()
	assertScreen(t, step, s, c, true)
}

// assertVisible compares what is visible: screen-mode frames draw the
// active screen onto whatever screen the client shows.
func assertVisible(t *testing.T, step string, s *Screen, c *client) {
	t.Helper()
	assertScreen(t, step, s, c, false)
}

func assertScreen(t *testing.T, step string, s *Screen, c *client, alt bool) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	cols, rows := s.cols, s.rows
	if c.emu.Width() != cols || c.emu.Height() != rows {
		t.Fatalf("%s: client %dx%d, screen %dx%d", step, c.emu.Width(), c.emu.Height(), cols, rows)
	}
	if alt && s.emu.IsAltScreen() != c.emu.IsAltScreen() {
		t.Fatalf("%s: alt screen server=%v client=%v", step, s.emu.IsAltScreen(), c.emu.IsAltScreen())
	}
	for y := range rows {
		for x := range cols {
			w, g := s.emu.CellAt(x, y), c.emu.CellAt(x, y)
			if !w.Equal(g) || w.Link != g.Link {
				t.Fatalf("%s: cell (%d,%d) server %+v client %+v\nserver:\n%s\nclient:\n%s",
					step, x, y, *w, *g, s.emu.String(), c.emu.String())
			}
		}
	}
	sc, cc := s.in.active().Cursor(), c.in.active().Cursor()
	if sc.X != cc.X || sc.Y != cc.Y || sc.Hidden != cc.Hidden {
		t.Fatalf("%s: cursor server (%d,%d hidden=%v) client (%d,%d hidden=%v)",
			step, sc.X, sc.Y, sc.Hidden, cc.X, cc.Y, cc.Hidden)
	}
}

const rich = "\x1b[1;31mred bold\x1b[m plain \x1b[48;5;22m bg \x1b[m\r\n" +
	"wide: 日本語 ok\r\n" +
	"\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\ after\r\n" +
	"\x1b[4;10H\x1b[38;2;10;20;30mtruecolor\x1b[m" +
	"\x1b[6;1H" + "0123456789012345678901234567890123456789" + // exactly full width
	"\x1b[8;38Hend" +
	"\x1b[3;5H\x1b[?25l"

func TestSnapshotReproducesScreen(t *testing.T) {
	s := New(40, 10, nil)
	defer s.Close()
	s.Write([]byte(rich))

	c := newClient(t, 40, 10)
	c.write(s.Snapshot())
	assertSame(t, "snapshot", s, c)
}

func TestSnapshotRestoresStateForFollowingRawBytes(t *testing.T) {
	cases := []struct{ name, before, after string }{
		{"pen and scroll region",
			"line1\r\nline2\r\n\x1b[2;5r\x1b[5;1H\x1b[32m",
			"green\nscrolls\nwithin\nthe\nregion\x1b[mplain"},
		{"alternate screen over main content",
			"main one\r\nmain two\x1b[?1049h\x1b[Halt content\x1b[2;3H",
			"x\x1b[?1049lback on main"},
		{"autowrap off",
			"\x1b[?7l\x1b[1;35H",
			"overflowing text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(40, 8, nil)
			defer s.Close()
			s.Write([]byte(tc.before))

			c := newClient(t, 40, 8)
			c.write(s.Snapshot())
			s.Write([]byte(tc.after))
			c.write([]byte(tc.after))
			assertSame(t, "after raw continuation", s, c)
		})
	}
}

func TestDiffFramesTrackTheScreen(t *testing.T) {
	s := New(30, 8, nil)
	defer s.Close()
	s.Write([]byte("header\r\nrow two\r\nrow three"))

	frame, d := s.SnapshotDiffer()
	c := newClient(t, 30, 8)
	c.write(frame)
	assertVisible(t, "initial", s, c)

	steps := []struct{ name, out string }{
		{"overwrite a row", "\x1b[2;1H\x1b[7mROW\x1b[m"},
		{"erase to end of line", "\x1b[1;3H\x1b[K"},
		{"scroll by newlines", "\x1b[8;1Hbottom\r\nnew1\r\nnew2 \x1b[44mblue\x1b[m"},
		{"clear screen", "\x1b[2J\x1b[4;4Hafter clear"},
		{"enter alternate screen", "\x1b[?1049h\x1b[1;1Htui frame\x1b[8;30H#"},
		{"cursor move only", "\x1b[5;7H"},
		{"leave alternate screen", "\x1b[?1049l"},
		{"shrinking wide text", "\x1b[3;1H日本語\x1b[3;1Hab\x1b[K"},
		{"RIS", "junk\x1bcfresh"},
	}
	for _, st := range steps {
		s.Write([]byte(st.out))
		c.write(d.Frame())
		assertVisible(t, st.name, s, c)
	}
	if f := d.Frame(); f != nil {
		t.Fatalf("frame without changes: %q", f)
	}
}

func TestDiffFrameRedrawsOnlyChangedRows(t *testing.T) {
	s := New(20, 6, nil)
	defer s.Close()
	for i := range 6 {
		fmt.Fprintf(&writerFunc{s.Write}, "\x1b[%d;1Hline-%d", i+1, i)
	}
	_, d := s.SnapshotDiffer()
	s.Write([]byte("\x1b[4;1HCHANGED"))
	f := d.Frame()
	if !bytes.Contains(f, []byte("CHANGED")) {
		t.Fatalf("frame lacks the changed row: %q", f)
	}
	for _, other := range []string{"line-0", "line-1", "line-2", "line-4", "line-5"} {
		if bytes.Contains(f, []byte(other)) {
			t.Fatalf("frame redraws unchanged %q: %q", other, f)
		}
	}
}

func TestResizeRedrawsEveryRow(t *testing.T) {
	s := New(20, 5, nil)
	defer s.Close()
	s.Write([]byte("a\r\nb\r\nc"))
	frame, d := s.SnapshotDiffer()
	c := newClient(t, 20, 5)
	c.write(frame)

	if cols, rows := s.Resize(25, 7); cols != 25 || rows != 7 {
		t.Fatalf("Resize → %dx%d", cols, rows)
	}
	c.emu.Resize(25, 7)
	c.write([]byte("\x1b[3;3Hstale client junk")) // whatever the client had must go
	c.write(d.Frame())
	assertVisible(t, "after resize", s, c)
}

func TestModesAndTitle(t *testing.T) {
	s := New(20, 5, nil)
	defer s.Close()
	s.Write([]byte("\x1b[?2004h\x1b[?1h\x1b]2;my title\x07"))
	if !s.BracketedPaste() || !s.AppCursorKeys() || s.Title() != "my title" {
		t.Fatalf("paste=%v appcursor=%v title=%q", s.BracketedPaste(), s.AppCursorKeys(), s.Title())
	}
	s.Write([]byte("\x1b[?2004l"))
	if s.BracketedPaste() {
		t.Fatal("bracketed paste still on after DECRST 2004")
	}
	s.Write([]byte("\x1b[?2004h\x1bc"))
	if s.BracketedPaste() || s.AppCursorKeys() {
		t.Fatal("modes survive RIS")
	}
}

func TestQueriesAreAnsweredWithoutBlockingWrite(t *testing.T) {
	replies := make(chan []byte, 4)
	s := New(20, 5, func(b []byte) { replies <- b })
	defer s.Close()
	s.Write([]byte("\x1b[3;7H\x1b[6n"))
	select {
	case r := <-replies:
		if string(r) != "\x1b[3;7R" {
			t.Fatalf("CPR reply %q", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply to CPR")
	}

	// Without a responder (passthrough) queries are drained and dropped.
	p := New(20, 5, nil)
	defer p.Close()
	done := make(chan struct{})
	go func() {
		for range 50 {
			p.Write([]byte("\x1b[6n\x1b[c"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on unanswered queries")
	}
}

func TestNoEmulatorScrollbackAccumulates(t *testing.T) {
	s := New(20, 5, nil)
	defer s.Close()
	if !s.in.ok() {
		t.Fatal("vt internals not reachable: emulator scrollback cannot be disabled")
	}
	var out strings.Builder
	for i := range 200 {
		fmt.Fprintf(&out, "main %d\r\n", i)
	}
	out.WriteString("\x1b[?1049h")
	for i := range 200 {
		fmt.Fprintf(&out, "alt %d\r\n\x1b[2J", i)
	}
	s.Write([]byte(out.String()))
	for i := range 2 {
		if sb := s.in.scrs[i].Scrollback(); sb.Len() != 0 {
			t.Fatalf("screen %d holds %d scrollback lines", i, sb.Len())
		}
	}
}

func TestClampSize(t *testing.T) {
	for _, tc := range []struct{ c, r, wc, wr int }{
		{0, 0, DefaultCols, DefaultRows},
		{1, 1, MinCols, MinRows},
		{10000, 10000, MaxCols, MaxRows},
		{120, 40, 120, 40},
	} {
		if c, r := ClampSize(tc.c, tc.r); c != tc.wc || r != tc.wr {
			t.Errorf("ClampSize(%d,%d) = %d,%d want %d,%d", tc.c, tc.r, c, r, tc.wc, tc.wr)
		}
	}
}

type writerFunc struct{ f func([]byte) }

func (w *writerFunc) Write(p []byte) (int, error) { w.f(p); return len(p), nil }

// Claude Code sets titles like "✳ Claude Code"; the 0x9C inside "✳" must not
// end the OSC early (the ansi parser treats it as C1 ST), which used to
// leave the title as "\xe2" and print the rest of the payload on screen.
func TestUTF8TitleIsNotCutByC1Bytes(t *testing.T) {
	s := New(40, 5, nil)
	defer s.Close()
	seq := []byte("\x1b]0;\u2733 Claude Code\x07ok \x1b]2;\u65e5\u672c\u8a9e\x1b\\done")
	for i := range seq { // byte by byte: sequences split across reads
		s.Write(seq[i : i+1])
	}
	if got := s.Title(); got != "日本語" {
		t.Fatalf("title = %q", got)
	}
	s.mu.Lock()
	text := s.emu.String()
	s.mu.Unlock()
	if strings.Contains(text, "Claude") || !strings.Contains(text, "ok done") {
		t.Fatalf("screen text = %q", text)
	}
}
