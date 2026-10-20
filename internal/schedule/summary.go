package schedule

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/wire"
)

// A run's summary (ScheduleTry.Summary) keeps the end of its output:
// SummaryLines lines at most, wire.ScheduleSummaryMax bytes at most.
const (
	SummaryLines = 50
	// SummaryRead is how much of the end of a run's output the summary is
	// made from.
	SummaryRead = 64 << 10
)

// OutputSummary turns the end of a terminal's raw output into the plain
// text it showed, its last SummaryLines lines within ScheduleSummaryMax
// bytes.
// Escape sequences are dropped, a line redrawn after \r or \b keeps what
// was written last, and an erase-in-line (CSI K) clears. whole says raw
// starts at the beginning of the output; otherwise its first line, which
// may begin inside a sequence or a character, is dropped.
func OutputSummary(raw []byte, whole bool) string {
	if !whole {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			return ""
		}
		raw = raw[i+1:]
	}
	lines := plainLines(strings.ToValidUTF8(string(raw), "\uFFFD"))
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > SummaryLines {
		lines = lines[len(lines)-SummaryLines:]
	}
	s := strings.Join(lines, "\n")
	if len(s) > wire.ScheduleSummaryMax {
		cut := len(s) - wire.ScheduleSummaryMax
		for cut < len(s) && !utf8.RuneStart(s[cut]) {
			cut++
		}
		s = s[cut:]
	}
	return s
}

// plainLines interprets s as a terminal would one line at a time: the
// cursor column moves with \r and \b, characters overwrite, CSI K erases.
func plainLines(s string) []string {
	var lines []string
	var line []rune
	col := 0
	put := func(r rune) {
		if col < len(line) {
			line[col] = r
		} else {
			line = append(line, r)
		}
		col++
	}
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '\n':
			lines = append(lines, strings.TrimRight(string(line), " "))
			line, col = line[:0], 0
		case r == '\r':
			col = 0
		case r == '\b':
			col = max(0, col-1)
		case r == '\t':
			put('\t')
		case r == 0x1b:
			var csi string
			i, csi = skipEscape(s, i)
			switch csi {
			case "K", "0K": // erase to the end of the line
				line = line[:min(col, len(line))]
			case "1K", "2K": // erase to the cursor, the whole line
				for len(line) < col {
					line = append(line, ' ')
				}
				for j := range min(col+1, len(line)) {
					line[j] = ' '
				}
				if csi == "2K" {
					line = line[:min(col, len(line))]
				}
			}
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
		default:
			put(r)
		}
	}
	if len(line) > 0 {
		lines = append(lines, strings.TrimRight(string(line), " "))
	}
	return lines
}

// skipEscape skips the escape sequence whose ESC is right before s[i] and
// returns where it ends and, for a CSI, its parameters and final byte.
func skipEscape(s string, i int) (int, string) {
	if i >= len(s) {
		return i, ""
	}
	switch s[i] {
	case '[': // CSI: parameters and intermediates, then a final byte
		for j := i + 1; j < len(s); j++ {
			if b := s[j]; b >= 0x40 && b <= 0x7e {
				return j + 1, s[i+1 : j+1]
			}
		}
		return len(s), ""
	case ']', 'P', 'X', '^', '_': // OSC, DCS, SOS, PM, APC: up to BEL or ST
		for j := i + 1; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1, ""
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2, ""
			}
		}
		return len(s), ""
	case '(', ')', '*', '+', '-', '.', '/', '#', '%', ' ': // one more byte
		return min(i+2, len(s)), ""
	default:
		return i + 1, ""
	}
}
