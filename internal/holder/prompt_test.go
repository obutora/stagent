package holder

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/screen"
)

// testdata/claude_*, codex_*: real screens of Claude Code and Codex, the
// fixtures of the app's screen reader
// (test/services/terminal_chat/_fixtures): `.ansi` is what a terminal of
// the size in the name is fed, `.txt` the app's plain rendering (`|` +
// row, then the cursor line).

var fixtureSize = regexp.MustCompile(`_(\d+)x(\d+)$`)

// fixtureScreen is the holder's screen after the program drew fixture name.
func fixtureScreen(t *testing.T, name string) *screen.Screen {
	t.Helper()
	m := fixtureSize.FindStringSubmatch(name)
	cols, _ := strconv.Atoi(m[1])
	rows, _ := strconv.Atoi(m[2])
	scr := screen.New(cols, rows, nil)
	t.Cleanup(scr.Close)
	scr.Write(readFixture(t, name+".ansi"))
	return scr
}

// fixtureText is the app's rendering of fixture name.
func fixtureText(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(string(readFixture(t, name+".txt")), "\n") {
		if row, ok := strings.CutPrefix(line, "|"); ok {
			out = append(out, row)
		}
	}
	return out
}

func readFixture(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPermissionMenu(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude_permission_60x30", true},     // "Do you want to create a.txt?"
		{"claude_permission_45x30", true},     // the same, wrapped
		{"claude_permission_nav_60x30", true}, // cursor moved to "3. No"
		{"claude_plan_60x30", true},           // "Would you like to proceed?"
		{"claude_working_after_answer_60x30", false},
		{"claude_ask_60x30", false}, // AskUserQuestion: no "1. Yes"
		{"claude_draft3_60x30", false},
		{"claude_done_60x30", false},
		{"claude_idle_60x30", false},
		{"codex_approval_60x30", true},     // "Would you like to run the following command?"
		{"codex_approval_45x30", true},     // the same, wrapped
		{"codex_approval_nav_60x30", true}, // cursor moved to "2. Yes, and don't ask again"
		{"codex_hooks_review_60x30", false},
		{"codex_trust_60x30", false},
		{"codex_update_60x30", false},
		{"codex_working_draft3_60x30", false},
		{"codex_draft3_60x30", false},
		{"codex_done_60x30", false},
		{"codex_idle_60x30", false},
	} {
		if _, got := permissionMenu(fixtureScreen(t, tc.name).Lines()); got != tc.want {
			t.Errorf("%s on the holder's screen: %v, want %v", tc.name, got, tc.want)
		}
		if _, got := permissionMenu(fixtureText(t, tc.name)); got != tc.want {
			t.Errorf("%s as the app renders it: %v, want %v", tc.name, got, tc.want)
		}
	}

	for name, lines := range map[string][]string{
		"numbered list in the transcript": {
			"● Steps:",
			"  1. Yes, build it",
			"  2. Run the tests",
			"",
			"✻ Worked for 3s · done",
		},
		"numbered prompt the user sent": {
			"❯ 1. Yes do it",
			"  2. then test",
			"",
			"● OK",
		},
		"highlighted list without a key hint": {
			"Do you want to proceed?",
			"❯ 1. Yes",
			"  2. No",
			"",
			"some text",
			"more text",
			"and more",
			"and more",
			"Esc to cancel",
		},
		"numbered draft in the prompt box": {
			"────────────────────────────",
			"❯ 1. Yes",
			"  2. No",
			"────────────────────────────",
			"  ? for shortcuts",
		},
	} {
		if _, ok := permissionMenu(lines); ok {
			t.Errorf("%s: read as a permission menu", name)
		}
	}
}

