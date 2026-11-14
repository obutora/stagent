package holder

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/screen"
	"github.com/obutora/stagent/internal/wire"
)

// A question's UI is on the screen while the user moves through it:
// claude's numbered options with the cursor on any of them, its tabs and
// review page; omp's Ask box at the bottom of the screen, and the note or
// custom answer box one of its options opens; Codex's request_user_input
// (`Question 1/2`), its notes field open too. A question answered or left
// is not.
func TestQuestionShown(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"claude_ask_60x30", true},
		{"claude_ask_type_60x30", true},     // cursor on "4. Type something."
		{"claude_multi_60x30", true},        // a multi-select's checkboxes
		{"claude_multi_review_60x30", true}, // the Submit tab: "1. Submit answers"
		{"claude_working_after_answer_60x30", false},
		{"claude_idle_60x30", false},
		{"claude_done_60x30", false},
		// Claude Code 2.1.325's AskUserQuestion with previews (real
		// screens, copied from the app's fixtures): the preview box beside
		// the options, the cursor moved, a digit, the notes field opened
		// with `n` and a note typed into it — at 45 columns it wraps to
		// four lines under the box, above `Chat about this` and the hint.
		{"claude_preview_60x30", true},
		{"claude_preview_45x30", true},
		{"claude_preview_nav_60x30", true},
		{"claude_preview_digit_45x30", true},
		{"claude_preview_notes_60x30", true},
		{"claude_preview_notes_typed_60x30", true},
		{"claude_preview_notes_typed_45x30", true},
		{"omp_ask_60x30", true},
		{"omp_ask_nav_60x30", true},
		{"omp_ask_scrolled_45x30", true}, // long options scroll inside the box
		{"omp_multi_60x30", true},
		{"omp_note_open_60x30", true}, // the note box of an option
		{"omp_answered_60x30", false}, // the transcript's `╭─── ? Ask` block stays
		{"omp_working_60x30", false},
		{"omp_idle_60x30", false},
		// Codex 0.160.1's request_user_input in Plan mode
		// (testdata/codex_question*: real screens, tmux `capture-pane -e
		// -p` rows joined with CRLF and `capture-pane -p`): the first of
		// two questions, its notes field open with Tab (`› Add notes`), a
		// note typed (at 45 columns the hint wraps), the second question,
		// the answer in the transcript while Codex goes on, and Esc (the
		// turn interrupted).
		{"codex_question_60x30", true},
		{"codex_question_notes_60x30", true},
		{"codex_question_note_typed_45x30", true},
		{"codex_question2_60x30", true},
		{"codex_question_answered_60x30", false},
		{"codex_question_declined_60x30", false},
		{"codex_idle_60x30", false},
		{"codex_approval_60x30", false},
	} {
		harnessID, _, _ := strings.Cut(tc.name, "_")
		if got := questionShown(fixtureScreen(t, tc.name).Lines(), harnessID); got != tc.want {
			t.Errorf("%s on the holder's screen: %v, want %v", tc.name, got, tc.want)
		}
		if got := questionShown(fixtureText(t, tc.name), harnessID); got != tc.want {
			t.Errorf("%s as the app renders it: %v, want %v", tc.name, got, tc.want)
		}
	}
	// omp 18.12.6 after Enter on "Other (type your own)": its custom answer
	// box (testdata/omp_custom_answer_100x40.txt, `tmux capture-pane -p`
	// of a real omp running under stagent, rows prefixed with `|`).
	if !questionShown(fixtureText(t, "omp_custom_answer_100x40"), "omp") {
		t.Error("omp's custom answer box: not read as its question")
	}
}

// questionScreen is a question watch over a 60x30 screen; reports collects
// what it reports.
func questionScreen(t *testing.T) (*screen.Screen, *promptWatch, chan int64) {
	t.Helper()
	scr := screen.New(60, 30, nil)
	t.Cleanup(scr.Close)
	reports := make(chan int64, 8)
	w := newPromptWatch(scr.Lines, questionMenu, func(gen int64) { reports <- gen })
	t.Cleanup(w.stop)
	return scr, w, reports
}

// redraw shows fixture name alone on scr.
func redraw(t *testing.T, scr *screen.Screen, w *promptWatch, name string) {
	t.Helper()
	draw(scr, w, "\x1b[2J\x1b[H"+string(readFixture(t, name+".ansi")))
}

// Moving through a question — the highlight, a multi-select's checks,
// claude's tabs, omp's note box, Codex's notes field and next question —
// keeps its watch; the watch is reported once the UI left the screen
// (answered, or cancelled with Esc).
func TestQuestionWatchReportsWhenTheQuestionLeft(t *testing.T) {
	for _, c := range []struct {
		harness string
		steps   []string // the question's screens, then one without it
	}{
		{"claude", []string{"claude_ask_60x30", "claude_ask_type_60x30", "claude_multi_60x30", "claude_multi_review_60x30", "claude_working_after_answer_60x30"}},
		{"omp", []string{"omp_ask_60x30", "omp_ask_nav_60x30", "omp_note_open_60x30", "omp_multi_60x30", "omp_answered_60x30"}},
		{"codex", []string{"codex_question_60x30", "codex_question_notes_60x30", "codex_question2_60x30", "codex_question_declined_60x30"}},
	} {
		t.Run(c.harness, func(t *testing.T) {
			scr, w, reports := questionScreen(t)
			w.set(4, true, c.harness)
			last := len(c.steps) - 1
			for _, name := range c.steps[:last] {
				redraw(t, scr, w, name)
				noReport(t, reports, settle)
			}
			redraw(t, scr, w, c.steps[last])
			wantReport(t, reports, 4)
		})
	}
}

// While the daemon asks (holder.question_watch), the holder reports once
// that omp's question it saw left the screen (cancelled with Esc: no hook
// tells), as holder.question_gone — never as a prompt_gone.
func TestQuestionWatchReportsQuestionGone(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	fixture, err := filepath.Abs("testdata/omp_ask_60x30.ansi")
	if err != nil {
		t.Fatal(err)
	}
	id, done := startDetached(t, l, 60, 30, "sh", "-c",
		`cat "$0"; read a; printf '\033[2J\033[H Question dismissed\r\n'; read b`, fixture)
	from := 0
	d.waitFor(t, &from, "holder.register", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	d.notifyHolders(t, wire.MethodHolderQuestionWatch, wire.HolderPromptWatchParams{ID: id, Gen: 7, On: true, Harness: wire.HarnessOmp})

	time.Sleep(4 * promptEvalEvery) // the question is up
	c := dialHolder(t, l, id)
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil) // Esc
	m := d.waitFor(t, &from, "holder.question_gone", func(m *wire.Msg) bool {
		return m.Method == wire.MethodHolderQuestionGone || m.Method == wire.MethodHolderPromptGone
	})
	var p wire.HolderPromptGoneParams
	json.Unmarshal(m.Params, &p)
	if m.Method != wire.MethodHolderQuestionGone || p != (wire.HolderPromptGoneParams{ID: id, Gen: 7}) {
		t.Fatalf("%s %+v, want holder.question_gone of watch 7", m.Method, p)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}
