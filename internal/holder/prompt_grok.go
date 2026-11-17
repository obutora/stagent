package holder

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/screenrules"
)

// Grok Build (1.0.77; screenrules builtin/grok.json) draws its permission
// menu and its question card right of a bar (`  ┃  …`:
// screenrules.Dialogs), the rows numbered with a radio mark after the
// number, ● on the highlighted one (`┃  2 (●) Yes, proceed`;
// screenrules.DialogRadioRows); the title above them starts with › (`┃  ›
// Run a command…`), which is no cursor. Its permission menu has 4 or 5
// rows — which number allows the call depends on the menu: 2 for a shell
// command, 3 for a command on its dangerous list or an edit — and its key
// hint below the bar (`1/4:select  │  Tab:next option`; `Tab/Space:permission
// │ …` once Esc parked the focus in the scrollback). A digit picks and
// confirms its row wherever the highlight is, which Grok Build moves to
// the kind of row answered last. Its question card has the rows of the
// options and a last one to type an answer in (`z (○) Type your answer
// here`; `z (●) ❯ …` while it is typed), with its key hint inside the bar
// (`↑/↓ navigate · y copy  Enter:submit`). Its folder trust prompt, before
// the conversation starts, has two rows without numbers (`Yes, proceed
// y`, `No, quit  n`; screenrules.DialogTitledRows). A dialog's edge is the
// line above its bar (screenrules.EdgeOutsideBar); a scroll bar (█) may
// be drawn in the last column.

// radioRow: a row of a dialog right of the bar, its number (z: the typed
// answer), radio mark and label (a question option's description after
// it, two spaces apart).
var radioRow = regexp.MustCompile(`^([1-9]|z) \(([○●])\) (\S.*)$`)

// radioRowsDialog returns the reader of a dialog whose options are the
// lowest consecutive rows in the bar numbered from 1 (z last) with a
// radio mark, with d's key hint below them.
func radioRowsDialog(b dialogBar, d *screenrules.Dialog) func(lines []string) ([]promptItem, bool) {
	below := hintBelow(b, d)
	row := func(line string) bool {
		t, inBar := b.text(line)
		return inBar && radioRow.MatchString(t)
	}
	return func(lines []string) ([]promptItem, bool) {
		for i := len(lines) - 1; i >= 0; i-- {
			if !row(lines[i]) {
				continue
			}
			first := i
			for first > 0 && row(lines[first-1]) {
				first--
			}
			items := make([]promptItem, 0, i-first+1)
			for j := first; j <= i; j++ {
				t, _ := b.text(lines[j])
				m := radioRow.FindStringSubmatch(t)
				n := len(items) + 1
				if m[1] != "z" && int(m[1][0]-'0') != n || m[1] == "z" && j != i {
					return nil, false
				}
				it := promptItem{
					number: n, label: rowGap.Split(m[3], 2)[0],
					indent: indentOf(lines[j]), start: j, end: j,
					col: utf8.RuneCountInString(lines[j]) - utf8.RuneCountInString(strings.TrimLeft(lines[j], " "+b.bar)),
				}
				if m[2] == "●" {
					it.cursor = "●"
				}
				items = append(items, it)
			}
			if len(items) < 2 || !below(lines, i) {
				return nil, false
			}
			return items, true
		}
		return nil, false
	}
}

// titledRowsDialog returns the reader of a dialog without a bar: under its
// title line (d.Title), consecutive lines matching d.Rows one by one, an
// option's label its text up to two spaces.
func titledRowsDialog(_ dialogBar, d *screenrules.Dialog) func(lines []string) ([]promptItem, bool) {
	title, rows := d.Title, d.Rows
	return func(lines []string) ([]promptItem, bool) {
		for i, line := range lines {
			if strings.TrimSpace(line) != title {
				continue
			}
			for j := i + 1; j+len(rows) <= len(lines); j++ {
				if !rows[0].MatchString(strings.TrimSpace(lines[j])) {
					continue
				}
				items := make([]promptItem, len(rows))
				for k := range items {
					l := lines[j+k]
					if !rows[k].MatchString(strings.TrimSpace(l)) {
						return nil, false
					}
					items[k] = promptItem{number: k + 1, label: rowGap.Split(strings.TrimSpace(l), 2)[0], indent: indentOf(l), start: j + k, end: j + k}
				}
				return items, true
			}
			return nil, false
		}
		return nil, false
	}
}
