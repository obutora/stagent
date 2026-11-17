package holder

import (
	"strings"
	"testing"
	"time"
)

// testdata/qwen_*: real screens of Qwen Code 0.27.4 (tmux, 60x30 and 45x30,
// driven by a local fake model): its permission menus for a shell command
// (4 rows), a file write (3 rows; 4 with an external editor set), in a
// folder it does not trust (2 rows), in Auto mode when its classifier could
// not decide (3 rows), in Japanese (ja) and at 45 columns (labels cut off
// with …); its ask_user_question menu, its input box idle, with drafts,
// working and after a turn, and its folder trust dialog.
var qwenApprovals = []string{
	"qwen_approval_shell_60x30",
	"qwen_approval_shell_45x30",
	"qwen_approval_shell_nav_60x30", // cursor moved to 2.
	"qwen_approval_shell_ja_60x30",
	"qwen_approval_shell_untrusted_60x30",
	"qwen_approval_shell_auto_60x30",
	"qwen_approval_write_60x30",
	"qwen_approval_write_45x30",
	"qwen_approval_write_ja_60x30",
	"qwen_approval_write_editor_60x30",
	"qwen_approval_write_untrusted_60x30",
}

var qwenOthers = []string{
	"qwen_ask_60x30", "qwen_ask_nav_60x30", "qwen_ask_answered_60x30",
	"qwen_idle_60x30", "qwen_idle_45x30", "qwen_idle_ja_60x30",
	"qwen_draft1_60x30", "qwen_draft3_60x30", "qwen_draft3_45x30",
	"qwen_working_60x30", "qwen_done_60x30", "qwen_resumed_60x30",
}

// Qwen Code's permission menu has no key hint under its options, only its
// spinner (`⠏ Waiting for user confirmation...`, translated), and its labels
// are translated: it is told by its shape — options under Codex's cursor
// ›, the last one marked with the key that turns it down (`(esc)`, `(Esc)`
// in Japanese) — on the holder's screen and as the app renders it, for a
// watch naming Qwen Code and for one naming no harness. Its question, the
// screens of its input box and its turns are no approval.
func TestQwenPermissionMenu(t *testing.T) {
	for _, name := range append(append([]string{}, qwenApprovals...), qwenOthers...) {
		want := strings.HasPrefix(name, "qwen_approval_")
		for _, h := range []string{"qwen", ""} {
			if _, got := permissionMenu(fixtureScreen(t, name).Lines(), h); got != want {
				t.Errorf("%s on the holder's screen, harness %q: %v, want %v", name, h, got, want)
			}
			if _, got := permissionMenu(fixtureText(t, name), h); got != want {
				t.Errorf("%s as the app renders it, harness %q: %v, want %v", name, h, got, want)
			}
		}
	}
	// A watch for Codex's menu (the same cursor) or claude's does not take
	// Qwen Code's, nor one for Qwen Code theirs.
	for _, c := range [][2]string{
		{"qwen_approval_shell_60x30", "codex"},
		{"qwen_approval_shell_60x30", "claude"},
		{"codex_approval_60x30", "qwen"},
		{"claude_permission_60x30", "qwen"},
	} {
		if _, ok := permissionMenu(fixtureText(t, c[0]), c[1]); ok {
			t.Errorf("%s read as %s's menu", c[0], c[1])
		}
	}
	// A numbered list in its transcript whose last item ends in
	// parentheses is no menu: no option is under the cursor.
	transcript := []string{
		"  ◆︎ Steps:",
		"    1. Yes, build it",
		"    2. Run the tests (later)",
		"",
		"  ⠧ Warp speed engaged...",
	}
	if _, ok := permissionMenu(transcript, "qwen"); ok {
		t.Error("a numbered list of the transcript read as a menu")
	}
}

// A chat message is refused while any of Qwen Code's menus is up: its
// permission menus and its question.
func TestQwenLiveMenu(t *testing.T) {
	for _, name := range append(append([]string{}, qwenApprovals...), "qwen_ask_60x30", "qwen_ask_nav_60x30") {
		if opts, ok := liveMenu(fixtureScreen(t, name).Lines()); !ok || len(opts) < 2 {
			t.Errorf("%s on the holder's screen: %v (%d options)", name, ok, len(opts))
		}
		if _, ok := liveMenu(fixtureText(t, name)); !ok {
			t.Errorf("%s as the app renders it: no menu", name)
		}
	}
	for _, name := range []string{"qwen_ask_answered_60x30", "qwen_idle_60x30", "qwen_draft3_60x30", "qwen_working_60x30", "qwen_done_60x30"} {
		if _, ok := liveMenu(fixtureText(t, name)); ok {
			t.Errorf("%s read as a menu", name)
		}
	}
	// Every row of each menu is read, wherever the cursor is.
	for name, want := range map[string]int{
		"qwen_approval_shell_60x30":           4,
		"qwen_approval_shell_nav_60x30":       4,
		"qwen_approval_write_60x30":           3,
		"qwen_approval_write_editor_60x30":    4,
		"qwen_approval_shell_untrusted_60x30": 2,
		"qwen_approval_shell_auto_60x30":      3,
		"qwen_ask_60x30":                      4,
	} {
		if opts, _ := liveMenu(fixtureText(t, name)); len(opts) != want {
			t.Errorf("%s: %d options, want %d", name, len(opts), want)
		}
	}
}

// The signature of Qwen Code's menu stays while the cursor moves through
// it and differs between the calls it asks about.
func TestQwenMenuSignature(t *testing.T) {
	sig := func(name string) string {
		t.Helper()
		s, ok := permissionMenu(fixtureText(t, name), "qwen")
		if !ok || s == "" {
			t.Fatalf("%s: no signature", name)
		}
		return s
	}
	if a, b := sig("qwen_approval_shell_60x30"), sig("qwen_approval_shell_nav_60x30"); a != b {
		t.Errorf("cursor moved: %q, was %q", b, a)
	}
	if a, b := sig("qwen_approval_shell_60x30"), sig("qwen_approval_write_60x30"); a == b {
		t.Errorf("a command and a write share the signature %q", a)
	}
}

// A watch for Qwen Code's approval reports once its menu left the screen:
// the cursor moving on the PC is no answer; the next screen (the allowed
// command ran, the next call is asked about) is.
func TestPromptWatchQwenMenuAnswered(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true, "qwen")
	draw(scr, w, string(readFixture(t, "qwen_approval_shell_60x30.ansi")))
	time.Sleep(2 * promptEvalEvery)
	draw(scr, w, "\x1b[2J\x1b[H"+string(readFixture(t, "qwen_approval_shell_nav_60x30.ansi"))) // ↓ on the PC
	noReport(t, reports, settle)
	draw(scr, w, "\x1b[2J\x1b[H"+string(readFixture(t, "qwen_approval_write_60x30.ansi")))
	wantReport(t, reports, 1)
	noReport(t, reports, settle)
}

// The question watch sees Qwen Code's ask_user_question menu while it is
// up, the cursor anywhere, and not once it was answered.
func TestQwenQuestionShown(t *testing.T) {
	for name, want := range map[string]bool{
		"qwen_ask_60x30":          true,
		"qwen_ask_nav_60x30":      true,
		"qwen_ask_answered_60x30": false,
	} {
		if got := questionShown(fixtureScreen(t, name).Lines(), "qwen"); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}
