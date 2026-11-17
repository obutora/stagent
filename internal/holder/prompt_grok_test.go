package holder

import (
	"strings"
	"testing"
)

// testdata/grok_*: real screens of Grok Build 1.0.77 (tmux, 60x30 and
// 45x30, driven by a local fake model): its permission menu for a shell
// command (4 rows), a command on its dangerous list (rm: 5 rows, an "Always
// allow" row added) and a file edit; the highlight where Grok Build puts it
// after an earlier answer (_prevalways_: on "Always allow", _prevonce_,
// _prev2_ and _prev3_: on Yes again, _defreject_: on No by its setting) or
// the user moved it (_nav_); the menu with the focus parked in the
// scrollback after Esc (_parked_); its question card (the answer to type
// open, _input_, typed, _typed_, the focus parked, _parked_) and after an
// answer; its folder trust prompt; its input box idle, with drafts, set
// aside (Ctrl-S), working, after a turn and resumed.
var grokApprovals = []string{
	"grok_approval_shell_60x30",
	"grok_approval_shell_45x30",
	"grok_approval_shell_nav_60x30",
	"grok_approval_shell_prev2_60x30",
	"grok_approval_shell_prev3_60x30",
	"grok_approval_shell_defreject_60x30",
	"grok_approval_rm_60x30",
	"grok_approval_rm_prevalways_60x30",
	"grok_approval_rm_prevonce_60x30",
	"grok_approval_write_60x30",
	"grok_approval_write_45x30",
	"grok_approval_parked_60x30",
}

var grokQuestions = []string{"grok_ask_60x30", "grok_ask_input_60x30", "grok_ask_typed_60x30", "grok_ask_parked_60x30"}

var grokOthers = []string{
	"grok_ask_after2_60x30",
	"grok_idle_60x30", "grok_idle_45x30",
	"grok_draft1_60x30", "grok_draft1_45x30", "grok_draft3_60x30", "grok_draft3_45x30",
	"grok_stashed_60x30", "grok_working_60x30", "grok_done_60x30", "grok_idle_parked_60x30",
	"grok_done_draft_60x30", "grok_done_draft_45x30", "grok_resumed_60x30",
}

// Grok Build's permission menu is numbered rows right of a bar (`┃  2 (●)
// Yes, proceed`), the highlighted one marked ●, above its key hint (`1/4:
// select`, or `Tab/Space:permission` once the focus is parked): read as a
// permission menu on the holder's screen and as the app renders it, for a
// watch naming Grok Build and for one naming no harness. Its question card
// and its trust prompt are menus, no approval; its other screens are
// neither.
func TestGrokPermissionMenu(t *testing.T) {
	all := append(append(append(append([]string{}, grokApprovals...), grokQuestions...), "grok_trust_60x30"), grokOthers...)
	for _, name := range all {
		want := strings.HasPrefix(name, "grok_approval_")
		for _, h := range []string{"grok", ""} {
			if _, got := permissionMenu(fixtureScreen(t, name).Lines(), h); got != want {
				t.Errorf("%s on the holder's screen, harness %q: %v, want %v", name, h, got, want)
			}
			if _, got := permissionMenu(fixtureText(t, name), h); got != want {
				t.Errorf("%s as the app renders it, harness %q: %v, want %v", name, h, got, want)
			}
		}
	}
	for _, c := range [][2]string{
		{"grok_approval_shell_60x30", "claude"},
		{"grok_approval_shell_60x30", "opencode"},
		{"claude_permission_60x30", "grok"},
		{"qwen_approval_shell_60x30", "grok"},
		{"opencode_approval_shell_60x30", "grok"},
	} {
		if _, ok := permissionMenu(fixtureText(t, c[0]), c[1]); ok {
			t.Errorf("%s read as %s's menu", c[0], c[1])
		}
	}
	// The same rows in the transcript, with no key hint below, are no menu.
	transcript := []string{
		"  ┃",
		"  ┃  1 (○) Yes, and don't ask again",
		"  ┃  2 (●) Yes, proceed",
		"  ┃",
		"",
		"     ◆ Ran a command",
	}
	if _, ok := permissionMenu(transcript, "grok"); ok {
		t.Error("rows of the transcript read as a menu")
	}
}

