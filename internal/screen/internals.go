package screen

import (
	"reflect"
	"unsafe"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// internals reaches two unexported fields of vt.Emulator (version pinned in
// go.mod) that its API does not expose:
//
//   - scrs [2]vt.Screen: the emulator gives both the main and the alternate
//     screen a 10,000-line scrollback and SetScrollbackSize only reaches the
//     main one. A holder keeps scrollback as raw bytes on disk, so both are
//     disabled through the exported (*vt.Screen).SetScrollback(nil) — a
//     full-screen TUI clearing the alternate screen would otherwise pile up
//     tens of MiB of cells.
//   - scr *vt.Screen: the active screen, for the cursor pen and scroll
//     region that a raw-mode snapshot must restore.
//   - parser *ansi.Parser: NewEmulator sizes its string-sequence buffer at
//     4 MiB per session; it is set back to the ansi default (64 KiB) through
//     the exported SetDataSize. Longer OSC/DCS payloads (large OSC 52
//     clipboard copies, inline images) are not interpreted by the screen
//     anyway; the raw stream still carries them.
//
// If a future vt changes these fields, reach reports false and the screen
// degrades to a 1-line main scrollback, the large parser buffer and
// snapshots without pen/scroll region; TestNoEmulatorScrollbackAccumulates
// fails loudly in that case.
type internals struct {
	scrs   *[2]vt.Screen
	scr    **vt.Screen
	parser *ansi.Parser
}

func reach(e *vt.Emulator) (internals, bool) {
	v := reflect.ValueOf(e).Elem()
	scrs := v.FieldByName("scrs")
	scr := v.FieldByName("scr")
	parser := v.FieldByName("parser")
	if !scrs.IsValid() || scrs.Type() != reflect.TypeFor[[2]vt.Screen]() ||
		!scr.IsValid() || scr.Type() != reflect.TypeFor[*vt.Screen]() ||
		!parser.IsValid() || parser.Type() != reflect.TypeFor[*ansi.Parser]() {
		return internals{}, false
	}
	return internals{
		scrs:   (*[2]vt.Screen)(unsafe.Pointer(scrs.UnsafeAddr())),
		scr:    (**vt.Screen)(unsafe.Pointer(scr.UnsafeAddr())),
		parser: *(**ansi.Parser)(unsafe.Pointer(parser.UnsafeAddr())),
	}, true
}

func (in internals) ok() bool { return in.scrs != nil }

// active returns the active screen.
func (in internals) active() *vt.Screen { return *in.scr }

// main returns the main (non-alternate) screen.
func (in internals) main() *vt.Screen { return &in.scrs[0] }
