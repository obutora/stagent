package holder

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func TestComposeInput(t *testing.T) {
	cases := []struct {
		name                     string
		p                        wire.InputParams
		bracketed, appCur, win32 bool
		want                     []inputChunk
	}{
		{"text, keys and submit keep their order; CR follows after a gap",
			wire.InputParams{Text: "ab", Keys: []string{"left", "ctrl-c"}, Submit: true}, false, false, false,
			[]inputChunk{{data: []byte("ab\x1b[D\x03")}, {data: []byte("\r"), delay: submitGap}}},
		{"submit alone is immediate",
			wire.InputParams{Submit: true}, false, false, false,
			[]inputChunk{{data: []byte("\r")}}},
		{"paste is bracketed only when the program enabled it",
			wire.InputParams{Paste: "l1\nl2\r\nl3"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~l1\rl2\rl3\x1b[201~")}}},
		{"paste without bracketed paste mode",
			wire.InputParams{Paste: "l1\nl2"}, false, false, false,
			[]inputChunk{{data: []byte("l1\rl2")}}},
		{"a pasted end marker cannot close the bracket early",
			wire.InputParams{Paste: "x\x1b[201~rm -rf ~\n"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~xrm -rf ~\r\x1b[201~")}}},
		{"an end marker that removing one would form is removed too",
			wire.InputParams{Paste: "\x1b[20\x1b[201~1~"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~\x1b[201~")}}},
		{"end markers nested in end markers are all removed",
			wire.InputParams{Paste: "a\x1b[20\x1b[20\x1b[201~1~1~b\x1b[201\x1b[201~~"}, true, false, false,
			[]inputChunk{{data: []byte("\x1b[200~ab\x1b[201~")}}},
		{"a chat message's CR is marked",
			wire.InputParams{Paste: "hi", Submit: true}, false, false, false,
			[]inputChunk{{data: []byte("hi")}, {data: []byte("\r"), delay: submitGap, submit: true}}},
		{"cursor keys follow DECCKM",
			wire.InputParams{Keys: []string{"up", "end", "tab"}}, false, true, false,
			[]inputChunk{{data: []byte("\x1bOA\x1bOF\t")}}},
		{"a lone Esc stays a byte outside win32-input-mode",
			wire.InputParams{Keys: []string{"esc"}}, false, false, false,
			[]inputChunk{{data: []byte("\x1b")}}},
		{"win32-input-mode: lone Escs become key events, sequences stay",
			wire.InputParams{Text: "\x1b\x1b", Keys: []string{"up", "esc"}}, false, false, true,
			[]inputChunk{{data: []byte(escKeyEvent + escKeyEvent + "\x1b[A" + escKeyEvent)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := composeInput(tc.p, tc.bracketed, tc.appCur, tc.win32)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	_, err := composeInput(wire.InputParams{Text: "x", Keys: []string{"f13"}}, false, false, false)
	if we, ok := err.(*wire.Error); !ok || we.Code != wire.ErrBadRequest {
		t.Fatalf("unknown key error = %v, want bad_request", err)
	}
}

// ptyRecorder records what the input queue writes; written runs on the
// queue's goroutine after each write (the program reacting to it).
type ptyRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written func(b []byte)
}

func (r *ptyRecorder) Write(b []byte) (int, error) {
	r.mu.Lock()
	r.buf.Write(b)
	r.mu.Unlock()
	if r.written != nil {
		r.written(b)
	}
	return len(b), nil
}

func (r *ptyRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitWritten waits until everything written ends with suffix.
func (r *ptyRecorder) waitWritten(t *testing.T, suffix string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !strings.HasSuffix(r.String(), suffix) {
		if time.Now().After(deadline) {
			t.Fatalf("written %q, want it to end with %q", r.String(), suffix)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// heldQueue runs an input queue whose program shows a permission menu
// (menu) as soon as the chat message "msg" was pasted, i.e. during
// submitGap, and sends it that message.
func heldQueue(t *testing.T) (*inputQueue, *ptyRecorder, *atomic.Bool) {
	t.Helper()
	var menu atomic.Bool
	rec := &ptyRecorder{written: func(b []byte) {
		if string(b) == "msg" {
			menu.Store(true)
		}
	}}
	q := newInputQueue(rec)
	q.menuUp = menu.Load
	go q.run()
	t.Cleanup(q.close)
	queueInput(t, q, wire.InputParams{Paste: "msg", Submit: true})
	time.Sleep(submitGap + 3*heldSubmitPoll)
	if got := rec.String(); got != "msg" {
		t.Fatalf("written %q while the menu is up, want the message without its Enter", got)
	}
	return q, rec, &menu
}

func queueInput(t *testing.T, q *inputQueue, p wire.InputParams) {
	t.Helper()
	req, err := composeInput(p, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.push(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

// A chat message's Enter that would land on a permission menu shown after
// the message was pasted is held: the menu's answer is typed meanwhile,
// and the Enter follows once, after the menu stayed gone promptGoneAfter.
func TestHeldSubmitWaitsForMenuToGo(t *testing.T) {
	q, rec, menu := heldQueue(t)
	queueInput(t, q, wire.InputParams{Text: "1"}) // the answer, while the menu is up
	rec.waitWritten(t, "1")
	time.Sleep(2 * promptGoneAfter)
	if got := rec.String(); got != "msg1" {
		t.Fatalf("written %q while the menu is up, want %q", got, "msg1")
	}

	menu.Store(false)
	gone := time.Now()
	q.tryPush([]byte("\x1b[?1;2c")) // a query reply is no input
	rec.waitWritten(t, "\r")
	if d := time.Since(gone); d < promptGoneAfter {
		t.Fatalf("Enter written %v after the menu went, want at least %v", d, promptGoneAfter)
	}
	time.Sleep(2 * promptGoneAfter)
	if got, want := rec.String(), "msg1\x1b[?1;2c\r"; got != want {
		t.Fatalf("written %q, want %q (one Enter)", got, want)
	}
}

// A menu that comes back before promptGoneAfter passed (claude's next
// parallel prompt) keeps the Enter held.
func TestHeldSubmitWaitsThroughNextMenu(t *testing.T) {
	_, rec, menu := heldQueue(t)
	menu.Store(false)
	time.Sleep(promptGoneAfter / 2)
	menu.Store(true)
	time.Sleep(promptGoneAfter)
	if got := rec.String(); got != "msg" {
		t.Fatalf("written %q with the next menu up, want %q", got, "msg")
	}
	menu.Store(false)
	rec.waitWritten(t, "msg\r")
}

// Input after the menu went drops the held Enter: the message stays in the
// input box with what was typed.
func TestHeldSubmitDroppedByLaterInput(t *testing.T) {
	q, rec, menu := heldQueue(t)
	menu.Store(false)
	time.Sleep(2 * heldSubmitPoll)
	queueInput(t, q, wire.InputParams{Text: "x"})
	time.Sleep(3 * promptGoneAfter)
	if got := rec.String(); got != "msgx" {
		t.Fatalf("written %q, want %q (no Enter)", got, "msgx")
	}
}

// Text is typed whatever the screen shows; only a chat message's Enter
// waits for the menu.
func TestTextNotHeldByMenu(t *testing.T) {
	var menu atomic.Bool
	menu.Store(true)
	rec := &ptyRecorder{}
	q := newInputQueue(rec)
	q.menuUp = menu.Load
	go q.run()
	t.Cleanup(q.close)
	queueInput(t, q, wire.InputParams{Text: "2", Submit: true})
	rec.waitWritten(t, "2\r")
}