// Each approval's rows are read with their numbers and labels, whichever is
// highlighted: the app picks the row whose label the user chose and types
// its number, which answers with that row wherever the highlight is.
func TestGrokMenuRows(t *testing.T) {
	for _, c := range []struct {
		name   string
		labels []string
		cursor int
	}{
		{"grok_approval_shell_60x30", []string{"Yes, and don't ask again for anything (always", "Yes, proceed", "No, reject (type to add feedback)", "Never allow: echo"}, 2},
		{"grok_approval_shell_nav_60x30", nil, 3},
		{"grok_approval_shell_defreject_60x30", nil, 3},
		{"grok_approval_rm_60x30", []string{"Yes, and don't ask again for anything (always", "Always allow: rm -f mock_touch.txt", "Yes, proceed", "No, reject (type to add feedback)", "Never allow: rm -f"}, 3},
		{"grok_approval_rm_prevalways_60x30", nil, 2},
		{"grok_approval_write_60x30", nil, 3},
		{"grok_approval_parked_60x30", nil, 2},
	} {
		for what, lines := range map[string][]string{"holder's screen": fixtureScreen(t, c.name).Lines(), "app's rendering": fixtureText(t, c.name)} {
			opts, ok := liveMenu(lines)
			if !ok {
				t.Errorf("%s on the %s: no menu", c.name, what)
				continue
			}
			cursor := 0
			for i, o := range opts {
				if o.number != i+1 {
					t.Errorf("%s on the %s: row %d numbered %d", c.name, what, i+1, o.number)
				}
				if o.cursor != "" {
					cursor = o.number
				}
			}
			if cursor != c.cursor {
				t.Errorf("%s on the %s: highlight on %d, want %d", c.name, what, cursor, c.cursor)
			}
			if c.labels == nil {
				continue
			}
			var labels []string
			for _, o := range opts {
				labels = append(labels, o.label)
			}
			if strings.Join(labels, "|") != strings.Join(c.labels, "|") {
				t.Errorf("%s on the %s: labels %q, want %q", c.name, what, labels, c.labels)
			}
		}
	}
}

// A chat message is refused while any of Grok Build's menus is up: its
// approvals, its question card (its three options and the one to type an
// answer in) and its folder trust prompt (Yes / No, no numbers).
func TestGrokLiveMenu(t *testing.T) {
	for _, c := range []struct {
		name string
		n    int
	}{
		{"grok_approval_shell_45x30", 4},
		{"grok_approval_rm_prevonce_60x30", 5},
		{"grok_approval_write_45x30", 4},
		{"grok_ask_60x30", 4},
		{"grok_ask_input_60x30", 4},
		{"grok_ask_typed_60x30", 4},
		{"grok_ask_parked_60x30", 4},
		{"grok_trust_60x30", 2},
	} {
		for what, lines := range map[string][]string{"holder's screen": fixtureScreen(t, c.name).Lines(), "app's rendering": fixtureText(t, c.name)} {
			if opts, ok := liveMenu(lines); !ok || len(opts) != c.n {
				t.Errorf("%s on the %s: %v, %d options, want %d", c.name, what, ok, len(opts), c.n)
			}
		}
	}
	if opts, _ := liveMenu(fixtureText(t, "grok_trust_60x30")); len(opts) == 2 && (opts[0].label != "Yes, proceed" || opts[1].label != "No, quit") {
		t.Errorf("trust prompt labels %+v", opts)
	}
	for _, name := range grokOthers {
		if _, ok := liveMenu(fixtureText(t, name)); ok {
			t.Errorf("%s read as a menu", name)
		}
	}
}

// The question watch sees Grok Build's question card while it is up (its
// typed answer open or the focus parked too), and not once it was
// answered, nor an approval.
func TestGrokQuestionShown(t *testing.T) {
	for _, name := range append([]string{"grok_ask_after2_60x30", "grok_approval_shell_60x30", "grok_approval_parked_60x30", "grok_idle_60x30"}, grokQuestions...) {
		want := strings.HasPrefix(name, "grok_ask_") && name != "grok_ask_after2_60x30"
		if got := questionShown(fixtureScreen(t, name).Lines(), "grok"); got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
}

// One approval keeps its signature while the highlight moves or the focus
// parks; another command's has another.
func TestGrokMenuSignature(t *testing.T) {
	sig := func(name string) string {
		s, ok := permissionMenu(fixtureText(t, name), "grok")
		if !ok {
			t.Fatalf("%s: no menu", name)
		}
		return s
	}
	shell := sig("grok_approval_shell_60x30")
	if shell == "" || sig("grok_approval_shell_nav_60x30") != shell {
		t.Errorf("signature changed with the highlight: %q, %q", shell, sig("grok_approval_shell_nav_60x30"))
	}
	if sig("grok_approval_write_60x30") == shell || sig("grok_approval_rm_60x30") == shell {
		t.Error("two commands share a signature")
	}
}
