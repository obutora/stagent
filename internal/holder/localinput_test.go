package holder

import "testing"

func TestIsTyping(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"letters", "hi", true},
		{"enter", "\r", true},
		{"ctrl-c", "\x03", true},
		{"esc", "\x1b", true},
		{"alt-x", "\x1bx", true},
		{"arrow", "\x1b[A", true},
		{"ss3 arrow", "\x1bOA", true},
		{"delete", "\x1b[3~", true},
		{"modified arrow", "\x1b[1;5C", true},
		{"kitty key", "\x1b[97;5u", true},
		{"bracketed paste", "\x1b[200~text\x1b[201~", true},
		{"incomplete csi", "\x1b[", true},
		{"unterminated osc (alt-])", "\x1b]", true},
		{"unterminated dcs (alt-P)", "\x1bP", true},

		{"cpr", "\x1b[12;40R", false},
		{"decxcpr", "\x1b[?12;40;1R", false},
		{"primary da", "\x1b[?62;22c", false},
		{"secondary da", "\x1b[>1;10;0c", false},
		{"decrpm", "\x1b[?2004;1$y", false},
		{"ansi mode report", "\x1b[4;2$y", false},
		{"dsr", "\x1b[0n", false},
		{"color scheme report", "\x1b[?997;1n", false},
		{"window size report", "\x1b[8;24;80t", false},
		{"kitty flags report", "\x1b[?1u", false},
		{"focus in", "\x1b[I", false},
		{"focus out", "\x1b[O", false},
		{"sgr mouse press", "\x1b[<0;10;5M", false},
		{"sgr mouse release", "\x1b[<0;10;5m", false},
		{"x10 mouse", "\x1b[M #!", false},
		{"urxvt mouse", "\x1b[32;10;5M", false},
		{"osc 11 reply bel", "\x1b]11;rgb:0000/0000/0000\x07", false},
		{"osc 10 reply st", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\", false},
		{"osc 4 reply", "\x1b]4;1;rgb:cdcd/0000/0000\x07", false},
		{"xtversion", "\x1bP>|xterm(390)\x1b\\", false},
		{"decrqss reply", "\x1bP1$r0m\x1b\\", false},
		{"kitty graphics reply", "\x1b_Gi=1;OK\x1b\\", false},
		{"several replies", "\x1b[?62c\x1b[5;1R\x1b]11;rgb:0/0/0\x07\x1b[I", false},
		{"empty", "", false},

		{"reply then key", "\x1b[5;1Ra", true},
		{"key then reply", "a\x1b[I", true},
		{"focus then arrow", "\x1b[I\x1b[B", true},
		{"x10 mouse then key", "\x1b[M #!q", true},
	} {
		if got := isTyping([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: isTyping(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
