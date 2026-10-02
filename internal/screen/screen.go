// Package screen keeps the visible terminal screen of a session in a VT
// emulator (charmbracelet/x/vt) and renders it for attached clients: a full
// ANSI snapshot to resynchronize a client, and per-client diffs that redraw
// only the rows changed since that client's previous frame.
//
// Only the visible screen is kept in memory; scrollback lives on disk as raw
// bytes (package scrollback).
package screen

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Size limits. Both screens (main and alternate) hold one cell struct per
// position, so an absurd size requested by a client must not become an
// absurd allocation.
const (
	MinCols, MaxCols = 2, 500
	MinRows, MaxRows = 2, 250
	DefaultCols      = 80
	DefaultRows      = 24
)

// parserDataSize bounds one OSC/DCS/APC payload the emulator interprets.
const parserDataSize = 64 << 10

// ClampSize returns the size a screen (and the PTY) actually uses; zero or
// negative values mean the default.
func ClampSize(cols, rows int) (int, int) {
	if cols <= 0 {
		cols = DefaultCols
	}
	if rows <= 0 {
		rows = DefaultRows
	}
	return min(max(cols, MinCols), MaxCols), min(max(rows, MinRows), MaxRows)
}

// Screen is a session's emulated terminal screen. All methods are safe for
// concurrent use.
type Screen struct {
	mu         sync.Mutex
	emu        *vt.Emulator
	in         internals
	cols, rows int

	// State tracked through emulator callbacks (title: through guard).
	title        string
	input        uint16 // bit i: inputModes[i] is set
	noAutowrap   bool
	cursorHidden bool

	respond atomic.Pointer[func([]byte)] // see SetResponder

	// Damage tracking: ver increases with every Write that changed cells;
	// rowVer[y] is the ver that last changed row y. A client that has seen
	// ver v needs every row with rowVer > v.
	ver       uint64
	rowVer    []uint64
	touchBase **uv.LineData // backing array of the emulator's Touched list
	full      bool          // invalidate every row at the next collect

	guard stringGuard // keeps UTF-8 OSC/DCS payloads away from the parser
	buf   []byte      // scratch for filtered output
}

// inputModes are the DEC private modes that change what a terminal sends as
// input: cursor key form, mouse tracking and its encodings, focus events and
// bracketed paste. A raw-mode snapshot sets or resets every one of them, so
// a client whose terminal was left in another state (a different program, a
// reconnect) sends the program input in the form it asked for. Tracking
// modes are listed in increasing order of what they report: terminals keep
// only one of them active, the last set.
var inputModes = [...]ansi.DECMode{
	ansi.ModeCursorKeys,
	ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent,
	ansi.ModeMouseExtUtf8, ansi.ModeMouseExtSgr, ansi.ModeMouseExtUrxvt,
	ansi.ModeFocusEvent,
	ansi.ModeBracketedPaste,
}

// New returns a screen of the given size. respond receives the emulator's
// replies to terminal queries (DA, DSR/CPR, DECRQM, colour queries, ...);
// pass nil when a real terminal answers them (passthrough), otherwise
// forward them to the program (detached). respond must not block for long
// and must not call into the Screen.
func New(cols, rows int, respond func([]byte)) *Screen {
	cols, rows = ClampSize(cols, rows)
	e := vt.NewEmulator(cols, rows)
	s := &Screen{emu: e, cols: cols, rows: rows, rowVer: make([]uint64, rows)}
	if in, ok := reach(e); ok {
		s.in = in
		in.scrs[0].SetScrollback(nil)
		in.scrs[1].SetScrollback(nil)
		in.parser.SetDataSize(parserDataSize)
	} else {
		e.SetScrollbackSize(1)
	}
	s.SetResponder(respond)
	s.guard.onTitle = func(t string) { s.title = t } // runs inside Write, s.mu held
	// Callbacks run inside emu.Write, i.e. with s.mu held.
	e.SetCallbacks(vt.Callbacks{
		AltScreen:        func(bool) { s.full = true },
		CursorVisibility: func(visible bool) { s.cursorHidden = !visible },
		EnableMode:       func(m ansi.Mode) { s.setMode(m, true) },
		DisableMode:      func(m ansi.Mode) { s.setMode(m, false) },
	})
	// RIS clears both screens and drops their touched lists; the default
	// handler still runs (false = not handled) and resets the modes vt
	// knows through DisableMode, but not the mouse encodings it does not
	// track (?1005, ?1015).
	e.RegisterEscHandler('c', func() bool {
		s.full = true
		s.input = 0
		return false
	})
	go s.drain()
	return s
}

