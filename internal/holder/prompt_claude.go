package holder

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// claudeMenus: claude highlights an option with ❯ and draws its dialogs
// under a full-width `───` or, for plan approval, `▔▔▔` (rules inside the
// dialog are indented or dashed): the edge is the nearest rule above the
// options that starts in the first column. Its workspace trust prompt has
// options without numbers.
var claudeMenus = menuStyle{
	cursor:     "❯",
	dialogEdge: isDialogEdge,
	unnumbered: trustMenu,
}

// trustTitle is the title of claude's workspace trust prompt (#491), shown
// at startup in a folder not trusted yet, before the initial prompt is sent.
const trustTitle = "Accessing workspace:"

// trustCursor: the highlighted option of claude's trust prompt, which has
// no numbers: `❯ No, exit` over `  Yes, I trust this folder`.
var trustCursor = regexp.MustCompile(`^(\s*❯\s+)\S`)

// trustMenu returns the options of claude's workspace trust prompt, if
// lines show it: unnumbered options in one column, one of them under ❯,
// with a key hint below them, in the dialog (under its top edge, see
// menuSignature) titled trustTitle. Its options without a number are
// recognized in that dialog only: anywhere else such a list is no menu.
func trustMenu(lines []string) (options []promptItem, ok bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		m := trustCursor.FindStringSubmatchIndex(lines[i])
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
		if first == last || !trustDialog(lines, first) || !promptHintFollows(lines, last) {
			return nil, false
		}
		for j := first; j <= last; j++ {
			it := promptItem{indent: col, label: strings.TrimSpace(lines[j]), start: j, end: j}
			if j == i {
				it.indent = indentOf(lines[j])
				it.cursor = "❯"
				it.label = strings.TrimSpace(lines[j][m[3]:])
			}
			options = append(options, it)
		}
		return options, true
	}
	return nil, false
}

// trustDialog reports whether the dialog the options from line first on
// are in is titled trustTitle: its first line under the top edge.
func trustDialog(lines []string, first int) bool {
	for j := first - 1; j >= 0; j-- {
		if !isDialogEdge(lines[j]) {
			continue
		}
		for _, line := range lines[j+1 : first] {
			if t := strings.TrimSpace(line); t != "" {
				return t == trustTitle
			}
		}
		return false
	}
	return false
}
