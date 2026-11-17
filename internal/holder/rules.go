package holder

import (
	"regexp"
	"slices"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/screenrules"
)

// screenRules is a screenrules.Set made ready to read screens with: one
// menuStyle per harness and the patterns every menu shares.
type screenRules struct {
	set *screenrules.Set
	// styles has an entry for every harness of the registry.
	styles map[string]menuStyle
	// option matches an option line, `❯ 1. Yes` (claude), `› 1. Yes,
	// proceed (y)` (Codex) or `  2. No`, of the menus drawn without a
	// frame.
	option *regexp.Regexp
	// hint: key hints every captured menu shows within hintRows lines
	// under its options (`Esc to cancel · Tab to amend`, `ctrl+g to edit
	// in Vim` under plan approval); a numbered list in the transcript has
	// none.
	hint     *regexp.Regexp
	hintRows int
	// transcriptMark: transcript entries (`● `, `⎿`, `✻ …`, Codex's `• `
	// and its status line's bullet blinking to `◦`) and prompts the user
	// sent (`❯ `); a list above one is part of the conversation.
	transcriptMark *regexp.Regexp
}

// dialogEdges are the ways to find a dialog's top edge, by their names
// in the rules (screenrules.Edge*).
var dialogEdges = map[string]func(line string) bool{
	screenrules.EdgeSolidRule:     isDialogEdge,
	screenrules.EdgeFirstColumn:   startsInFirstColumn,
	screenrules.EdgeBoxTop:        isBoxTop,
	screenrules.EdgeShallowIndent: isShallowIndent,
}

// loadScreenRules reads the screen rules in force on this host: the
// built-in ones with the overrides in dir; logf names an override that is
// not used.
func loadScreenRules(dir string, logf func(string, ...any)) *screenRules {
	set := screenrules.Load(dir)
	for _, f := range set.Files {
		if f.Err != nil {
			logf("stagent run: screen rules %s not used, the built-in rules apply: %v", f.Path, f.Err)
		}
	}
	return newScreenRules(set)
}

func newScreenRules(set *screenrules.Set) *screenRules {
	c := set.Common.Menu
	r := &screenRules{
		set: set, styles: map[string]menuStyle{},
		hint: c.Hint.Regexp, hintRows: c.HintRows, transcriptMark: c.TranscriptMark.Regexp,
	}
	var cursors []string
	for _, h := range harness.All {
		rules := set.Harnesses[h.ID]
		s := menuStyle{id: h.ID}
		if m := rules.Menu; m != nil {
			s.cursor = m.Cursor
			if m.PermissionLabel != nil {
				s.permission = m.PermissionLabel.Regexp
			}
			s.dialogEdge = dialogEdges[m.DialogEdge]
			if m.Frame == screenrules.FrameBox {
				s.framed = boxFramed
			} else if !slices.Contains(cursors, m.Cursor) {
				cursors = append(cursors, m.Cursor)
			}
			if e := m.Ends; e != nil {
				s.ends = markedLastOption(e.LastLabel.Regexp, e.Below.Regexp, e.Rows, e.ScrollBar)
				s.endsPermission = e.Permission
			}
			if s.framed != nil || s.ends != nil {
				s.option = optionPattern([]string{m.Cursor})
			}
			if u := rules.Unnumbered; u != nil {
				s.unnumbered = r.titledDialogMenu(m.Cursor, s.dialogEdge, u.Title)
			}
		}
		if q := rules.Question; q != nil {
			s.questionRule = "question"
			switch q.Kind {
			case screenrules.QuestionHintAndHeader:
				s.question = hintAndHeaderQuestion(q.Header.Regexp, q.Hint.Regexp, q.HintRows)
			case screenrules.QuestionBottomBox:
				s.question = bottomBoxQuestion(q.Title.Regexp, q.Cursor)
			}
		}
		if d := rules.Dialogs; d != nil {
			if s.dialogEdge == nil {
				s.dialogEdge = barDialogEdge(d)
			}
			s.dialogs = barDialogs(d)
			if d.Question != nil {
				read := barDialogKinds[d.Question.Kind](dialogBar{d.Bar, d.ScrollBar}, d.Question)
				s.question = func(lines []string) ([]int, bool) {
					items, ok := read(lines)
					var at []int
					for _, it := range items {
						at = append(at, it.start)
					}
					return at, ok
				}
				s.questionRule = "dialogs"
			}
		}
		r.styles[h.ID] = s
	}
	r.option = optionPattern(cursors)
	return r
}

// barDialogs returns the reader of the dialogs right of a bar d has: the
// options of the first of them (permission, question, trust) lines show,
// and its name.
func barDialogs(d *screenrules.Dialogs) func(lines []string) ([]promptItem, string, bool) {
	type dialog struct {
		name string
		read func(lines []string) ([]promptItem, bool)
	}
	b := dialogBar{d.Bar, d.ScrollBar}
	var dialogs []dialog
	for _, c := range []struct {
		name string
		d    *screenrules.Dialog
	}{{dialogPermission, d.Permission}, {dialogQuestion, d.Question}, {dialogTrust, d.Trust}} {
		if c.d != nil {
			dialogs = append(dialogs, dialog{c.name, barDialogKinds[c.d.Kind](b, c.d)})
		}
	}
	return func(lines []string) ([]promptItem, string, bool) {
		for _, d := range dialogs {
			if items, ok := d.read(lines); ok {
				return items, d.name, true
			}
		}
		return nil, "", false
	}
}
