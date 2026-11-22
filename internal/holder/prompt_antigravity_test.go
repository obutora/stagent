package holder

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// testdata/antigravity_*: real screens of Antigravity CLI 1.5.14 (tmux,
// 60x30 and 45x30; the account scrubbed): its permission menus for a shell
// command (4 options, two of them wrapped to the first column) and for a
// file written outside the workspace (3), right after a command was
// allowed, its ask_question menu, its folder trust prompt, its input box
// idle, with a draft, working (generating, running a command, thinking),
// after a turn, after a menu was dismissed (Interrupted) and turned down,
// and just resumed.
var antigravityApprovals = []string{
	"antigravity_approval_shell_60x30",
	"antigravity_approval_shell_45x30",
	"antigravity_approval_write_60x30",
	"antigravity_approval_write_45x30",
}

var antigravityOthers = []string{
	"antigravity_ask_60x30", "antigravity_ask_45x30",
	"antigravity_trust_60x30", "antigravity_trust_45x30",
	"antigravity_approval_answered_60x30",
	"antigravity_idle_60x30", "antigravity_draft1_60x30", "antigravity_draft1_45x30",
	"antigravity_working_60x30", "antigravity_working_45x30",
	"antigravity_working_command_60x30", "antigravity_working_thinking_60x30",
	"antigravity_done_60x30", "antigravity_done_45x30",
	"antigravity_interrupted_60x30", "antigravity_declined_60x30", "antigravity_resumed_45x30",
}

// Antigravity CLI's permission menus are numbered options under its
// cursor > ("1. Yes, …" first) with a key hint below them (`↑/↓ Navigate ·
// …`): they are told on the holder's screen and as the app renders it, for
// a watch naming Antigravity CLI and for one naming no harness. Its
// question, its trust prompt and the screens of its input box and turns
// are no approval.
func TestAntigravityPermissionMenu(t *testing.T) {
	for _, name := range append(append([]string{}, antigravityApprovals...), antigravityOthers...) {
		want := slices.Contains(antigravityApprovals, name)
		for _, h := range []string{"antigravity", ""} {
			if _, got := permissionMenu(fixtureScreen(t, name).Lines(), h); got != want {
				t.Errorf("%s on the holder's screen, harness %q: %v, want %v", name, h, got, want)
			}
			if _, got := permissionMenu(fixtureText(t, name), h); got != want {
				t.Errorf("%s as the app renders it, harness %q: %v, want %v", name, h, got, want)
			}
		}
	}
	// A watch for another harness's menu does not take Antigravity CLI's,
	// nor one for Antigravity CLI's theirs.
	for _, c := range [][2]string{
		{"antigravity_approval_shell_60x30", "claude"},
		{"antigravity_approval_shell_60x30", "codex"},
		{"antigravity_approval_shell_60x30", "qwen"},
		{"claude_permission_60x30", "antigravity"},
		{"codex_approval_60x30", "antigravity"},
		{"qwen_approval_shell_60x30", "antigravity"},
	} {
		if _, ok := permissionMenu(fixtureText(t, c[0]), c[1]); ok {
			t.Errorf("%s read as %s's menu", c[0], c[1])
		}
	}
	// A numbered list in its prompt box (between two rules) or in a reply
	// is no menu.
	for what, lines := range map[string][]string{
		"a draft": {
			"────────────────────────────────────────",
			"> 1. Yes, do it",
			"  2. No",
			"────────────────────────────────────────",
			"esc to cancel",
		},
		"a reply": {
			"  Steps:",
			"  1. Yes, build it",
			"  2. Run the tests",
			"",
			"? for shortcuts",
		},
	} {
		if _, ok := permissionMenu(lines, "antigravity"); ok {
			t.Errorf("%s read as a menu", what)
		}
	}
}

// Every option of its menus is read with its whole label: a long one
// wraps to the first column of the lines under it, which continue it, at
// either width.
func TestAntigravityMenuOptions(t *testing.T) {
	shell := []string{
		"Yes, run command",
		"Yes, and always allow in this conversation for commands that start with 'curl -sI https://example.com'",
		"Yes, and always allow for commands that start with 'curl -sI https://example.com' (Persist to settings.json)",
		"No, cancel",
	}
	write := []string{"Yes, allow creation", "Yes, and always allow non-workspace access", "No, deny creation"}
	ask := []string{"Red", "Blue", "Write-in..."}
	for name, want := range map[string][]string{
		"antigravity_approval_shell_60x30": shell,
		"antigravity_approval_shell_45x30": shell,
		"antigravity_approval_write_60x30": write,
		"antigravity_approval_write_45x30": write,
		"antigravity_ask_60x30":            ask,
		"antigravity_ask_45x30":            ask,
	} {
		for what, lines := range map[string][]string{"holder's screen": fixtureScreen(t, name).Lines(), "app's rendering": fixtureText(t, name)} {
			opts, ok := liveMenu(lines)
			var labels []string
			for i, o := range opts {
				labels = append(labels, o.label)
				if o.number != i+1 || (o.cursor == ">") != (i == 0) {
					t.Errorf("%s (%s): option %d is %d, cursor %q", name, what, i+1, o.number, o.cursor)
				}
			}
			if !ok || !slices.Equal(labels, want) {
				t.Errorf("%s (%s): options %q, want %q", name, what, labels, want)
			}
		}
	}
	// The wrapped rows are the options' own lines.
	opts, _ := liveMenu(fixtureText(t, "antigravity_approval_shell_60x30"))
	if len(opts) == 4 && (opts[1].end != opts[1].start+1 || opts[2].end != opts[2].start+1 || opts[3].start != opts[2].end+1) {
		t.Errorf("option lines %+v", opts)
	}
}

