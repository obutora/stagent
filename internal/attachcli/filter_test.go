package attachcli

import (
	"strings"
	"testing"
)

// feedAll runs reads through a fresh filter and returns its output and
// whether bytes are still held back.
func feedAll(reads ...string) (string, bool) {
	var f replyFilter
	var out []byte
	for _, r := range reads {
		out = f.filter(out, []byte(r))
	}
	return string(out), f.holding()
}

var replies = []struct{ name, seq string }{
	{"CPR", "\x1b[12;40R"},
	{"DECXCPR", "\x1b[?12;40R"},
	{"DECXCPR with page", "\x1b[?12;40;1R"},
	{"DA1", "\x1b[?62;22c"},
	{"DA1 short", "\x1b[?6c"},
	{"DA2", "\x1b[>1;10;0c"},
	{"DECRPM private", "\x1b[?2004;1$y"},
	{"DECRPM ANSI", "\x1b[4;2$y"},
	{"OSC 10 BEL", "\x1b]10;rgb:ffff/ffff/ffff\x07"},
	{"OSC 11 ST", "\x1b]11;rgb:0000/0000/0000\x1b\\"},
	{"OSC 12", "\x1b]12;rgb:aaaa/bbbb/cccc\x07"},
	{"OSC 4", "\x1b]4;1;rgb:cdcd/0000/0000\x1b\\"},
	{"XTVERSION", "\x1bP>|XTerm(388)\x1b\\"},
}

func TestFilterDropsRepliesAtEverySplit(t *testing.T) {
	for _, r := range replies {
		t.Run(r.name, func(t *testing.T) {
			for k := range len(r.seq) + 1 {
				out, held := feedAll("a"+r.seq[:k], r.seq[k:]+"b")
				if out != "ab" || held {
					t.Fatalf("split %d: out %q held %v, want \"ab\"", k, out, held)
				}
			}
			bytewise := strings.Split(r.seq, "")
			if out, held := feedAll(bytewise...); out != "" || held {
				t.Fatalf("byte by byte: out %q held %v", out, held)
			}
		})
	}
}

func TestFilterPassesKeys(t *testing.T) {
	keys := []struct{ name, seq string }{
		{"focus in", "\x1b[I"},
		{"focus out", "\x1b[O"},
		{"arrow", "\x1b[A"},
		{"SS3 arrow", "\x1bOA"},
		{"function key", "\x1b[15~"},
		{"ctrl+arrow", "\x1b[1;5A"},
		{"rxvt shift+right", "\x1b[c"},
		{"bracketed paste", "\x1b[200~p;1R\x1b[201~"},
		{"SGR mouse press", "\x1b[<0;10;5M"},
		{"SGR mouse release", "\x1b[<0;10;5m"},
		{"X10 mouse", "\x1b[M !!"},
		{"alt+x", "\x1bx"},
		{"alt+[ then text", "\x1b[x"},
		{"alt+up", "\x1b\x1b[A"},
		{"kitty key", "\x1b[97;5u"},
		{"other OSC reply", "\x1b]52;c;aGk=\x07"},
		{"OSC 1 is not a colour", "\x1b]1;x\x07"},
		{"other DCS", "\x1bP1$r0m\x1b\\"},
		{"ESC inside OSC body", "\x1b]10;rgb\x1bx"},
		{"text", "héllo, 世界"},
	}
	for _, k := range keys {
		t.Run(k.name, func(t *testing.T) {
			for i := range len(k.seq) + 1 {
				out, held := feedAll(k.seq[:i], k.seq[i:])
				if out != k.seq || held {
					t.Fatalf("split %d: out %q held %v, want %q", i, out, held, k.seq)
				}
			}
		})
	}
}

func TestFilterMixedAndHeld(t *testing.T) {
	cases := []struct {
		name  string
		reads []string
		out   string
		held  bool
	}{
		{"keys around replies",
			[]string{"x\x1b[1;1Ry\x1b[Az\x1b]11;rgb:0/0/0\x07!"}, "xy\x1b[Az!", false},
		{"reply right after a bare ESC", []string{"\x1b", "\x1b[5;5R"}, "\x1b", false},
		{"two replies back to back", []string{"\x1b[?1;2c\x1b[3;4R"}, "", false},
		{"a trailing ESC is held", []string{"a\x1b"}, "a", true},
		{"a reply prefix is held", []string{"a\x1b[?6"}, "a", true},
		{"a CPR missing a column is no reply", []string{"\x1b[12;R"}, "\x1b[12;R", false},
		{"too long to be a reply", []string{"\x1b]10;" + strings.Repeat("f", maxReply) + "\x07"},
			"\x1b]10;" + strings.Repeat("f", maxReply) + "\x07", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, held := feedAll(tc.reads...)
			if out != tc.out || held != tc.held {
				t.Fatalf("out %q held %v, want %q held %v", out, held, tc.out, tc.held)
			}
		})
	}

	var f replyFilter
	out := f.filter(nil, []byte("a\x1b[1"))
	out = f.flush(out)
	if string(out) != "a\x1b[1" || f.holding() {
		t.Fatalf("flush: %q holding %v", out, f.holding())
	}
}
