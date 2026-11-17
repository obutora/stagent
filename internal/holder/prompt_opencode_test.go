package holder

import (
	"strings"
	"testing"
)

// testdata/opencode_*: real screens of OpenCode 2.3.0 (tmux, 60x30 and
// 45x30, --standalone, driven by a local fake model): its permission menu
// for a shell command and a file edit, the highlight on each of its three
// options (_nav1_: Always allow, which changes the text above; _nav2_:
// Reject), full screen (ctrl+f), at 45 columns; its question (_nav_ with
// the highlight moved, _typed_ with the free-text answer open); its input
// box idle and with drafts, working, after a turn, after a rejection, and
// resumed.
var openCodeApprovals = []string{
	"opencode_approval_shell_60x30",
	"opencode_approval_shell_45x30",
	"opencode_approval_shell_nav1_60x30",
	"opencode_approval_shell_nav1_45x30",
	"opencode_approval_shell_nav2_60x30",
	"opencode_approval_edit_60x30",
	"opencode_approval_edit_always_60x30",
	"opencode_approval_fullscreen_45x30",
}

var openCodeQuestions = []string{"opencode_ask_60x30", "opencode_ask_nav_60x30", "opencode_ask_typed_60x30"}

var openCodeOthers = []string{
	"opencode_ask_answered_60x30",
	"opencode_idle_60x30", "opencode_idle_45x30",
	"opencode_draft1_60x30", "opencode_draft1_45x30", "opencode_draft3_60x30", "opencode_draft3_45x30",
	"opencode_working_60x30", "opencode_done_60x30", "opencode_rejected_60x30", "opencode_resumed_60x30",
}

// OpenCode's permission menu is one row of options without numbers or a
// cursor (`Allow once   Always allow   Reject`, the highlight only a
// colour) above its key hint (`⇆ select  enter confirm`, cut off at 45
// columns): read as a permission menu of three options on the holder's
// screen and as the app renders it, for a watch naming OpenCode and for one
// naming no harness. Its question is a menu, no approval; its other
// screens are neither.
func TestOpenCodePermissionMenu(t *testing.T) {
	all := append(append(append([]string{}, openCodeApprovals...), openCodeQuestions...), openCodeOthers...)
	for _, name := range all {
		want := strings.HasPrefix(name, "opencode_approval_")
		for _, h := range []string{"opencode", ""} {
			if _, got := permissionMenu(fixtureScreen(t, name).Lines(), h); got != want {
				t.Errorf("%s on the holder's screen, harness %q: %v, want %v", name, h, got, want)
			}
			if _, got := permissionMenu(fixtureText(t, name), h); got != want {
				t.Errorf("%s as the app renders it, harness %q: %v, want %v", name, h, got, want)
			}
		}
	}
	for _, c := range [][2]string{
		{"opencode_approval_shell_60x30", "claude"},
		{"opencode_approval_shell_60x30", "codex"},
		{"claude_permission_60x30", "opencode"},
		{"qwen_approval_shell_60x30", "opencode"},
	} {
		if _, ok := permissionMenu(fixtureText(t, c[0]), c[1]); ok {
			t.Errorf("%s read as %s's menu", c[0], c[1])
		}
	}
	// The same row in the transcript, with no key hint below, is no menu.
	transcript := []string{
		"  ┃  The buttons read:",
		"  ┃   Allow once   Always allow   Reject",
		"",
		"     Build · Mock Model · 930ms",
	}
	if _, ok := permissionMenu(transcript, "opencode"); ok {
		t.Error("a row of the transcript read as a menu")
	}
}

// A chat message is refused while any of OpenCode's menus is up, its
// permission menu (every option read) and its question (every option,
// whichever is highlighted, the free-text answer open or not).
func TestOpenCodeLiveMenu(t *testing.T) {
	for _, name := range append(append([]string{}, openCodeApprovals...), openCodeQuestions...) {
		want := 3
		if strings.Contains(name, "_ask") {
			want = 4
		}
		for what, lines := range map[string][]string{"holder's screen": fixtureScreen(t, name).Lines(), "app's rendering": fixtureText(t, name)} {
			opts, ok := liveMenu(lines)
			if !ok || len(opts) != want {
				t.Errorf("%s on the %s: %v, %d options, want %d", name, what, ok, len(opts), want)
			}
		}
	}
	if opts, _ := liveMenu(fixtureText(t, "opencode_approval_shell_60x30")); len(opts) == 3 &&
		(opts[0].label != "Allow once" || opts[1].label != "Always allow" || opts[2].label != "Reject") {
		t.Errorf("labels %+v", opts)
	}
	for _, name := range openCodeOthers {
		if _, ok := liveMenu(fixtureText(t, name)); ok {
			t.Errorf("%s read as a menu", name)
		}
	}
}

// The question watch sees OpenCode's question while it is up, the cursor
// anywhere or its free-text answer open, and not once it was answered.
func TestOpenCodeQuestionShown(t *testing.T) {
	for _, name := range append(append([]string{}, openCodeQuestions...), "opencode_ask_answered_60x30", "opencode_approval_shell_60x30") {
		want := strings.Contains(name, "_ask_") && !strings.Contains(name, "answered")
		if got := questionShown(fixtureScreen(t, name).Lines(), "opencode"); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}
