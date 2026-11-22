package holder

import (
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/harness"
)

// Prompt watch (holder.prompt_watch): while the daemon has an approval of
// the session pending from a harness whose menu it watches on screen (the
// daemon's watchPrompt), the holder looks for the program's permission
// menu on the screen and reports once the menu it saw is gone
// (holder.prompt_gone): the prompt was answered, wherever that happened.
// The programs themselves do not tell — claude's hook lives on until the
// approved tool finished, and the others' returned before their prompt
// showed (or with it). Antigravity CLI sends no hook for its menu at all:
// the daemon starts its watch when it checks a call's permission, with
// shown, and the holder reports the menu's coming as well
// (holder.prompt_shown), which registers the approval.
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
	// report: the menu's coming on the screen is reported too, when the
	// watch adopts it (promptWatch.shown).
	report bool
}

// promptWatch is the holder's side of the prompt watches, and of the
// question watches (question.go), which differ in the menu they look for.
// lines reads the screen; menu tells whether it shows the menu looked for
// as harnessID draws it, and its signature (permissionMenu, questionMenu);
// gone reports a watch whose menu left it, shown one started with
// startShown whose menu came on it (both called with mu held, they must
// not block or call back).
type promptWatch struct {
	lines func() []string
	menu  func(lines []string, harnessID string) (sig string, ok bool)
	gone  func(gen int64)
	shown func(gen int64)

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

func newPromptWatch(lines func() []string, menu func([]string, string) (string, bool), gone func(gen int64)) *promptWatch {
	return &promptWatch{lines: lines, menu: menu, gone: gone}
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
	w.startLocked(gen, harnessID, false)
}

// startShown starts watch gen as set does, for a menu no approval is
// registered for yet (holder.prompt_watch with shown): the menu's coming
// on the screen is reported (shown) before its leaving is.
func (w *promptWatch) startShown(gen int64, harnessID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.startLocked(gen, harnessID, true)
}

func (w *promptWatch) startLocked(gen int64, harnessID string, report bool) {
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
	w.tracks = append(kept, &promptTrack{gen: gen, report: report})
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
	sig, up := w.menu(w.lines(), w.harness)
	kept := w.tracks[:0] // the tracks still to be reported, in order
	for _, t := range w.tracks {
		switch {
		case !t.adopted:
			// The newest watch takes the menu on the screen, unless an
			// older watch is waiting for that menu to go.
			if up && !awaited(kept, sig) {
				t.adopted, t.sig, t.seen = true, sig, true
				if t.report && w.shown != nil {
					w.shown(t.gen)
				}
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
	_, ok := h.rules.liveMenu(h.scr.Lines())
	return ok
}

// ---------------------------------------------------------------------------
// Menu recognition, after the app's screen reader
// (lib/services/terminal_chat/agent_activity.dart): numbered options with
// one under a harness's cursor (claude's and Copilot CLI's ❯, Codex's and
// Qwen Code's ›) and a key hint below them. Of those menus, the permission
// dialogs are the ones whose first option is "1. Yes…": claude's
// permission prompt ("Do you want to …?" → "1. Yes") and plan approval
// ("Would you like to proceed?" → "1. Yes, and …"), Codex's approval
// ("Would you like to run the following command?" → "1. Yes, proceed
// (y)"), Copilot CLI's ("Do you want to run this command?" → "1. Yes", in a
// box). A harness may have a menu without numbers too, recognized its own
// way (claude's and Antigravity CLI's workspace trust prompts), menus with
// no key hint below them, told by their own end (Qwen Code's permission
// menu, whose labels are translated), options whose long labels wrap to
// the first column (Antigravity CLI's), dialogs right of a bar told by
// their rows and key hints instead (OpenCode's, Grok Build's), and a
// question UI that is no menu (Codex's, omp's).
//
// What is data and what is code. The screen rules (internal/screenrules:
// built in as builtin/<name>.json, overridden per host by
// ~/.ssh-term/agent/screen-rules/<name>.json, read when the holder starts)
// hold what a harness's screen shows: its cursor, the label of a
// permission prompt's first option, the patterns of key hints, transcript
// marks, headers, titles, rows and menu ends, and how many rows to look
// at. They pick by name the way to find a dialog's top edge (dialogEdges,
// barDialogEdge), a frame (boxFramed), where wrapped labels continue
// (wrapMode), a menu without numbers (titledDialogMenu), a menu's own end
// (markedLastOption), a dialog right of a bar (barDialogKinds) and a
// question UI (hintAndHeaderQuestion, bottomBoxQuestion). Those ways stay
// code, as do walking the rows
// (numberedMenu, promptHintFollows, menuSignature), the option line
// (optionPattern, radioRow) and the rule and box characters
// (promptRuleChars, promptEdgeChars).

// menuStyle is how one harness draws its menus, after its rules
// (newScreenRules). A prompt watch names the harness whose menu it looks
// for; a chat message and an older daemon's watch do not, and every style
// is tried.
type menuStyle struct {
	id string // the harness
	// cursor marks the highlighted option of the harness's numbered
	// menus; "" when it has none.
	cursor string
	// permission matches the label of a permission menu's first option.
	permission *regexp.Regexp
	// dialogEdge reports whether a line above the options is the top edge
	// of the harness's dialog (menuSignature).
	dialogEdge func(line string) bool
	// unnumbered returns the options of a menu without numbers the lines
	// show, if any.
	unnumbered func(lines []string) ([]promptItem, bool)
	// framed, for a harness that draws its menus in a box, returns what
	// each of lines shows inside a box: the text between its side
	// borders, or outsideFrame for a line of no box. Its numbered menus
	// are read there only (with option), an option's wrapped label
	// continuing at the column of its number.
	framed func(lines []string) []string
	// wrap is where an option's wrapped label continues: wrapFirstColumn
	// for a harness whose menus are read with option and that mode only
	// (wrappedMenu).
	wrap   wrapMode
	option *regexp.Regexp
	// question reports whether lines show the UI of a question the
	// harness asked through its question tool (questionShown), for a
	// harness that draws it as no menu of its own, and the lines that
	// tell; nil: its menus are. questionRule names the rule it is read
	// with (question, or dialogs for a question dialog).
	question     func(lines []string) (at []int, ok bool)
	questionRule string
	// ends, for a harness whose menus may have no key hint below them,
	// reports whether the numbered options group (one under cursor, read
	// with option) end a menu of its own on lines; nil: a key hint must
	// follow them. With endsPermission, such a menu is a permission menu.
	ends           func(lines []string, group []promptItem) bool
	endsPermission bool
	// dialogs, for a harness that draws its dialogs right of a bar,
	// returns the options of the first of them lines show and its name
	// (dialogPermission, dialogQuestion, dialogTrust).
	dialogs func(lines []string) (options []promptItem, dialog string, ok bool)
}

// outsideFrame stands for a line outside the boxes in a menuStyle.framed
// view: neither blank nor a key hint, it ends a menu above it.
const outsideFrame = "\x00"

// optionPattern matches an option line, `❯ 1. Yes` or `  2. No`, of a menu
// with one of cursors.
func optionPattern(cursors []string) *regexp.Regexp {
	quoted := make([]string, len(cursors))
	for i, c := range cursors {
		quoted[i] = regexp.QuoteMeta(c)
	}
	return regexp.MustCompile(`^(\s*)(?:(` + strings.Join(quoted, "|") + `)\s*)?(\d{1,2})\.\s+(\S.*)$`)
}

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
func (r *screenRules) liveMenu(lines []string) (options []promptItem, ok bool) {
	m, ok := r.findMenu(lines, "")
	return m.options, ok
}

// foundMenu is a menu on the screen and the style of the harness that drew
// it; unnumbered: it is the style's menu without numbers; dialog: it is
// the style's dialog of that name (menuStyle.dialogs).
type foundMenu struct {
	options    []promptItem
	style      menuStyle
	unnumbered bool
	dialog     string
}

// findMenu returns the menu lines show as harnessID draws its menus, or as
// any harness does when harnessID is "" or has no menus: the lowest
// numbered list with one option under the cursor and a key hint below it,
// in the harness's boxes for a framed style, or else a harness's menu
// without numbers (menuStyle.unnumbered) or a dialog right of a bar
// (menuStyle.dialogs). A harness with menus of its own (a cursor, menus
// without numbers or dialogs) has no other harness's menus.
func (r *screenRules) findMenu(lines []string, harnessID string) (foundMenu, bool) {
	own := r.styles[harnessID]
	mine := own.cursor != "" || own.unnumbered != nil || own.dialogs != nil
	if own.framed != nil {
		return r.framedMenu(lines, own)
	}
	if own.wrap != wrapIndented {
		if m, ok := r.wrappedMenu(lines, own); ok {
			return m, true
		}
	} else if group, ok := r.numberedMenu(lines, r.option, wrapIndented, r.hintFollows); ok && (own.cursor != "" || !mine) {
		s := own
		if s.cursor == "" {
			s = r.plainStyle(group)
		}
		return foundMenu{options: group, style: s}, true
	}
	if own.ends != nil {
		return r.endedMenu(lines, own)
	}
	if !mine {
		for _, h := range harness.All {
			s := r.styles[h.ID]
			read := r.endedMenu
			switch {
			case s.wrap != wrapIndented:
				read = r.wrappedMenu
			case s.ends == nil:
				continue
			}
			if m, ok := read(lines, s); ok {
				return m, true
			}
		}
	}
	for _, h := range harness.All {
		if mine && h.ID != harnessID {
			continue
		}
		s := r.styles[h.ID]
		if s.unnumbered != nil {
			if options, ok := s.unnumbered(lines); ok {
				return foundMenu{options: options, style: s, unnumbered: true}, true
			}
		}
		if s.dialogs != nil {
			if options, dialog, ok := s.dialogs(lines); ok {
				return foundMenu{options: options, style: s, dialog: dialog}, true
			}
		}
	}
	if mine {
		return foundMenu{}, false
	}
	for _, h := range harness.All {
		if s := r.styles[h.ID]; s.framed != nil {
			if m, ok := r.framedMenu(lines, s); ok {
				return m, true
			}
		}
	}
	return foundMenu{}, false
}

// framedMenu returns the numbered menu in the boxes of style s.
func (r *screenRules) framedMenu(lines []string, s menuStyle) (foundMenu, bool) {
	group, ok := r.numberedMenu(s.framed(lines), s.option, wrapHanging, r.hintFollows)
	return foundMenu{options: group, style: s}, ok
}

// endedMenu returns the numbered menu under the cursor of style s that its
// ends tells, a key hint below it or not.
func (r *screenRules) endedMenu(lines []string, s menuStyle) (foundMenu, bool) {
	group, ok := r.numberedMenu(lines, s.option, wrapIndented, func(lines []string, group []promptItem) bool {
		return r.hintFollows(lines, group) || s.ends(lines, group)
	})
	return foundMenu{options: group, style: s}, ok
}

// wrappedMenu returns the numbered menu under the cursor of style s, whose
// options' labels wrap as its wrap says.
func (r *screenRules) wrappedMenu(lines []string, s menuStyle) (foundMenu, bool) {
	group, ok := r.numberedMenu(lines, s.option, s.wrap, r.hintFollows)
	return foundMenu{options: group, style: s}, ok
}

// hintFollows: a key hint follows the options of group (promptHintFollows).
func (r *screenRules) hintFollows(lines []string, group []promptItem) bool {
	return r.promptHintFollows(lines, group[len(group)-1].end)
}

// plainStyle is the style of the harness whose cursor highlights an
// option of group, of those drawing their menus without a frame.
func (r *screenRules) plainStyle(group []promptItem) menuStyle {
	for _, it := range group {
		if it.cursor == "" {
			continue
		}
		for _, h := range harness.All {
			if s := r.styles[h.ID]; s.cursor == it.cursor && s.framed == nil {
				return s
			}
		}
	}
	return menuStyle{}
}

// wrapMode is where the wrapped label of an option continues on the lines
// right under it (numberedMenu): indented deeper than the option (also a
// description under it); with wrapHanging also at the column of its
// number (a framed menu); with wrapFirstColumn also in the first column,
// which continues the label itself.
//
// Antigravity CLI (1.5.14; screenrules builtin/antigravity.json)
// highlights an option with > (`> 1. Yes, run command` over `  2. …`) and
// wraps a long label to the first column (`that start with 'curl …'`).
// Its permission menus sit under a full-width rule below their title
// (Command, Create file, …; screenrules.EdgeSolidRule) and end with the
// question (`Run this command?`), the first option allowing the call; its
// key hint is below the options (`↑/↓ Navigate · tab Amend · …`). Its
// ask_question menu is a plain menu of the same kind, the last option the
// one to type an answer in (`Write-in...`). Its folder trust prompt
// (`Accessing workspace:` at the top of an otherwise empty screen) has
// two options without numbers, `> Yes, I trust this folder` and `No,
// exit`. Its input box is a `> ` line between two rules, which a draft
// numbered like a menu does not make one.
type wrapMode int

const (
	wrapIndented wrapMode = iota
	wrapHanging
	wrapFirstColumn
)

// numberedMenu returns the lowest numbered list of lines that option
// matches with one option under the cursor and its end told by ends (a key
// hint below it: hintFollows). The lines continuing an option (wrap) are
// part of it.
func (r *screenRules) numberedMenu(lines []string, option *regexp.Regexp, wrap wrapMode, ends func(lines []string, group []promptItem) bool) ([]promptItem, bool) {
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
		in := indentOf(text)
		switch {
		case gap == 0 && (in > last.indent || wrap == wrapHanging && in >= last.col):
			last.end = i // a wrapped label or a description under it
			continue
		case gap == 0 && wrap == wrapFirstColumn && in == 0:
			last.end = i
			last.label += " " + t
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
		if !ends(lines, group) {
			continue
		}
		return group, true
	}
	return nil, false
}

// permissionMenu reports whether lines (a screen, top to bottom) show a
// permission menu of harnessID (claude's permission or plan approval menu,
// the approval menu or dialog of the others), or of any harness when
// harnessID is "", and its signature.
func (r *screenRules) permissionMenu(lines []string, harnessID string) (sig string, ok bool) {
	m, ok := r.findMenu(lines, harnessID)
	if !ok || !m.permission(lines) {
		return "", false
	}
	return menuSignature(lines, m.options[0].start, m.edge()), true
}

// permission reports whether m, on lines, is a permission menu: the
// style's permission dialog, or a menu whose first option, 1, has the
// label of one (menuStyle.permission) or that its own end tells
// (menuStyle.endsPermission).
func (m foundMenu) permission(lines []string) bool {
	if m.dialog != "" {
		return m.dialog == dialogPermission
	}
	first := m.options[0]
	if first.number != 1 {
		return false
	}
	if p := m.style.permission; p != nil && p.MatchString(first.label) {
		return true
	}
	return m.style.endsPermission && m.style.ends(lines, m.options)
}

// edge is the top edge of m's dialog (menuSignature).
func (m foundMenu) edge() func(line string) bool {
	if m.style.dialogEdge != nil {
		return m.style.dialogEdge
	}
	return isDialogEdge
}

// menuSignature identifies the dialog whose first option is on line first:
// the text above the options, from the dialog's top edge (a line edge
// reports: the dialogEdge of the style that drew the menu), or from the top
// of the screen when that edge scrolled away. That is the dialog's title,
// the command or file it asks about with its preview, and the question: it
// differs between the prompts of two tool calls, and stays the same while
// the dialog is up, whatever happens below it (the cursor moving between
// the options, Tab to amend) or above it (the blinking ● of the tool line,
// the transcript). Blanks, rule and box characters, the bars left of Grok
// Build's dialogs and the ●/⏺ marks are left out, so the same dialog
// wrapped at another width has the same signature. Two prompts for the
// very same tool call text share a signature: the second is not told apart
// from the first.
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
			if !unicode.IsSpace(r) && r != '●' && r != '⏺' && r != '│' && r != '┃' && r != '┆' &&
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

// promptHintFollows reports a key hint within the few lines (hintRows)
// under the options, or nothing but blanks below them. A transcript entry
// below means the list is part of the conversation, e.g. a numbered prompt
// the user sent. When the bottom edge of a box beside the options comes
// right under them (claude's question with previews), what is in that
// column below it — its notes field, a note typed there however many
// lines it wraps to — is not counted.
func (r *screenRules) promptHintFollows(lines []string, end int) bool {
	side := 0 // the column of the box beside the options
	seen := 0
	for j := end + 1; j < len(lines) && seen < r.hintRows; j++ {
		t := strings.TrimSpace(lines[j])
		if side == 0 && seen == 0 && strings.HasPrefix(t, "└") {
			side = indentOf(lines[j])
		}
		if t == "" || isPromptRule(t) || side > 0 && indentOf(lines[j]) >= side {
			continue
		}
		if r.transcriptMark.MatchString(t) {
			return false
		}
		if r.hint.MatchString(t) {
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