// The signature of its menu is the dialog's text from the rule under its
// title down to the question: the same at either width, different for
// another call.
func TestAntigravityMenuSignature(t *testing.T) {
	sig := func(name string) string {
		t.Helper()
		s, ok := permissionMenu(fixtureText(t, name), "antigravity")
		if !ok || s == "" {
			t.Fatalf("%s: no signature", name)
		}
		return s
	}
	if a := sig("antigravity_approval_shell_60x30"); !strings.HasPrefix(a, "Requestingpermissionfor:") || !strings.HasSuffix(a, "Runthiscommand?") {
		t.Errorf("signature %q", a)
	}
	if a, b := sig("antigravity_approval_shell_60x30"), sig("antigravity_approval_shell_45x30"); a != b {
		t.Errorf("at 45 columns %q, at 60 %q", b, a)
	}
	if a, b := sig("antigravity_approval_shell_60x30"), sig("antigravity_approval_write_60x30"); a == b {
		t.Errorf("a command and a write share the signature %q", a)
	}
}

// Its folder trust prompt, drawn at the top of an otherwise empty screen
// before the conversation starts, has two options without numbers under
// its cursor: a menu (a chat message must not land on it), no approval.
func TestAntigravityTrustPrompt(t *testing.T) {
	for _, name := range []string{"antigravity_trust_60x30", "antigravity_trust_45x30"} {
		opts, ok := liveMenu(fixtureScreen(t, name).Lines())
		if !ok || len(opts) != 2 || opts[0].label != "Yes, I trust this folder" || opts[0].cursor != ">" ||
			opts[1].label != "No, exit" || opts[1].cursor != "" {
			t.Errorf("%s: %+v", name, opts)
		}
	}
}

// A chat message is refused while any of its menus is up — its permission
// menus, its question, its trust prompt — and only then.
func TestAntigravityLiveMenu(t *testing.T) {
	menus := append([]string{"antigravity_ask_60x30", "antigravity_ask_45x30", "antigravity_trust_60x30", "antigravity_trust_45x30"}, antigravityApprovals...)
	for _, name := range append(append([]string{}, antigravityApprovals...), antigravityOthers...) {
		want := slices.Contains(menus, name)
		if _, got := liveMenu(fixtureScreen(t, name).Lines()); got != want {
			t.Errorf("%s on the holder's screen: %v, want %v", name, got, want)
		}
		if _, got := liveMenu(fixtureText(t, name)); got != want {
			t.Errorf("%s as the app renders it: %v, want %v", name, got, want)
		}
	}
}

// The question watch sees its ask_question menu while it is up, and not
// once it was answered (the next screens of the turn).
func TestAntigravityQuestionShown(t *testing.T) {
	for name, want := range map[string]bool{
		"antigravity_ask_60x30":               true,
		"antigravity_ask_45x30":               true,
		"antigravity_approval_answered_60x30": false,
		"antigravity_working_60x30":           false,
		"antigravity_done_60x30":              false,
	} {
		if got := questionShown(fixtureScreen(t, name).Lines(), "antigravity"); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// A watch started for a permission check (shown) reports when
// Antigravity CLI's menu comes on the screen, then when it leaves (the
// allowed command runs); one whose check brought no menu reports nothing.
func TestPromptWatchAntigravityMenuShown(t *testing.T) {
	scr, w, gone := watchScreen(t)
	shown := make(chan int64, 4)
	w.shown = func(gen int64) { shown <- gen }
	w.startShown(1, "antigravity")
	draw(scr, w, string(readFixture(t, "antigravity_working_60x30.ansi")))
	select {
	case gen := <-shown:
		t.Fatalf("prompt_shown %d without a menu", gen)
	case <-time.After(settle):
	}
	w.startShown(2, "antigravity") // the next check
	draw(scr, w, "\x1b[2J\x1b[H"+string(readFixture(t, "antigravity_approval_shell_60x30.ansi")))
	select {
	case gen := <-shown:
		if gen != 2 {
			t.Fatalf("prompt_shown %d, want 2", gen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt_shown")
	}
	noReport(t, gone, settle)
	draw(scr, w, "\x1b[2J\x1b[H"+string(readFixture(t, "antigravity_approval_answered_60x30.ansi")))
	wantReport(t, gone, 2)
	noReport(t, gone, settle)
	if len(shown) != 0 {
		t.Errorf("prompt_shown again: %d", <-shown)
	}
}
