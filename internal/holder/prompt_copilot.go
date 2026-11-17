package holder

import "strings"

// Copilot CLI (1.0.98, cli-main.js; screenrules builtin/copilot.json)
// draws its permission dialog as a rounded box (`╭─ … ─╮`, `│ … │`, `╰─ …
// ─╯`: the Dialog component) holding its title, a divider, the body (the
// command in a box of its own, the question), the options and the key
// hints. The options are numbered, the highlighted one under ❯ — claude's
// cursor, which is why its menus are read inside its boxes only
// (screenrules.FrameBox) — and a wrapped label continues at the column of
// its number. The dialog's edge is its top border, the nearest line above
// the options whose text starts with ╭ (screenrules.EdgeBoxTop: a box
// inside the dialog starts with the dialog's │).

// isBoxTop reports whether line is the top border of a box.
func isBoxTop(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "╭")
}

// boxFramed returns the text inside the outermost box of each line: what
// lies between its │ borders, less the one column of padding the dialog
// keeps; outsideFrame for a line of no box.
func boxFramed(lines []string) []string {
	out := make([]string, len(lines))
	for i, line := range lines {
		t := strings.TrimSpace(line)
		inner, ok := strings.CutPrefix(t, "│")
		if ok {
			inner, ok = strings.CutSuffix(inner, "│")
		}
		if !ok {
			out[i] = outsideFrame
			continue
		}
		out[i] = strings.TrimRight(strings.TrimPrefix(inner, " "), " ")
	}
	return out
}
