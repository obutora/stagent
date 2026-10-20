package schedule

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/wire"
)

// The summary is the text the terminal showed: escape sequences gone,
// \r\n a line break, a line redrawn after \r (a progress bar) as it was
// left, and no blank lines at the end.
func TestOutputSummaryIsPlainText(t *testing.T) {
	raw := "$ make test\r\n" +
		"\x1b[32m✓\x1b[0m cart adds an item\r\n" +
		"\x1b]0;title\x07downloading 10%\rdownloading 100%\r\n" +
		"waiting...\r\x1b[2Kdone\r\n" +
		"abc\b\bXY\r\n" +
		"\x1b[1;31mmake: *** [test] Error 2\x1b[m\r\n\r\n"
	want := "$ make test\n✓ cart adds an item\ndownloading 100%\ndone\naXY\nmake: *** [test] Error 2"
	if got := OutputSummary([]byte(raw), true); got != want {
		t.Errorf("summary\n%q\nwant\n%q", got, want)
	}
}

// Only the last 50 lines are kept.
func TestOutputSummaryKeepsLast50Lines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 60; i++ {
		fmt.Fprintf(&b, "line %d\r\n", i)
	}
	lines := strings.Split(OutputSummary([]byte(b.String()), true), "\n")
	if len(lines) != SummaryLines || lines[0] != "line 11" || lines[len(lines)-1] != "line 60" {
		t.Errorf("%d lines, %q … %q", len(lines), lines[0], lines[len(lines)-1])
	}
}

// Long lines are cut to 8 KiB, keeping the end, on a character boundary.
func TestOutputSummaryAtMost8KiB(t *testing.T) {
	line := strings.Repeat("あ", 1000) // 3000 bytes
	raw := strings.Repeat(line+"\r\n", 5) + "end\r\n"
	got := OutputSummary([]byte(raw), true)
	if len(got) > wire.ScheduleSummaryMax || !utf8.ValidString(got) || !strings.HasSuffix(got, "\nend") {
		t.Errorf("%d bytes (valid %v), ends %q", len(got), utf8.ValidString(got), got[max(0, len(got)-10):])
	}
	if !strings.HasPrefix(got, "あ") {
		t.Errorf("starts %q", got[:9])
	}
}

// Output read from the middle of the stream starts after its first line
// break: the first line may begin inside a sequence or a character.
func TestOutputSummaryFromTheMiddle(t *testing.T) {
	raw := "1m partial\x1b[0m\r\nwhole line\r\n"
	if got := OutputSummary([]byte(raw), false); got != "whole line" {
		t.Errorf("summary %q", got)
	}
}
