package holder

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Prompt watch (holder.prompt_watch): while the daemon has a claude
// approval of the session pending, the holder looks for claude's
// permission menu on the screen and reports once the menu it saw is gone
// (holder.prompt_gone): the prompt was answered, wherever that happened.
// claude itself does not tell — its hook lives on until the approved tool
// finished.
//
// A menu is told apart from the next one by its signature (menuSignature):
// claude shows parallel tool calls' prompts one after the other, switching
// from one menu straight to the next, and the hook of the next prompt (a new
// watch) may come before or after that switch. A watch's menu is therefore
// gone once no menu of its signature is on the screen — none, or another
// one — and a watch whose menu is still up when the next watch starts keeps
// being watched until it goes. Each watch is reported at most once.

const (
	// promptEvalEvery bounds how often output makes the watch read the
	// screen; a check also follows the last output, so a screen that went
	// quiet is read too.
	promptEvalEvery = 250 * time.Millisecond
	// promptGoneAfter: the menu must stay off the screen this long. A
	// clear-and-redraw of the dialog is not an answer.
	promptGoneAfter = 400 * time.Millisecond
)

// promptTrack is one watch (gen) still to be reported.
type promptTrack struct {
	gen     int64
	adopted bool   // sig is the menu this watch saw
	sig     string // its menu's signature
	// seen: the menu of sig is known to be up (cleared by a resize until
	// the redrawn menu is seen again).
	seen      bool
	goneSince time.Time // no menu of sig on the screen since (zero: it is)
}

// promptWatch is the holder's side of the prompt watches. lines reads the
// screen; gone reports a watch whose menu left it (called with mu held,
// it must not block or call back).
type promptWatch struct {
	lines func() []string
	gone  func(gen int64)

	mu sync.Mutex
	// tracks are the watches not reported yet, oldest first; the last one
	// is the newest watch (gen). Only the newest adopts a menu: an older
	// one stays only while it has one.
	tracks   []*promptTrack
	gen      int64 // newest watch started (0: none)
	lastEval time.Time
	timer    *time.Timer
	due      time.Time // when timer fires (zero: not armed)
}

func newPromptWatch(lines func() []string, gone func(gen int64)) *promptWatch {
	return &promptWatch{lines: lines, gone: gone}
}

// set starts watch gen (on) or ends every watch (off). Starting a watch
// keeps an older one that saw a menu: it is reported when that menu goes
// (at once if it already went); an older one that saw none is dropped —
// the daemon closes its approval with the newer watch's report. The same
// gen again (the daemon re-sends it to a holder that re-registered) keeps
// everything as it is.
func (w *promptWatch) set(gen int64, on bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !on {
		if gen >= w.gen {
			w.stopLocked()
		}
		return
	}
	if gen <= w.gen {
		return
	}
	w.gen = gen
	kept := w.tracks[:0]
	for _, t := range w.tracks {
		if t.adopted {
			kept = append(kept, t)
		}
	}
	w.tracks = append(kept, &promptTrack{gen: gen})
	w.evalLocked(time.Now()) // the menu may be up already
}

// stop ends every watch (the daemon connection is gone: a watch belongs to
// the daemon that asked for it).
func (w *promptWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopLocked()
	w.gen = 0
}

func (w *promptWatch) stopLocked() {
	w.tracks = nil
	if w.timer != nil {
		w.timer.Stop()
	}
	w.due = time.Time{}
}

// output notes that the screen changed.
func (w *promptWatch) output() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tracks) == 0 {
		return
	}
	now := time.Now()
	if now.Sub(w.lastEval) >= promptEvalEvery {
		w.evalLocked(now)
		return
	}
	w.armLocked(w.lastEval.Add(promptEvalEvery))
}

// resized forgets that the menus were up: at the new size a menu may not
// fit, which must not read as an answer. A menu still up is seen again,
// by its signature, once the program redrew it.
func (w *promptWatch) resized() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, t := range w.tracks {
		t.seen, t.goneSince = false, time.Time{}
	}
}

func (w *promptWatch) tick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.due = time.Time{}
	if len(w.tracks) > 0 {
		w.evalLocked(time.Now())
	}
}

