package holder

import (
	"regexp"
	"strings"
)

// Qwen Code (0.27.4; screenrules builtin/qwen.json) highlights an option
// with Codex's › and draws its permission menu below the tool call it asks
// about (`? Shell echo hi ←` at the transcript's indent, the command or
// the diff, the question), with no key hint under the options: only its
// spinner line (`⠏ Waiting for user confirmation...`) below them. Its
// labels are translated (`No, suggest changes (esc)`, `いいえ、変更を提案
// (Esc)`) and cut off with … to the width; the rows differ with the tool,
// the folder's trust and the approval mode, but the first allows the call
// once and the last turns it down, marked with the key that does so in
// parentheses. So a menu of its own ends in a last option marked so,
// followed by nothing or its spinner (screenrules.Ends, read by
// markedLastOption), and is its permission menu (Ends.Permission). The
// dialog's edge is the nearest line above the options at the transcript's
// indent, the tool call's own line or the entry above it
// (screenrules.EdgeShallowIndent). Its ask_user_question menu (❯, a key
// hint below) is a plain menu, and no permission menu: a key hint follows
// it, and its last option is the one to type an answer in.

// isShallowIndent reports whether line has text indented two columns at
// most.
func isShallowIndent(line string) bool {
	return strings.TrimSpace(line) != "" && indentOf(line) <= 2
}

// markedLastOption returns the reader of a menu's own end: the label of
// its last option matches last and, within rows rows under that option,
// there is nothing but blanks before a line matching below or the bottom
// of the screen; scrollBar, drawn in the last column, is left out of both.
func markedLastOption(last, below *regexp.Regexp, rows int, scrollBar string) func(lines []string, group []promptItem) bool {
	cut := " " + scrollBar
	return func(lines []string, group []promptItem) bool {
		end := group[len(group)-1]
		if !last.MatchString(strings.TrimRight(end.label, cut)) {
			return false
		}
		for j := end.end + 1; j < len(lines) && j <= end.end+rows; j++ {
			if t := strings.Trim(lines[j], cut); t != "" {
				return below.MatchString(t)
			}
		}
		return true
	}
}
