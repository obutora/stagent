package holder

import (
	"regexp"
	"strings"
)

// ompMenus: omp brings no approvals to the daemon, so the holder looks for
// no menu of its own; its question UI is a box of its own (ompQuestion).
var ompMenus = menuStyle{question: ompQuestion}

// ompQuestionTitle: the top edge of an omp dialog, a short rule and its
// title — the ask box (`╭─ Ask ───╮`) and the boxes its options open
// (`╭─ Note for Green: … ─╮`, `╭─ Custom answer: … ─╮`); while a question
// is in force omp shows no other. The transcript's block of the call
// (`╭─── Ask 1 questions`, `╭─── ? Ask` once answered) has a longer rule
// before its title, a tool's block none.
var ompQuestionTitle = regexp.MustCompile(`^╭─ [^─\s]`)

// ompQuestion reports whether lines show omp's question UI: a box at the
// bottom of the screen, nothing but blanks below it, whose top edge has a
// dialog's title; or, when that edge scrolled away, whose rows reach the
// top of the screen with an option under omp's cursor (`│ ❯ ○ Red`).
func ompQuestion(lines []string) bool {
	end := len(lines) - 1
	for end >= 0 && strings.TrimSpace(lines[end]) == "" {
		end--
	}
	if end < 0 || !strings.HasPrefix(lines[end], "╰") {
		return false
	}
	cursor := false
	for j := end - 1; j >= 0; j-- {
		switch line := lines[j]; {
		case strings.HasPrefix(line, "╭"):
			return ompQuestionTitle.MatchString(line)
		case strings.HasPrefix(line, "│"):
			cursor = cursor || strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(line, "│"), " "), "❯ ")
		case strings.HasPrefix(line, "├"):
		default:
			return false
		}
	}
	return cursor
}