// A chat message is refused on any menu, not only on approvals (#440):
// Codex's update notice and folder trust prompt take an Enter as their
// first option. Numbered drafts in the input box and numbered lists in the
// transcript are no menus.
func TestLiveMenu(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude_permission_60x30", true},
		{"claude_plan_60x30", true},
		{"claude_ask_60x30", true},
		{"claude_working_after_answer_60x30", false},
		{"claude_draft3_60x30", false},
		{"claude_done_60x30", false},
		{"claude_idle_60x30", false},
		{"codex_approval_60x30", true},
		{"codex_hooks_review_60x30", true}, // "1. Review hooks"
		{"codex_trust_60x30", true},        // "1. Trust and continue"
		{"codex_update_60x30", true},       // "1. Update now"
		{"codex_working_draft3_60x30", false},
		{"codex_draft3_60x30", false},
		{"codex_done_60x30", false},
		{"codex_idle_60x30", false},
	} {
		if _, got := liveMenu(fixtureScreen(t, tc.name).Lines()); got != tc.want {
			t.Errorf("%s on the holder's screen: %v, want %v", tc.name, got, tc.want)
		}
		if _, got := liveMenu(fixtureText(t, tc.name)); got != tc.want {
			t.Errorf("%s as the app renders it: %v, want %v", tc.name, got, tc.want)
		}
	}

	// The three-line drafts with each line numbered.
	numbered := strings.NewReplacer("first line", "1. first line", "second line", "2. second line", "third line", "3. third line")
	for _, name := range []string{"claude_draft3_60x30", "codex_draft3_60x30", "codex_working_draft3_60x30"} {
		if _, ok := liveMenu(editRows(fixtureText(t, name), numbered.Replace)); ok {
			t.Errorf("%s with a numbered draft: read as a menu", name)
		}
	}
	for name, lines := range map[string][]string{
		"numbered list in the transcript": {
			"● Steps:",
			"  1. Build it",
			"  2. Run the tests",
			"",
			"✻ Worked for 3s · done",
		},
		"numbered prompt the user sent to Codex": {
			"› 1. build it",
			"  2. then test",
			"",
			"• Working (0s • esc to interrupt)",
		},
		// Codex's status bullet blinks between • and ◦.
		"numbered prompt the user sent to Codex, status bullet blinked": {
			"› 1. build it",
			"  2. then test",
			"",
			"◦ Working (0s • esc to interrupt)",
		},
	} {
		if _, ok := liveMenu(lines); ok {
			t.Errorf("%s: read as a menu", name)
		}
	}
}

// textScreen draws rows (the app's rendering of a screen) on a cleared
// screen.
func textScreen(rows []string) string {
	var b strings.Builder
	b.WriteString("\x1b[2J")
	for i, row := range rows {
		b.WriteString("\x1b[" + strconv.Itoa(i+1) + ";1H" + row)
	}
	return b.String()
}

// editRows returns rows with f applied to each.
func editRows(rows []string, f func(string) string) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = f(row)
	}
	return out
}

// cursorOnNo is the claude_permission_60x30 dialog with the ❯ cursor moved
// from "1. Yes" to "3. No".
func cursorOnNo(t *testing.T) []string {
	t.Helper()
	return editRows(fixtureText(t, "claude_permission_60x30"), func(row string) string {
		if strings.Contains(row, "❯ 1. Yes") {
			return strings.Replace(row, "❯", " ", 1)
		}
		if rest, ok := strings.CutPrefix(row, "   3. No"); ok {
			return " ❯ 3. No" + rest
		}
		return row
	})
}

// otherFile is the claude_permission_60x30 dialog asking about another
// file, as the next of two parallel tool calls would.
func otherFile(t *testing.T) []string {
	t.Helper()
	return editRows(fixtureText(t, "claude_permission_60x30"), func(row string) string {
		return strings.ReplaceAll(row, "a.txt", "c.txt")
	})
}

