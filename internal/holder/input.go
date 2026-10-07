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

// heldSubmitPoll is how often the screen is read while a chat message's
// Enter is held (inputQueue.held).
const heldSubmitPoll = 50 * time.Millisecond

// inputChunk is written to the PTY after waiting delay.
type inputChunk struct {
	data  []byte
	delay time.Duration
	// message marks the text of a chat message (session.input with paste
	// and submit): if a menu is on the screen when its turn comes, nothing
	// of its request is written.
	message bool
	// submit marks the Enter of a chat message: it is held while a menu is
	// on the screen.
	submit bool
}

// inputRequest is one writer's input; its chunks are written back to back.
type inputRequest struct {
	chunks []inputChunk
	// sent, if not nil, gets whether the request was written (false: a
	// chat message dropped for a menu). Buffered, so run never waits.
	sent chan<- bool
}

// inputQueue serializes every writer of program input — the app, the local
// terminal and the emulator's query replies — through one goroutine, so a
// program that stops reading its input never blocks a caller that holds a
// lock, and each request's chunks stay contiguous.
//
// A chat message must not land on a menu (Holder.menuShown): the menu's
// keys would take its text, its Enter pick the option under the cursor.
// session.input refuses one while a menu is up, and the queue looks again
// when the message's turn comes: a menu shown meanwhile drops the whole
// message.
// A menu shown after its text was written holds its Enter instead, which
// is written once the menu has been off the screen for promptGoneAfter (as
// holder.prompt_gone: claude shows parallel tool calls' prompts one after
// the other). Input that arrives meanwhile keeps the hold while the menu
// is up (an answer to it) and drops the held Enter once the menu is gone:
// the user went on editing, and the message stays in the input box.
type inputQueue struct {
	w         io.Writer
	ch        chan inputRequest
	quit      chan struct{}
	closeOnce sync.Once
	// menuUp reports whether a menu is on the screen (nil: never). Set
	// before run.
	menuUp func() bool

	// The held Enter (run goroutine only): held, and when the menu was
	// first seen gone (zero: it is up).
	held      bool
	goneSince time.Time
}

func newInputQueue(w io.Writer) *inputQueue {
	return &inputQueue{w: w, ch: make(chan inputRequest, 64), quit: make(chan struct{})}
}

func (q *inputQueue) run() {
	var poll *time.Ticker
	defer func() {
		if poll != nil {
			poll.Stop()
		}
	}()
	for {
		var tick <-chan time.Time
		switch {
		case q.held && poll == nil:
			poll = time.NewTicker(heldSubmitPoll)
			fallthrough
		case q.held:
			tick = poll.C
		case poll != nil:
			poll.Stop()
			poll = nil
		}
		select {
		case <-q.quit:
			return
		case <-tick:
			// Queued input first: once the menu is gone it drops the held
			// Enter, which the tick would otherwise write.
			if len(q.ch) > 0 {
				continue
			}
			// Not the tick's time: a slow write may have delayed this
			// tick, and the menu must be seen gone for promptGoneAfter.
			q.checkHeld(time.Now())
		case req := <-q.ch:
			if q.held && typedRequest(req.chunks) && !q.menuShown() {
				q.held = false // input after the menu went: drop the Enter
			}
			sent := true
			for _, c := range req.chunks {
				if c.delay > 0 {
					select {
					case <-time.After(c.delay):
					case <-q.quit:
						return
					}
				}
				if c.message && q.menuShown() {
					sent = false
					break
				}
				if c.submit && q.menuShown() {
					q.held, q.goneSince = true, time.Time{}
					continue
				}
				// Errors mean the program is gone; the exit path follows.
				writeAll(q.w, c.data)
			}
			if req.sent != nil {
				req.sent <- sent
			}
		}
	}
}

// checkHeld writes the held Enter once the menu has been off the screen
// for promptGoneAfter.
func (q *inputQueue) checkHeld(now time.Time) {
	switch {
	case q.menuShown():
		q.goneSince = time.Time{}
	case q.goneSince.IsZero():
		q.goneSince = now
	case now.Sub(q.goneSince) >= promptGoneAfter:
		q.held = false
		writeAll(q.w, []byte{'\r'})
	}
}

func (q *inputQueue) menuShown() bool {
	return q.menuUp != nil && q.menuUp()
}

// typedRequest reports whether req holds anything typed, as opposed to
// replies to the program's queries and focus or mouse reports.
func typedRequest(req []inputChunk) bool {
	for _, c := range req {
		if isTyping(c.data) {
			return true
		}
	}
	return false
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
func (q *inputQueue) push(ctx context.Context, chunks []inputChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	return q.enqueue(ctx, inputRequest{chunks: chunks})
}

// pushMessage queues a chat message (composeInput with paste and submit);
// the channel then tells whether its text was written or dropped for a
// menu.
func (q *inputQueue) pushMessage(ctx context.Context, chunks []inputChunk) (<-chan bool, error) {
	sent := make(chan bool, 1)
	return sent, q.enqueue(ctx, inputRequest{chunks: chunks, sent: sent})
}

func (q *inputQueue) enqueue(ctx context.Context, req inputRequest) error {
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
	case q.ch <- inputRequest{chunks: []inputChunk{{data: b}}}:
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

// stripPasteEnd removes every pasteEnd from s, including those that removing
// one joins from the bytes around it (`\x1b[20\x1b[201~1~`), in one pass:
// each byte goes on a stack, and a marker completed on top is popped.
// Removing them one at a time would take quadratic time on nested markers.
func stripPasteEnd(s string) string {
	if !strings.Contains(s, pasteEnd) {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := range len(s) {
		b = append(b, s[i])
		if len(b) >= len(pasteEnd) && string(b[len(b)-len(pasteEnd):]) == pasteEnd {
			b = b[:len(b)-len(pasteEnd)]
		}
	}
	return string(b)
}

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
// enabled it), keys, then CR after a short gap if submit; with a paste these
// are a chat message's text (inputChunk.message) and Enter
// (inputChunk.submit). While the program asks for win32-input-mode a lone
// Esc becomes that mode's key event.
func composeInput(p wire.InputParams, bracketed, appCursor, win32Input bool) ([]inputChunk, error) {
	var b strings.Builder
	b.WriteString(p.Text)
	if p.Paste != "" {
		paste := strings.ReplaceAll(p.Paste, "\r\n", "\r")
		paste = strings.ReplaceAll(paste, "\n", "\r")
		if bracketed {
			// A pasted end marker would let the text escape the paste.
			paste = stripPasteEnd(paste)
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
		req = append(req, inputChunk{data: data, message: p.Paste != "" && p.Submit})
	}
	if p.Submit {
		c := inputChunk{data: []byte{'\r'}, submit: p.Paste != ""}
		if len(req) > 0 {
			c.delay = submitGap
		}
		req = append(req, c)
	}
	return req, nil
}
