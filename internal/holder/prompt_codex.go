package holder

import (
	"regexp"
	"strings"
)

// codexMenus: Codex highlights an option with › and indents every line of
// its dialog: the edge is the nearest line above the options that starts in
// the first column (the transcript entry above, e.g. `• Running …`). Its
// request_user_input UI is told by its own rows (codexQuestion): the notes
// field Tab opens there (`› Add notes`) reads as a prompt the user sent,
// not as a menu.
var codexMenus = menuStyle{
	cursor:     "›",
	dialogEdge: startsInFirstColumn,
	question:   codexQuestion,
}

// codexQuestionHeader is the first row of Codex's request_user_input UI,
// `Question 2/2 (1 unanswered)`. Once answered, the transcript has a
// `• Questions 2/2 answered` entry instead.
var codexQuestionHeader = regexp.MustCompile(`^\s+Question \d+/\d+(?:\s|$)`)

// codexQuestionHint is the key hint under it, for its options and for its
// notes field alike: `… | enter to submit answer` (more questions to go)
// or `… enter to submit all` (the last).
var codexQuestionHint = regexp.MustCompile(`\benter to submit (?:answer|all)\b`)

// codexQuestionHintRows: the hint wraps to up to three rows at a narrow
// width (`tab to add notes | enter to submit answer`, `←/→ to navigate
// questions`, `esc to interrupt`).
const codexQuestionHintRows = 3

// codexQuestion reports whether lines show Codex's request_user_input UI
// (Codex 0.160.1): its hint among the last rows of the screen and, above
// it, its header with nothing between starting in the first column (a
// transcript entry, a prompt the user sent); or, when the header scrolled
// away, its indented rows reaching the top of the screen.
func codexQuestion(lines []string) bool {
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	hint := -1
	for j, rows := end, 0; j >= 0 && rows < codexQuestionHintRows; j-- {
		if strings.TrimSpace(lines[j]) == "" {
			continue
		}
		if codexQuestionHint.MatchString(lines[j]) {
			hint = j
			break
		}
		rows++
	}
	if hint < 0 {
		return false
	}
	for j := hint - 1; j >= 0; j-- {
		switch line := lines[j]; {
		case codexQuestionHeader.MatchString(line):
			return true
		case strings.TrimSpace(line) != "" && startsInFirstColumn(line):
			return false
		}
	}
	return true
}
