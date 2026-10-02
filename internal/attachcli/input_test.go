package attachcli

import (
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const ctrlBracket = 0x1d

// step is one event for an input: a read at time at, or (read empty) the
// timer firing at at.
type step struct {
	at   time.Duration
	read string
}

// run drives an input through steps and returns what was sent and whether
// it detached (and stops there).
func run(steps []step) (string, bool) {
	t0 := time.Unix(1000, 0)
	in := newInput(ctrlBracket)
	var sent strings.Builder
	for _, s := range steps {
		var text []byte
		var detach bool
		if s.read != "" {
			text, detach = in.feed([]byte(s.read), t0.Add(s.at))
		} else {
			text, detach = in.expire(t0.Add(s.at))
		}
		sent.Write(text)
		if detach {
			return sent.String(), true
		}
	}
	return sent.String(), false
}

func TestInputDetachKey(t *testing.T) {
	ms := time.Millisecond
	cases := []struct {
		name   string
		steps  []step
		sent   string
		detach bool
	}{
		{"one press detaches once the window passes; earlier text is sent",
			[]step{{0, "ab\x1d"}, {299 * ms, ""}, {300 * ms, ""}}, "ab", true},
		{"two presses in one read send the key once",
			[]step{{0, "a\x1d\x1db"}, {time.Second, ""}}, "a\x1db", false},
		{"two presses in separate reads send the key once",
			[]step{{0, "\x1d"}, {100 * ms, "\x1dc"}, {time.Second, ""}}, "\x1dc", false},
		{"three presses: one literal, then armed again",
			[]step{{0, "\x1d\x1d\x1d"}, {300 * ms, ""}}, "\x1d", true},
		{"another key after the press detaches and is discarded",
			[]step{{0, "\x1d"}, {10 * ms, "x"}}, "", true},
		{"another key in the same read detaches and is discarded",
			[]step{{0, "q\x1dxyz"}}, "q", true},
		{"a query reply after the press neither detaches nor cancels",
			[]step{{0, "\x1d"}, {10 * ms, "\x1b[3;4R"}, {20 * ms, "\x1d"}, {time.Second, ""}}, "\x1d", false},
		{"a bare ESC after the press detaches when it is released",
			[]step{{0, "\x1d"}, {10 * ms, "\x1b"}, {60 * ms, ""}}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent, detach := run(tc.steps)
			if sent != tc.sent || detach != tc.detach {
				t.Fatalf("sent %q detach %v, want %q detach %v", sent, detach, tc.sent, tc.detach)
			}
		})
	}
}

func TestInputHeldBytes(t *testing.T) {
	ms := time.Millisecond
	a := "あ" // 3 bytes
	cases := []struct {
		name  string
		steps []step
		sent  string
	}{
		{"a rune split across reads is sent whole",
			[]step{{0, "x" + a[:1]}, {1 * ms, a[1:2]}, {2 * ms, a[2:] + "y"}}, "x" + a + "y"},
		{"a bare ESC goes out after the flush delay, not before",
			[]step{{0, "\x1b"}, {49 * ms, ""}, {50 * ms, ""}}, "\x1b"},
		{"a cut-off rune goes out after the flush delay",
			[]step{{0, a[:2]}, {50 * ms, ""}}, a[:2]},
		{"held ESC then a cut-off rune keep their order",
			[]step{{0, a[:2]}, {1 * ms, a[2:] + "\x1b"}, {51 * ms, ""}}, a + "\x1b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sent, detach := run(tc.steps)
			if sent != tc.sent || detach {
				t.Fatalf("sent %q detach %v, want %q", sent, detach, tc.sent)
			}
		})
	}

	// Bytes held after a press must not postpone the detach.
	in := newInput(ctrlBracket)
	t0 := time.Unix(1000, 0)
	in.feed([]byte("\x1d"), t0)
	in.feed([]byte("\x1b"), t0.Add(280*time.Millisecond))
	if d, ok := in.deadline(); !ok || !d.Equal(t0.Add(detachWindow)) {
		t.Fatalf("deadline %v %v, want the end of the detach window", d, ok)
	}
}

func TestParseDetachKey(t *testing.T) {
	good := map[string]byte{"ctrl-]": 0x1d, "ctrl-a": 0x01, "CTRL-Q": 0x11, "ctrl-@": 0x00, "ctrl-\\": 0x1c, "ctrl-^": 0x1e, "ctrl-_": 0x1f}
	for s, want := range good {
		if got, err := parseDetachKey(s); err != nil || got != want {
			t.Errorf("%q: %#x %v, want %#x", s, got, err, want)
		}
	}
	for _, s := range []string{"ctrl-[", "ctrl-1", "ctrl-", "ctrl-ab", "alt-a", "]", "ctrl-?"} {
		if _, err := parseDetachKey(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestPickSession(t *testing.T) {
	code := 0
	ended := wire.Session{ID: "00000000000000e1", ExitCode: &code, LastActivityAt: 900}
	older := wire.Session{ID: "00000000000000a1", LastActivityAt: 100, Command: []string{"sh"}}
	newer := wire.Session{ID: "00000000000000a2", LastActivityAt: 500, Command: []string{"claude"}}
	now := time.UnixMilli(1000)

	if id, err := pickSession([]wire.Session{ended, older}, false, "", now, ""); err != nil || id != older.ID {
		t.Fatalf("only live: %q %v", id, err)
	}
	list := []wire.Session{ended, newer, older}
	if id, err := pickSession(list, true, "", now, ""); err != nil || id != newer.ID {
		t.Fatalf("--last: %q %v", id, err)
	}
	_, err := pickSession(list, false, "", now, "")
	if _, ok := err.(*errChoose); !ok || !strings.Contains(err.Error(), newer.ID) || !strings.Contains(err.Error(), older.ID) || strings.Contains(err.Error(), ended.ID) {
		t.Fatalf("ambiguous: %v", err)
	}
	// Run from a shell inside newer: the enclosing session is never picked.
	for _, last := range []bool{false, true} {
		if id, err := pickSession(list, last, newer.ID, now, ""); err != nil || id != older.ID {
			t.Fatalf("self excluded (last=%v): %q %v", last, id, err)
		}
		if _, err := pickSession([]wire.Session{ended, newer}, last, newer.ID, now, ""); err == nil {
			t.Fatalf("only self live accepted (last=%v)", last)
		}
		if _, err := pickSession([]wire.Session{ended}, last, "", now, ""); err == nil {
			t.Fatalf("no live session accepted (last=%v)", last)
		}
	}
}