// SetResponder replaces the receiver of terminal query replies (see New);
// nil drops them. A passthrough session whose local terminal went away
// starts answering queries itself this way.
func (s *Screen) SetResponder(respond func([]byte)) {
	if respond == nil {
		s.respond.Store(nil)
		return
	}
	s.respond.Store(&respond)
}

// drain consumes the emulator's reply pipe. The emulator writes replies
// synchronously while parsing, so an unread pipe would block Write.
func (s *Screen) drain() {
	buf := make([]byte, 512)
	for {
		n, err := s.emu.Read(buf)
		if n > 0 {
			if respond := s.respond.Load(); respond != nil {
				(*respond)(bytes.Clone(buf[:n]))
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Screen) setMode(m ansi.Mode, on bool) {
	if m == ansi.ModeAutoWrap {
		s.noAutowrap = !on
		return
	}
	for i, im := range inputModes {
		if m == im {
			if on {
				s.input |= 1 << i
			} else {
				s.input &^= 1 << i
			}
			return
		}
	}
}

// modeOn reports whether input mode m is set (s.mu held).
func (s *Screen) modeOn(m ansi.DECMode) bool {
	for i, im := range inputModes {
		if im == m {
			return s.input&(1<<i) != 0
		}
	}
	return false
}

// Write feeds program output to the emulator.
func (s *Screen) Write(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = s.guard.filter(s.buf[:0], p)
	if len(s.buf) > 0 {
		s.emu.Write(s.buf)
	}
	if cap(s.buf) > 64<<10 {
		s.buf = nil // do not pin a burst-sized buffer
	}
	s.collect()
}

// Resize changes the screen size (clamped with ClampSize) and returns the
// size in effect.
func (s *Screen) Resize(cols, rows int) (int, int) {
	cols, rows = ClampSize(cols, rows)
	s.mu.Lock()
	defer s.mu.Unlock()
	if cols == s.cols && rows == s.rows {
		return cols, rows
	}
	s.emu.Resize(cols, rows)
	s.cols, s.rows = cols, rows
	s.rowVer = make([]uint64, rows)
	s.full = true
	s.collect()
	return cols, rows
}

// Size returns the current size.
func (s *Screen) Size() (cols, rows int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cols, s.rows
}

// Title is the window title last set with OSC 0 or 2.
func (s *Screen) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// BracketedPaste reports whether the program enabled DECSET 2004.
func (s *Screen) BracketedPaste() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modeOn(ansi.ModeBracketedPaste)
}

// AppCursorKeys reports whether the program enabled DECCKM (cursor keys send
// SS3 sequences).
func (s *Screen) AppCursorKeys() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modeOn(ansi.ModeCursorKeys)
}

// Close stops the reply drain. The screen stays readable.
func (s *Screen) Close() {
	// Closing the pipe writer (not emu.Close) ends the drain goroutine
	// without racing on the emulator's own closed flag.
	if pw, ok := s.emu.InputPipe().(*io.PipeWriter); ok {
		pw.CloseWithError(io.EOF)
	}
}

// collect turns the emulator's touched-line list into row versions and
// clears it. A replaced or emptied list (resize, RIS, switching between the
// main and alternate screen) means rows changed without being listed, so
// every row is invalidated.
func (s *Screen) collect() {
	t := s.emu.Touched()
	var base **uv.LineData
	if len(t) > 0 {
		base = &t[0]
	}
	full := s.full || base != s.touchBase
	s.full = false
	s.touchBase = base
	if full {
		s.ver++
		for y := range s.rowVer {
			s.rowVer[y] = s.ver
		}
		clear(t)
		return
	}
	bumped := false
	for y, ld := range t {
		if ld == nil {
			continue
		}
		t[y] = nil
		if y >= len(s.rowVer) {
			continue
		}
		if !bumped {
			s.ver++
			bumped = true
		}
		s.rowVer[y] = s.ver
	}
}

type cursorState struct {
	x, y   int
	hidden bool
	pen    uv.Style
	top    int // scroll region rows [top, bottom)
	bottom int
}

func (s *Screen) cursor() cursorState {
	if s.in.ok() {
		scr := s.in.active()
		c := scr.Cursor()
		r := scr.ScrollRegion()
		return cursorState{x: c.X, y: c.Y, hidden: c.Hidden, pen: c.Pen, top: r.Min.Y, bottom: r.Max.Y}
	}
	p := s.emu.CursorPosition()
	return cursorState{x: p.X, y: p.Y, hidden: s.cursorHidden, top: 0, bottom: s.rows}
}