func TestMenuSignature(t *testing.T) {
	sig := func(what string, lines []string) string {
		t.Helper()
		s, ok := permissionMenu(lines)
		if !ok {
			t.Fatalf("%s: no menu", what)
		}
		return s
	}
	a := sig("a.txt", fixtureScreen(t, "claude_permission_60x30").Lines())
	if !strings.Contains(a, "Doyouwanttocreatea.txt?") || !strings.Contains(a, "Createfile") || strings.Contains(a, "Write(") {
		t.Fatalf("signature %q: want the dialog from its top edge to the question", a)
	}
	for what, lines := range map[string][]string{
		"as the app renders it":    fixtureText(t, "claude_permission_60x30"),
		"wrapped at 45 columns":    fixtureScreen(t, "claude_permission_45x30").Lines(),
		"cursor moved to 3. No":    cursorOnNo(t),
		"tool line's ● blinks off": editRows(fixtureText(t, "claude_permission_60x30"), func(row string) string { return strings.Replace(row, "●", " ", 1) }),
	} {
		if got := sig(what, lines); got != a {
			t.Errorf("%s: signature %q, want %q", what, got, a)
		}
	}
	for what, lines := range map[string][]string{
		"another file":  otherFile(t),
		"b.txt":         fixtureScreen(t, "claude_permission_nav_60x30").Lines(),
		"plan approval": fixtureScreen(t, "claude_plan_60x30").Lines(),
	} {
		if got := sig(what, lines); got == a {
			t.Errorf("%s: same signature as a.txt's dialog %q", what, got)
		}
	}

	// Codex's dialog has no top rule: it starts under the transcript entry
	// above it (`• Running …`), which is not part of it.
	c := sig("codex", fixtureScreen(t, "codex_approval_60x30").Lines())
	if !strings.HasPrefix(c, "Wouldyouliketorunthefollowingcommand?") || !strings.HasSuffix(c, "$printfhi>a.txt") {
		t.Fatalf("codex signature %q: want the dialog from its title to the command", c)
	}
	for what, lines := range map[string][]string{
		"as the app renders it":              fixtureText(t, "codex_approval_60x30"),
		"cursor moved to 2.":                 fixtureScreen(t, "codex_approval_nav_60x30").Lines(),
		"the transcript entry scrolled away": fixtureText(t, "codex_approval_60x30")[12:],
	} {
		if got := sig(what, lines); got != c {
			t.Errorf("codex %s: signature %q, want %q", what, got, c)
		}
	}
	if got := sig("another command", editRows(fixtureText(t, "codex_approval_60x30"), func(row string) string {
		return strings.ReplaceAll(row, "printf hi", "printf ho")
	})); got == c {
		t.Errorf("another command: same signature as the first %q", got)
	}
}

func TestScreenLinesMatchTheAppRendering(t *testing.T) {
	for _, name := range []string{"claude_permission_60x30", "claude_plan_60x30", "codex_approval_60x30"} {
		got := fixtureScreen(t, name).Lines()
		want := fixtureText(t, name)
		for i := range want {
			if strings.TrimRight(want[i], " ") != got[i] {
				t.Errorf("%s row %d = %q, want %q", name, i, got[i], want[i])
			}
		}
	}
}

// watchScreen is a prompt watch over a 60x30 screen; reports collects
// what it reports.
func watchScreen(t *testing.T) (*screen.Screen, *promptWatch, chan int64) {
	t.Helper()
	scr := screen.New(60, 30, nil)
	t.Cleanup(scr.Close)
	reports := make(chan int64, 8)
	w := newPromptWatch(scr.Lines, func(gen int64) { reports <- gen })
	t.Cleanup(w.stop)
	return scr, w, reports
}

const answered = "\x1b[2J\x1b[H● Write(a.txt)\r\n  ⎿  Wrote 1 line to a.txt\r\n"

func draw(scr *screen.Screen, w *promptWatch, s string) {
	scr.Write([]byte(s))
	w.output()
}

func noReport(t *testing.T, reports chan int64, within time.Duration) {
	t.Helper()
	select {
	case gen := <-reports:
		t.Fatalf("unexpected prompt_gone %d", gen)
	case <-time.After(within):
	}
}

