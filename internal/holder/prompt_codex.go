package holder

import (
	"regexp"
	"strings"
)

// Codex (screenrules builtin/codex.json) highlights an option with › and
// indents every line of its dialog: the edge is the nearest line above the
// options that starts in the first column (screenrules.EdgeFirstColumn:
// the transcript entry above, e.g. `• Running …`). Its request_user_input
// UI (Codex 0.160.1) is told by its own rows
// (screenrules.QuestionHintAndHeader): the notes field Tab opens there (`›
// Add notes`) reads as a prompt the user sent, not as a menu. Its first
// row is the header `Question 2/2 (1 unanswered)` — once answered, the
// transcript has a `• Questions 2/2 answered` entry instead — and the key
// hint under it, for its options and for its notes field alike, ends `…
// | enter to submit answer` (more questions to go) or `… enter to submit
// all` (the last), wrapping to up to three rows at a narrow width (`tab to
// add notes | enter to submit answer`, `←/→ to navigate questions`, `esc
// to interrupt`).

// hintAndHeaderQuestion returns the reader of a question UI with hint
// among the last hintRows rows of the screen and, above it, header with
// nothing between starting in the first column (a transcript entry, a
// prompt the user sent); or, when the header scrolled away, its indented
// rows reaching the top of the screen. at is the header's line, when on
// the screen, and the hint's.
func hintAndHeaderQuestion(header, hint *regexp.Regexp, hintRows int) func(lines []string) (at []int, ok bool) {
	return func(lines []string) ([]int, bool) {
		end := len(lines) - 1
		for end >= 0 && strings.TrimSpace(lines[end]) == "" {
			end--
		}
		h := -1
		for j, rows := end, 0; j >= 0 && rows < hintRows; j-- {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if hint.MatchString(lines[j]) {
				h = j
				break
			}
			rows++
		}
		if h < 0 {
			return nil, false
		}
		for j := h - 1; j >= 0; j-- {
			switch line := lines[j]; {
			case header.MatchString(line):
				return []int{j, h}, true
			case strings.TrimSpace(line) != "" && startsInFirstColumn(line):
				return nil, false
			}
		}
		return []int{h}, true
	}
}
