package holder

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Prompt watch (holder.prompt_watch): while the daemon has a claude, Codex
// or Copilot CLI approval of the session pending, the holder looks for the
// program's permission menu on the screen and reports once the menu it saw
// is gone (holder.prompt_gone): the prompt was answered, wherever that
// happened. The programs themselves do not tell — claude's hook lives on
// until the approved tool finished, and Codex's and Copilot CLI's returned
// before their prompt showed.
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
	tracks []*promptTrack
	gen    int64 // newest watch started (0: none)
	// harness is the harness whose menu the newest watch looks for ("":
	// any; see permissionMenu).
	harness  string
	lastEval time.Time
	timer    *time.Timer
	due      time.Time // when timer fires (zero: not armed)
}

func newPromptWatch(lines func() []string, gone func(gen int64)) *promptWatch {
	return &promptWatch{lines: lines, gone: gone}
}

// set starts watch gen (on) for the menu of harnessID or ends every watch
// (off). Starting a watch keeps an older one that saw a menu: it is
// reported when that menu goes (at once if it already went); an older one
// that saw none is dropped — the daemon closes its approval with the newer
// watch's report. The same gen again (the daemon re-sends it to a holder
// that re-registered) keeps everything as it is.
func (w *promptWatch) set(gen int64, on bool, harnessID string) {
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
	w.gen, w.harness = gen, harnessID
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
	sig, up := permissionMenu(w.lines(), w.harness)
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

// menuShown reports whether a menu of a harness is on the screen, whatever
// it asks: a chat message must land on none of them. Codex's update notice
// (`1. Update now`) and folder trust prompt (`1. Trust and continue`) and
// claude's workspace trust prompt (`❯ No, exit`) at startup would take its
// Enter as their highlighted option just as an approval menu would; for
// claude's that is No, which ends claude.
func (h *Holder) menuShown() bool {
	_, ok := liveMenu(h.scr.Lines())
	return ok
}

// ---------------------------------------------------------------------------
// Menu recognition, after the app's screen reader
// (lib/services/terminal_chat/agent_activity.dart): numbered options with
// one under a harness's cursor (menuStyle.cursor: claude's and Copilot
// CLI's ❯, Codex's ›) and a key hint below them. Of those menus, the
// permission dialogs are the ones whose first option is "1. Yes…": claude's
// permission prompt ("Do you want to …?" → "1. Yes") and plan approval
// ("Would you like to proceed?" → "1. Yes, and …"), Codex's approval
// ("Would you like to run the following command?" → "1. Yes, proceed
// (y)"), Copilot CLI's ("Do you want to run this command?" → "1. Yes", in a
// box). A harness may have a menu without numbers too, recognized its own
// way (menuStyle.unnumbered: claude's workspace trust prompt).

// menuStyle is how one harness draws its menus (prompt_<harness>.go). A
// prompt watch names the harness whose menu it looks for; a chat message
// and an older daemon's watch do not, and every style is tried.
type menuStyle struct {
	// cursor marks the highlighted option of the harness's numbered
	// menus; "" when it has none.
	cursor string
	// dialogEdge reports whether a line above the options is the top edge
	// of the harness's dialog (menuSignature).
	dialogEdge func(line string) bool
	// unnumbered returns the options of a menu without numbers the lines
	// show, if any.
	unnumbered func(lines []string) ([]promptItem, bool)
	// framed, for a harness that draws its menus in a box, returns what
	// each of lines shows inside a box: the text between its side
	// borders, or outsideFrame for a line of no box. Its numbered menus
	// are read there only, an option's wrapped label continuing at the
	// column of its number.
	framed func(lines []string) []string
}

// outsideFrame stands for a line outside the boxes in a menuStyle.framed
// view: neither blank nor a key hint, it ends a menu above it.
const outsideFrame = "\x00"

// menuStyles has an entry for every harness of the registry.
var menuStyles = map[string]menuStyle{
	wire.HarnessClaude:  claudeMenus,
	wire.HarnessCodex:   codexMenus,
	wire.HarnessOmp:     ompMenus,
	wire.HarnessPi:      piMenus,
	wire.HarnessCopilot: copilotMenus,
}

// optionPattern matches an option line, `❯ 1. Yes` or `  2. No`, of a menu
// with one of cursors.
func optionPattern(cursors []string) *regexp.Regexp {
	quoted := make([]string, len(cursors))
	for i, c := range cursors {
		quoted[i] = regexp.QuoteMeta(c)
	}
	return regexp.MustCompile(`^(\s*)(?:(` + strings.Join(quoted, "|") + `)\s*)?(\d{1,2})\.\s+(\S.*)$`)
}

// promptOption: `❯ 1. Yes` (claude), `› 1. Yes, proceed (y)` (Codex),
// `  2. No`; the cursors are those of the menuStyles drawn without a
// frame.
var promptOption = func() *regexp.Regexp {
	var cursors []string
	for _, h := range harness.All {
		if s := menuStyles[h.ID]; s.cursor != "" && s.framed == nil && !slices.Contains(cursors, s.cursor) {
			cursors = append(cursors, s.cursor)
		}
	}
	return optionPattern(cursors)
}()

// framedOption holds the option pattern of each framed menuStyle, by its
// cursor.
var framedOption = func() map[string]*regexp.Regexp {
	m := map[string]*regexp.Regexp{}
	for _, s := range menuStyles {
		if s.framed != nil {
			m[s.cursor] = optionPattern([]string{s.cursor})
		}
	}
	return m
}()

// promptHint: key hints every captured menu shows under its options (`Esc
// to cancel · Tab to amend`, `ctrl+g to edit in Vim` under plan approval).
// A numbered list in the transcript has none.
var promptHint = regexp.MustCompile(`(?i)\besc\b|\benter\b|↑/↓|ctrl\+g`)

// promptTranscriptMark: transcript entries (`● `, `⎿`, `✻ …`, Codex's `• `
// and its status line's bullet blinking to `◦`) and prompts the user sent
// (`❯ `); a list above one is part of the conversation.
var promptTranscriptMark = regexp.MustCompile(`^(?:[●•◦⏺⎿✻✢✳✶✽✔✗■]|[›❯]\s)`)

// promptRuleChars make up horizontal rules and box edges.
const promptRuleChars = "─━═╌╍┄┅┈┉├┤┌┐└┘╭╮╰╯┬┴┼┝┥┠┨╞╡"

// promptEdgeChars make up the solid rule along the top of claude's dialogs.
const promptEdgeChars = "─━═▔"

type promptItem struct {
	indent     int
	cursor     string // a menuStyle's cursor on the highlighted option, else ""
	number     int
	col        int // the column of the number
	label      string
	start, end int // screen lines of the option (end: its last wrapped line)
}

// liveMenu returns the options of the menu lines (a screen, top to bottom)
// show, if any, as any harness draws its menus.
func liveMenu(lines []string) (options []promptItem, ok bool) {
	m, ok := findMenu(lines, "")
	return m.options, ok
}

// foundMenu is a menu on the screen and the style of the harness that drew
// it.
type foundMenu struct {
	options []promptItem
	style   menuStyle
}

// findMenu returns the menu lines show as harnessID draws its menus, or as
// any harness does when harnessID is "" or has no menus: the lowest
// numbered list with one option under the cursor and a key hint below it,
// in the harness's boxes for a framed style, or else a harness's menu
// without numbers (menuStyle.unnumbered).
func findMenu(lines []string, harnessID string) (foundMenu, bool) {
	own := menuStyles[harnessID]
	if own.framed != nil {
		return framedMenu(lines, own)
	}
	if group, ok := numberedMenu(lines, promptOption, false); ok {
		s := own
		if s.cursor == "" {
			s = plainStyle(group)
		}
		return foundMenu{group, s}, true
	}
	for _, h := range harness.All {
		if find := menuStyles[h.ID].unnumbered; find != nil {
			if options, ok := find(lines); ok {
				return foundMenu{options, menuStyles[h.ID]}, true
			}
		}
	}
	if own.cursor != "" {
		return foundMenu{}, false
	}
	for _, h := range harness.All {
		if s := menuStyles[h.ID]; s.framed != nil {
			if m, ok := framedMenu(lines, s); ok {
				return m, true
			}
		}
	}
	return foundMenu{}, false
}

// framedMenu returns the numbered menu in the boxes of style s.
func framedMenu(lines []string, s menuStyle) (foundMenu, bool) {
	group, ok := numberedMenu(s.framed(lines), framedOption[s.cursor], true)
	return foundMenu{group, s}, ok
}

// plainStyle is the style of the harness whose cursor highlights an
// option of group, of those drawing their menus without a frame.
func plainStyle(group []promptItem) menuStyle {
	for _, it := range group {
		if it.cursor == "" {
			continue
		}
		for _, h := range harness.All {
			if s := menuStyles[h.ID]; s.cursor == it.cursor && s.framed == nil {
				return s
			}
		}
	}
	return menuStyle{}
}

// numberedMenu returns the lowest numbered list of lines that option
// matches with one option under the cursor and a key hint below it. A
// line indented deeper than an option continues it (a wrapped label, a
// description); with hanging, so does one at the column of its number.
func numberedMenu(lines []string, option *regexp.Regexp, hanging bool) ([]promptItem, bool) {
	var groups [][]promptItem
	cur := -1 // index in groups of the menu being read
	// Blank lines and rules since the last line of the current menu; one is
	// allowed between options.
	gap := 0
	for i, text := range lines {
		if it, ok := parsePromptItem(option, text, i); ok {
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
		if in := indentOf(text); gap == 0 && (in > last.indent || hanging && in >= last.col) {
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
			if it.cursor != "" {
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
		return group, true
	}
	return nil, false
}

// permissionMenu reports whether lines (a screen, top to bottom) show a
// permission menu of harnessID (claude's permission or plan approval menu,
// Codex's or Copilot CLI's approval menu), or of any harness when
// harnessID is "", and its signature.
func permissionMenu(lines []string, harnessID string) (sig string, ok bool) {
	m, ok := findMenu(lines, harnessID)
	if !ok || m.options[0].number != 1 || !strings.HasPrefix(m.options[0].label, "Yes") {
		return "", false
	}
	edge := m.style.dialogEdge
	if edge == nil {
		edge = isDialogEdge
	}
	return menuSignature(lines, m.options[0].start, edge), true
}

// menuSignature identifies the dialog whose first option is on line first:
// the text above the options, from the dialog's top edge (a line edge
// reports: the dialogEdge of the style that drew the menu), or from the top
// of the screen when that edge scrolled away. That is the dialog's title,
// the command or file it asks about with its preview, and the question: it
// differs between the prompts of two tool calls, and stays the same while
// the dialog is up, whatever happens below it (the cursor moving between
// the options, Tab to amend) or above it (the blinking ● of the tool line,
// the transcript). Blanks, rule and box characters and the ●/⏺ marks are
// left out, so the same dialog wrapped at another width has the same
// signature. Two prompts for the very same tool call text share a
// signature: the second is not told apart from the first.
func menuSignature(lines []string, first int, edge func(line string) bool) string {
	top := 0
	for j := first - 1; j >= 0; j-- {
		if edge(lines[j]) {
			top = j + 1
			break
		}
	}
	var b strings.Builder
	for _, line := range lines[top:first] {
		for _, r := range line {
			if !unicode.IsSpace(r) && r != '●' && r != '⏺' && r != '│' &&
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

// startsInFirstColumn reports whether line has text in its first column.
func startsInFirstColumn(line string) bool {
	return line != "" && indentOf(line) == 0
}

func parsePromptItem(option *regexp.Regexp, text string, line int) (promptItem, bool) {
	m := option.FindStringSubmatchIndex(text)
	if m == nil {
		return promptItem{}, false
	}
	n, _ := strconv.Atoi(text[m[6]:m[7]])
	it := promptItem{
		indent: m[3] - m[2], number: n, label: text[m[8]:m[9]],
		col:   utf8.RuneCountInString(text[:m[6]]),
		start: line, end: line,
	}
	if m[4] >= 0 {
		it.cursor = text[m[4]:m[5]]
	}
	return it, true
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
