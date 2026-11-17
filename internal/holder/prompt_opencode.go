package holder

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/screenrules"
)

// OpenCode (2.3.0; screenrules builtin/opencode.json) draws its dialogs
// right of a coloured bar (`  ┃  …`: screenrules.Dialogs). Its permission
// dialog (`△ Permission required`, what is asked, a preview) has its
// options on one row, without numbers or a cursor — the highlight is a
// background colour only —, `Allow once   Always allow   Reject` (`Allow
// once   Reject` when nothing can be allowed always), with its key hint
// below (`ctrl+f fullscreen  ⇆ select  enter confirm`, cut off at the
// screen's width): screenrules.DialogChoiceRow. Its question tool's dialog
// numbers its options (`1. Red`, a description under each, the last one
// the answer typed in) with no cursor either, its key hint below (`↑↓
// select  enter submit  esc dismiss`; `enter submit  esc close` while the
// typed answer is open): screenrules.DialogNumberedRows. The permission
// dialog's edge is its title line, the one with △
// (screenrules.EdgeTitleMark).

// Names of a harness's dialogs right of a bar (the members of
// screenrules.Dialogs).
const (
	dialogPermission = "permission"
	dialogQuestion   = "question"
	dialogTrust      = "trust"
)

// dialogBar is the bar left of a harness's dialogs and the scroll bar it
// draws in the last column (screenrules.Dialogs).
type dialogBar struct{ bar, scrollBar string }

// text returns line without the bar and the scroll bar, trimmed; inBar
// tells whether the line is in the bar.
func (b dialogBar) text(line string) (text string, inBar bool) {
	t := strings.TrimSpace(strings.TrimRight(line, " "+b.scrollBar))
	if rest, ok := strings.CutPrefix(t, b.bar); ok {
		return strings.TrimSpace(rest), true
	}
	return t, false
}

// barDialogEdge returns the way to find the top edge of a dialog of d:
// the nearest line above its options that is out of the bar
// (screenrules.EdgeOutsideBar) or holds its title mark
// (screenrules.EdgeTitleMark).
func barDialogEdge(d *screenrules.Dialogs) func(line string) bool {
	if d.DialogEdge == screenrules.EdgeTitleMark {
		mark := d.TitleMark
		return func(line string) bool { return strings.Contains(line, mark) }
	}
	bar := d.Bar
	return func(line string) bool { return !strings.HasPrefix(strings.TrimSpace(line), bar) }
}

// barDialogKinds are the ways to read a dialog right of a bar, by their
// names in the rules (screenrules.Dialog*).
var barDialogKinds = map[string]func(b dialogBar, d *screenrules.Dialog) func(lines []string) ([]promptItem, bool){
	screenrules.DialogChoiceRow:    choiceRowDialog,
	screenrules.DialogNumberedRows: numberedRowsDialog,
	screenrules.DialogRadioRows:    radioRowsDialog,
	screenrules.DialogTitledRows:   titledRowsDialog,
}

var (
	// rowGap parts the options on a row, and an option's label from what
	// follows it.
	rowGap = regexp.MustCompile(`\s{2,}`)
	// numberedRow: an option of a dialog numbered without a cursor.
	numberedRow = regexp.MustCompile(`^(\d{1,2})\.\s+(\S.*)$`)
)

// hintBelow returns whether d's key hint is on one of the d.HintRows
// non-blank lines under line i, where d.HintPlace says.
func hintBelow(b dialogBar, d *screenrules.Dialog) func(lines []string, i int) bool {
	hint, rows, place := d.Hint.Regexp, d.HintRows, d.HintPlace
	return func(lines []string, i int) bool {
		seen := 0
		for j := i + 1; j < len(lines) && seen < rows; j++ {
			t, inBar := b.text(lines[j])
			if t == "" {
				continue
			}
			if hint.MatchString(t) && (place == "" || (place == screenrules.HintInBar) == inBar) {
				return true
			}
			seen++
		}
		return false
	}
}

// choiceRowDialog returns the reader of a dialog whose options are on one
// row matching d.Row, two or more spaces apart, with its key hint below:
// the lowest such row.
func choiceRowDialog(b dialogBar, d *screenrules.Dialog) func(lines []string) ([]promptItem, bool) {
	row, below := d.Row.Regexp, hintBelow(b, d)
	return func(lines []string) ([]promptItem, bool) {
		for i := len(lines) - 1; i >= 0; i-- {
			t, _ := b.text(lines[i])
			if !row.MatchString(t) {
				continue
			}
			if !below(lines, i) {
				return nil, false
			}
			var items []promptItem
			for n, label := range rowGap.Split(t, -1) {
				items = append(items, promptItem{number: n + 1, label: label, start: i, end: i, indent: indentOf(lines[i])})
			}
			return items, true
		}
		return nil, false
	}
}

// numberedRowsDialog returns the reader of a dialog whose options are
// numbered without a cursor (1, 2, …; a description indented under each)
// right above its key hint, the lowest line matching d.Hint.
func numberedRowsDialog(b dialogBar, d *screenrules.Dialog) func(lines []string) ([]promptItem, bool) {
	hint := d.Hint.Regexp
	return func(lines []string) ([]promptItem, bool) {
		at := -1
		for i := len(lines) - 1; i >= 0; i-- {
			if t, _ := b.text(lines[i]); hint.MatchString(t) {
				at = i
				break
			}
		}
		if at < 0 {
			return nil, false
		}
		var items []promptItem
		blank := 0
		for j := at - 1; j >= 0; j-- {
			t, _ := b.text(lines[j])
			if t == "" {
				if blank++; blank > 1 || len(items) > 0 {
					break
				}
				continue
			}
			m := numberedRow.FindStringSubmatch(t)
			if m == nil {
				if len(items) > 0 && items[0].number == 1 {
					break
				}
				continue // a description under an option
			}
			n, _ := strconv.Atoi(m[1])
			items = append([]promptItem{{number: n, label: m[2], start: j, end: j, indent: indentOf(lines[j])}}, items...)
		}
		if len(items) < 2 || items[0].number != 1 {
			return nil, false
		}
		for k, it := range items {
			if it.number != k+1 {
				return nil, false
			}
		}
		return items, true
	}
}