func TestPromptWatchReportsOnceAfterTheMenuLeft(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(3, true)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	noReport(t, reports, 3*promptEvalEvery)

	// Cleared and drawn again at once: no answer.
	draw(scr, w, "\x1b[2J\x1b[H")
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	noReport(t, reports, promptEvalEvery+2*promptGoneAfter)

	// The screen goes quiet without the menu.
	draw(scr, w, answered)
	select {
	case gen := <-reports:
		if gen != 3 {
			t.Fatalf("prompt_gone %d, want 3", gen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt_gone")
	}
	draw(scr, w, "more output\r\n")
	w.set(3, true) // re-sent by the daemon: the same watch
	noReport(t, reports, promptEvalEvery+2*promptGoneAfter)
}

func TestPromptWatchNeverSeenReportsNothing(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	for range 5 {
		draw(scr, w, answered)
		time.Sleep(promptEvalEvery / 2)
	}
	noReport(t, reports, promptEvalEvery+2*promptGoneAfter)
}

func wantReport(t *testing.T, reports chan int64, want int64) {
	t.Helper()
	select {
	case gen := <-reports:
		if gen != want {
			t.Fatalf("prompt_gone %d, want %d", gen, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no prompt_gone %d", want)
	}
}

// settle waits long enough for any report the screen calls for.
const settle = promptEvalEvery + 2*promptGoneAfter

// claude shows parallel tool calls' prompts one after the other: answering
// the first switches the screen straight to the next menu, and the next
// prompt's watch starts a moment later. The first watch is reported, the
// second one when its own menu goes.
func TestPromptWatchNextMenuReplacesTheFirst(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	noReport(t, reports, 2*promptEvalEvery)

	draw(scr, w, string(readFixture(t, "claude_permission_nav_60x30.ansi"))) // b.txt's prompt
	time.Sleep(300 * time.Millisecond)                                       // its hook comes later
	w.set(2, true)
	wantReport(t, reports, 1)
	noReport(t, reports, settle) // b.txt's prompt is up

	draw(scr, w, answered)
	wantReport(t, reports, 2)
	noReport(t, reports, settle)
}

// The next prompt's watch may also start while the first menu is still up:
// it does not take that menu, and the first watch is reported once the
// screen switched to the next one.
func TestPromptWatchNextHookBeforeTheSwitch(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	time.Sleep(2 * promptEvalEvery)
	w.set(2, true)
	noReport(t, reports, settle)

	draw(scr, w, textScreen(otherFile(t)))
	wantReport(t, reports, 1)
	noReport(t, reports, settle)

	draw(scr, w, answered)
	wantReport(t, reports, 2)
	noReport(t, reports, settle)
}

// Moving the cursor through the options is no answer.
func TestPromptWatchCursorMovesWithinTheMenu(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	time.Sleep(2 * promptEvalEvery)
	draw(scr, w, textScreen(cursorOnNo(t)))
	noReport(t, reports, settle)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	noReport(t, reports, settle)

	draw(scr, w, answered)
	wantReport(t, reports, 1)
}

// A watch that saw no menu when the next one starts is left to the next
// one (the daemon closes its approval with the next report).
func TestPromptWatchUnseenWatchFoldsIntoTheNext(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	draw(scr, w, answered)
	time.Sleep(2 * promptEvalEvery)
	w.set(2, true)
	draw(scr, w, string(readFixture(t, "claude_permission_60x30.ansi")))
	time.Sleep(2 * promptEvalEvery)
	draw(scr, w, answered)
	wantReport(t, reports, 2)
	noReport(t, reports, settle)
}

// A resize forgets that the menu was up (it may not fit at the new size)
// until the redrawn menu is seen again, and a watch turned off reports
// nothing.
func TestPromptWatchResizeAndOff(t *testing.T) {
	scr, w, reports := watchScreen(t)
	menu := string(readFixture(t, "claude_permission_60x30.ansi"))

	w.set(1, true)
	draw(scr, w, menu)
	time.Sleep(2 * promptEvalEvery)
	w.resized()
	draw(scr, w, answered)
	noReport(t, reports, settle)

	draw(scr, w, menu)
	time.Sleep(2 * promptEvalEvery)
	w.set(1, false)
	draw(scr, w, answered)
	noReport(t, reports, settle)
}

// Codex blinks its terminal title while its approval is up, and an Esc
// leaves the menu for the canceled request and an empty prompt (#320).
func TestPromptWatchCodexMenuCanceled(t *testing.T) {
	scr, w, reports := watchScreen(t)
	w.set(1, true)
	draw(scr, w, string(readFixture(t, "codex_approval_60x30.ansi")))
	for _, title := range []string{"[ . ]", "[ ! ]", "[ . ]"} {
		time.Sleep(promptEvalEvery)
		draw(scr, w, "\x1b]0;"+title+" Action Required | Create a.txt | w\x07")
	}
	noReport(t, reports, settle)

	draw(scr, w, textScreen([]string{
		"• I’ll create a.txt with exactly hi (no trailing newline).",
		"",
		"✗ You canceled the request to run printf hi > a.txt",
		"",
		"• Failed (exit 1) printf hi > a.txt",
		"  └ (no output)",
		"",
		"■ Conversation interrupted - use /feedback if something",
		"  went wrong",
		"",
		"",
		"› Ask Codex to do anything",
		"",
		"  ? for shortcuts",
	}))
	wantReport(t, reports, 1)
	noReport(t, reports, settle)
}
