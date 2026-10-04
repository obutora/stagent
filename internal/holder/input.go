package holder

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// submitGap separates typed/pasted text from the Enter that submits it.
// TUIs that detect pastes by timing (without bracketed paste) would
// otherwise take a CR arriving in the same read as part of the text. On
// Windows a pseudo console hands programs key events, not a bracketed
// paste, and Codex takes an Enter within about 120 ms of fast input as a
// line break of the paste, so the gap is longer there.
var submitGap = func() time.Duration {
	if runtime.GOOS == "windows" {
		return 250 * time.Millisecond
	}
	return 30 * time.Millisecond
}()

var errInputClosed = errors.New("session input closed")

// inputChunk is written to the PTY after waiting delay.
type inputChunk struct {
	data  []byte
	delay time.Duration
}

// inputQueue serializes every writer of program input — the app, the local
// terminal and the emulator's query replies — through one goroutine, so a
// program that stops reading its input never blocks a caller that holds a
// lock, and each request's chunks stay contiguous.
type inputQueue struct {
	w         io.Writer
	ch        chan []inputChunk
	quit      chan struct{}
	closeOnce sync.Once
}

func newInputQueue(w io.Writer) *inputQueue {
	return &inputQueue{w: w, ch: make(chan []inputChunk, 64), quit: make(chan struct{})}
}

func (q *inputQueue) run() {
	for {
		select {
		case <-q.quit:
			return
		case req := <-q.ch:
			for _, c := range req {
				if c.delay > 0 {
					select {
					case <-time.After(c.delay):
					case <-q.quit:
						return
					}
				}
				// Errors mean the program is gone; the exit path follows.
				writeAll(q.w, c.data)
			}
		}
	}
}

func writeAll(w io.Writer, b []byte) {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return
		}
		b = b[n:]
	}
}

// push queues one request, waiting while the queue is full.
func (q *inputQueue) push(ctx context.Context, req []inputChunk) error {
	if len(req) == 0 {
		return nil
	}
	select {
	case q.ch <- req:
		return nil
	case <-q.quit:
		return errInputClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryPush queues b unless the queue is full (terminal query replies are
// best effort).
func (q *inputQueue) tryPush(b []byte) {
	select {
	case q.ch <- []inputChunk{{data: b}}:
	default:
	}
}

func (q *inputQueue) close() {
	q.closeOnce.Do(func() { close(q.quit) })
}

// Bracketed paste markers.
const (
	pasteStart = "\x1b[200~"
	pasteEnd   = "\x1b[201~"
)

// appCursorKeys are the SS3 forms of cursor keys a program that enabled
// DECCKM expects.
var appCursorKeys = map[string]string{
	"up": "\x1bOA", "down": "\x1bOB", "right": "\x1bOC", "left": "\x1bOD",
	"home": "\x1bOH", "end": "\x1bOF",
}

// escKeyEvent is the Esc key as a win32-input-mode key press and release
// (CSI Vk;Sc;Uc;Kd;Cs;Rc _).
const escKeyEvent = "\x1b[27;1;27;1;0;1_\x1b[27;1;27;0;0;1_"

// encodeLoneEsc rewrites each Esc of b that starts no sequence (the last
// byte, or followed by another Esc) as escKeyEvent. Behind a Windows pseudo
// console in win32-input-mode, Claude Code does not take a lone Esc byte as
// the Esc key, so the app's Esc button would not cancel its prompts. A
// terminal attached to the session sends that mode's sequences itself;
// session.input does not.
func encodeLoneEsc(b []byte) []byte {
	if bytes.IndexByte(b, 0x1b) < 0 {
		return b
	}
	out := make([]byte, 0, len(b)+len(escKeyEvent))
	for i, c := range b {
		if c == 0x1b && (i == len(b)-1 || b[i+1] == 0x1b) {
			out = append(out, escKeyEvent...)
			continue
		}
		out = append(out, c)
	}
	return out
}

// composeInput turns session.input params into PTY writes: text raw, paste
// (newlines as CR, like a terminal pastes; bracketed when the program
// enabled it), keys, then CR after a short gap if submit. While the program
// asks for win32-input-mode a lone Esc becomes that mode's key event.
func composeInput(p wire.InputParams, bracketed, appCursor, win32Input bool) ([]inputChunk, error) {
	var b strings.Builder
	b.WriteString(p.Text)
	if p.Paste != "" {
		paste := strings.ReplaceAll(p.Paste, "\r\n", "\r")
		paste = strings.ReplaceAll(paste, "\n", "\r")
		if bracketed {
			// A pasted end marker would let the text escape the paste.
			paste = strings.ReplaceAll(paste, pasteEnd, "")
			b.WriteString(pasteStart + paste + pasteEnd)
		} else {
			b.WriteString(paste)
		}
	}
	for _, k := range p.Keys {
		seq, ok := wire.KeySequences[k]
		if !ok {
			return nil, wire.Errorf(wire.ErrBadRequest, "unknown key %q", k)
		}
		if appCursor {
			if s, ok := appCursorKeys[k]; ok {
				seq = s
			}
		}
		b.WriteString(seq)
	}
	var req []inputChunk
	if b.Len() > 0 {
		data := []byte(b.String())
		if win32Input {
			data = encodeLoneEsc(data)
		}
		req = append(req, inputChunk{data: data})
	}
	if p.Submit {
		c := inputChunk{data: []byte{'\r'}}
		if len(req) > 0 {
			c.delay = submitGap
		}
		req = append(req, c)
	}
	return req, nil
}