// armLocked makes sure the screen is read again by at.
func (w *promptWatch) armLocked(at time.Time) {
	if !w.due.IsZero() && !w.due.After(at) {
		return
	}
	w.due = at
	if w.timer == nil {
		w.timer = time.AfterFunc(time.Until(at), w.tick)
	} else {
		w.timer.Reset(time.Until(at))
	}
}

func (w *promptWatch) evalLocked(now time.Time) {
	w.lastEval = now
	sig, up := claudePermissionMenu(w.lines())
	kept := w.tracks[:0] // the tracks still to be reported, in order
	for _, t := range w.tracks {
		switch {
		case !t.adopted:
			// The newest watch takes the menu on the screen, unless an
			// older watch is waiting for that menu to go.
			if up && !awaited(kept, sig) {
				t.adopted, t.sig, t.seen = true, sig, true
			}
		case up && sig == t.sig:
			t.seen, t.goneSince = true, time.Time{}
		case t.seen:
			if t.goneSince.IsZero() {
				t.goneSince = now
			}
			if at := t.goneSince.Add(promptGoneAfter); now.Before(at) {
				w.armLocked(at)
			} else {
				w.gone(t.gen)
				continue
			}
		}
		kept = append(kept, t)
	}
	clear(w.tracks[len(kept):])
	w.tracks = kept
}

