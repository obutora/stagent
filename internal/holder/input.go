package holder

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// submitGap separates typed/pasted text from the Enter that submits it.
// TUIs that detect pastes by timing (without bracketed paste) would
// otherwise take a CR arriving in the same read as part of the text.
const submitGap = 30 * time.Millisecond

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

// composeInput turns session.input params into PTY writes: text raw, paste
// (newlines as CR, like a terminal pastes; bracketed when the program
// enabled it), keys, then CR after a short gap if submit.
func composeInput(p wire.InputParams, bracketed, appCursor bool) ([]inputChunk, error) {
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
		req = append(req, inputChunk{data: []byte(b.String())})
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
