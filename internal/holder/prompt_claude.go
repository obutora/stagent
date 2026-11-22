package holder

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// claude (screenrules builtin/claude.json) highlights an option with ❯
// and draws its dialogs under a full-width `───` or, for plan approval,
// `▔▔▔` (rules inside the dialog are indented or dashed): the edge is the
// nearest rule above the options that starts in the first column
// (screenrules.EdgeSolidRule). Its workspace trust prompt (#491), shown at
// startup in a folder not trusted yet, before the initial prompt is sent,
// has options without numbers — `❯ No, exit` over `  Yes, I trust this
// folder` — in a dialog titled `Accessing workspace:`
// (screenrules.UnnumberedTitledDialog).

// titledDialogMenu returns the reader of a menu without numbers drawn
// with cursor in the dialog titled title: unnumbered options in one
// column, one of them under the cursor, with a key hint below them, in the
// dialog (under its top edge, which edge finds) whose first line is
// title. Such options are recognized in that dialog only: anywhere else a
// list without numbers is no menu.
func (r *screenRules) titledDialogMenu(cursor string, edge func(line string) bool, title string) func(lines []string) ([]promptItem, bool) {
	at := regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(cursor) + `\s+)\S`)
	return func(lines []string) (options []promptItem, ok bool) {
		for i := len(lines) - 1; i >= 0; i-- {
			m := at.FindStringSubmatchIndex(lines[i])
			if m == nil {
				continue
			}
			col := utf8.RuneCountInString(lines[i][:m[3]])
			option := func(j int) bool {
				t := strings.TrimSpace(lines[j])
				return t != "" && indentOf(lines[j]) == col
			}
			first, last := i, i
			for first > 0 && option(first-1) {
				first--
			}
			for last+1 < len(lines) && option(last+1) {
				last++
			}
			if first == last || !titledDialog(lines, first, edge, title) || !r.promptHintFollows(lines, last) {
				return nil, false
			}
			for j := first; j <= last; j++ {
				it := promptItem{indent: col, label: strings.TrimSpace(lines[j]), start: j, end: j}
				if j == i {
					it.indent = indentOf(lines[j])
					it.cursor = cursor
					it.label = strings.TrimSpace(lines[j][m[3]:])
				}
				options = append(options, it)
			}
			return options, true
		}
		return nil, false
	}
}

// titledDialog reports whether the dialog the options from line first on
// are in is titled title: its first line under the top edge, or on the
// screen when no edge is above the options (Antigravity CLI draws its
// trust prompt at the top of an otherwise empty screen).
func titledDialog(lines []string, first int, edge func(line string) bool, title string) bool {
	top := 0
	for j := first - 1; j >= 0; j-- {
		if edge(lines[j]) {
			top = j + 1
			break
		}
	}
	for _, line := range lines[top:first] {
		if t := strings.TrimSpace(line); t != "" {
			return t == title
		}
	}
	return false
}