// awaited reports whether one of tracks waits for the menu of signature
// sig to go.
func awaited(tracks []*promptTrack, sig string) bool {
	for _, t := range tracks {
		if t.adopted && t.sig == sig {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Menu recognition, after the app's screen reader
// (lib/services/terminal_chat/agent_activity.dart): numbered options with
// one under claude's ❯ cursor and a key hint below them. Of those menus,
// claude's PermissionRequest dialogs are the ones whose first option is
// "1. Yes…": a permission prompt ("Do you want to …?" → "1. Yes") and
// plan approval ("Would you like to proceed?" → "1. Yes, and …").

// promptOption: `❯ 1. Yes` / `  2. No`.
var promptOption = regexp.MustCompile(`^(\s*)(?:(❯)\s*)?(\d{1,2})\.\s+(\S.*)$`)

// promptHint: key hints every captured menu shows under its options (`Esc
// to cancel · Tab to amend`, `ctrl+g to edit in Vim` under plan approval).
// A numbered list in the transcript has none.
var promptHint = regexp.MustCompile(`(?i)\besc\b|\benter\b|↑/↓|ctrl\+g`)

// promptTranscriptMark: transcript entries (`● `, `⎿`, `✻ …`) and prompts
// the user sent (`❯ `); a list above one is part of the conversation.
var promptTranscriptMark = regexp.MustCompile(`^(?:[●•⏺⎿✻✢✳✶✽✔✗■]|[›❯]\s)`)

// promptRuleChars make up horizontal rules and box edges.
const promptRuleChars = "─━═╌╍┄┅┈┉├┤┌┐└┘╭╮╰╯┬┴┼┝┥┠┨╞╡"

// promptEdgeChars make up the solid rule along the top of claude's dialogs.
const promptEdgeChars = "─━═▔"

type promptItem struct {
	indent      int
	highlighted bool
	number      int
	label       string
	start, end  int // screen lines of the option (end: its last wrapped line)
}

// claudePermissionMenu reports whether lines (a screen, top to bottom) show
// claude's permission or plan approval menu, and its signature.
func claudePermissionMenu(lines []string) (sig string, ok bool) {
	var groups [][]promptItem
	cur := -1 // index in groups of the menu being read
	// Blank lines and rules since the last line of the current menu; one is
	// allowed between options.
	gap := 0
	for i, text := range lines {
		if it, ok := parsePromptItem(text, i); ok {
			if cur >= 0 && gap <= 1 && it.number == groups[cur][len(groups[cur])-1].number+1 {
				groups[cur] = append(groups[cur], it)
			} else {
				groups = append(groups, []promptItem{it})
				cur = len(groups) - 1
			}
			gap = 0
			continue
		}
		if cur < 0 {
			continue
		}
		t := strings.TrimSpace(text)
		if t == "" || isPromptRule(t) {
			if gap++; gap > 1 {
				cur = -1
			}
			continue
		}
		last := &groups[cur][len(groups[cur])-1]
		if gap == 0 && indentOf(text) > last.indent {
			last.end = i // a wrapped label or a description under it
			continue
		}
		cur = -1
	}

	// The live menu is the lowest one; lists higher up are transcript.
	for g := len(groups) - 1; g >= 0; g-- {
		group := groups[g]
		if len(group) < 2 {
			continue
		}
		highlighted := 0
		for _, it := range group {
			if it.highlighted {
				highlighted++
			}
		}
		if highlighted != 1 {
			continue
		}
		// claude's prompt sits between two rules; a numbered draft typed
		// there is not a menu.
		if first := group[0].start; first > 0 && isPromptRule(strings.TrimSpace(lines[first-1])) {
			continue
		}
		if !promptHintFollows(lines, group[len(group)-1].end) {
			continue
		}
		if group[0].number != 1 || !strings.HasPrefix(group[0].label, "Yes") {
			return "", false
		}
		return menuSignature(lines, group[0].start), true
	}
	return "", false
}

// menuSignature identifies the dialog whose first option is on line first:
// the text above the options, from the dialog's top edge — the nearest rule
// above them that starts in the first column (claude draws its dialogs
// under a full-width `───` or, for plan approval, `▔▔▔`; rules inside the
// dialog are indented or dashed) — or from the top of the screen when that
// edge scrolled away. That is the dialog's title, the command or file it
// asks about with its preview, and the question: it differs between the
// prompts of two tool calls, and stays the same while the dialog is up,
// whatever happens below it (the ❯ cursor moving between the options, Tab
// to amend) or above it (the blinking ● of the tool line, the transcript).
// Blanks, rule characters and the ●/⏺ marks are left out, so the same
// dialog wrapped at another width has the same signature. Two prompts for
// the very same tool call text share a signature: the second is not told
// apart from the first.
func menuSignature(lines []string, first int) string {
	top := 0
	for j := first - 1; j >= 0; j-- {
		if isDialogEdge(lines[j]) {
			top = j + 1
			break
		}
	}
	var b strings.Builder
	for _, line := range lines[top:first] {
		for _, r := range line {
			if !unicode.IsSpace(r) && r != '●' && r != '⏺' &&
				!strings.ContainsRune(promptRuleChars, r) && !strings.ContainsRune(promptEdgeChars, r) {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// isDialogEdge reports whether line is a solid rule starting in the first
// column.
func isDialogEdge(line string) bool {
	n := 0
	for _, r := range line {
		if !strings.ContainsRune(promptEdgeChars, r) {
			return false
		}
		n++
	}
	return n >= 3
}

func parsePromptItem(text string, line int) (promptItem, bool) {
	m := promptOption.FindStringSubmatch(text)
	if m == nil {
		return promptItem{}, false
	}
	n, _ := strconv.Atoi(m[3])
	return promptItem{
		indent: len(m[1]), highlighted: m[2] != "", number: n, label: m[4],
		start: line, end: line,
	}, true
}

// promptHintFollows reports a key hint within the few lines under the
// options, or nothing but blanks below them. A transcript entry below means
// the list is part of the conversation, e.g. a numbered prompt the user
// sent.
func promptHintFollows(lines []string, end int) bool {
	seen := 0
	for j := end + 1; j < len(lines) && seen < 4; j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" || isPromptRule(t) {
			continue
		}
		if promptTranscriptMark.MatchString(t) {
			return false
		}
		if promptHint.MatchString(t) {
			return true
		}
		seen++
	}
	return seen == 0
}

// isPromptRule reports whether trimmed is a horizontal rule or a box edge:
// mostly rule characters.
func isPromptRule(trimmed string) bool {
	all := utf8.RuneCountInString(trimmed)
	if all < 3 {
		return false
	}
	box := 0
	for _, r := range trimmed {
		if strings.ContainsRune(promptRuleChars, r) {
			box++
		}
	}
	return box*2 >= all
}

func indentOf(text string) int {
	return len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace))
}
