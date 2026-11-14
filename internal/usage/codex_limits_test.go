package usage

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// codex-limits.jsonl is token_count lines with info:null as codex writes
// them between requests: line 0 is codex 0.160.1's shape (used_percent,
// window_minutes, resets_at in seconds, plan_type), lines 1–3 carry no
// window (rate_limits null, empty, both windows null), and lines 4–7 the
// other names codex versions used (used_percentage, limit_window_seconds,
// resets_in_seconds), windows without a length and other lengths.

func codexLimitsAt(minute int) int64 {
	return time.Date(2026, 11, 14, 1, minute, 0, 0, time.UTC).UnixMilli()
}

// Each prefix of codex-limits.jsonl leaves the windows of its last line
// that has any: 300 and 10080 minutes (±1) are the 5-hour and weekly
// windows, another length is named by its minutes, a window without a
// length is the 5-hour one when primary and the weekly one when
// secondary, a reset written as seconds left is the event time plus them,
// and a line with no window leaves the value as it was.
func TestCodexRateLimitsFromRollout(t *testing.T) {
	lines := fixtureLines(t, "codex-limits.jsonl")
	five, week := wire.UsageWindowFiveHour, wire.UsageWindowSevenDay
	tests := []struct {
		lines int
		want  RateLimits
	}{
		{1, RateLimits{At: codexLimitsAt(0), Plan: "plus", Windows: []Window{{five, 12, 1794610000_000}, {week, 3, 1795100000_000}}}},
		// rate_limits null, {}, and both windows null: unchanged.
		{2, RateLimits{At: codexLimitsAt(0), Plan: "plus", Windows: []Window{{five, 12, 1794610000_000}, {week, 3, 1795100000_000}}}},
		{4, RateLimits{At: codexLimitsAt(0), Plan: "plus", Windows: []Window{{five, 12, 1794610000_000}, {week, 3, 1795100000_000}}}},
		{5, RateLimits{At: codexLimitsAt(4), Plan: "pro", Windows: []Window{
			{five, 20.5, codexLimitsAt(4) + 3600_000}, {week, 5, codexLimitsAt(4) + 86400_000}}}},
		{6, RateLimits{At: codexLimitsAt(5), Windows: []Window{{five, 25, 1794700000_000}, {week, 6, 1795200000_000}}}},
		{7, RateLimits{At: codexLimitsAt(6), Windows: []Window{{"1440m", 30, 1794800000_000}, {week, 7, 1795200000_000}}}},
		{8, RateLimits{At: codexLimitsAt(7), Windows: []Window{{five, 26, 1794700000_000}}}},
	}
	for _, tc := range tests {
		tr := newTree(t)
		tr.write(wire.HarnessCodex, codexRel, strings.Join(lines[:tc.lines], ""))
		s := tr.open()
		scan(t, s)
		got, ok := s.RateLimits(wire.HarnessCodex)
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("first %d lines: rate limits = %+v (%v), want %+v", tc.lines, got, ok, tc.want)
		}
	}
}

// Lines appended after a scan with no window keep the value, also when a
// restarted daemon reads them; the real codex.jsonl's info:null line gives
// its windows and the later request lines with null windows keep them.
func TestCodexRateLimitsSurviveNullLines(t *testing.T) {
	lines := fixtureLines(t, "codex-limits.jsonl")
	tr := newTree(t)
	tr.write(wire.HarnessCodex, codexRel, lines[0])
	scan(t, tr.open())
	tr.appendTo(wire.HarnessCodex, codexRel, strings.Join(lines[1:4], ""))
	s := tr.open()
	scan(t, s)
	if got, ok := s.RateLimits(wire.HarnessCodex); !ok || got.At != codexLimitsAt(0) || len(got.Windows) != 2 {
		t.Fatalf("after null lines: %+v (%v)", got, ok)
	}

	tr = newTree(t)
	tr.write(wire.HarnessCodex, codexRel, fixture(t, "codex.jsonl"))
	s = tr.open()
	scan(t, s)
	want := []Window{{wire.UsageWindowFiveHour, 12, 1794610000_000}, {wire.UsageWindowSevenDay, 3, 1795100000_000}}
	if got, ok := s.RateLimits(wire.HarnessCodex); !ok || !reflect.DeepEqual(got.Windows, want) || got.Plan != "plus" {
		t.Fatalf("codex.jsonl: %+v (%v)", got, ok)
	}
}

// Of two rollouts, the value of the newer event wins, whichever file was
// written last; a codex home with no window gives none.
func TestCodexRateLimitsNewestRolloutWins(t *testing.T) {
	lines := fixtureLines(t, "codex-limits.jsonl")
	const other = "sessions/2026/11/14/rollout-2026-11-14T10-00-00-other.jsonl"
	tr := newTree(t)
	s := tr.open()
	scan(t, s)
	if got, ok := s.RateLimits(wire.HarnessCodex); ok {
		t.Fatalf("no rollout: %+v", got)
	}
	// The newer event (01:05) is written first, the older (01:00) last.
	tr.write(wire.HarnessCodex, codexRel, lines[5])
	scan(t, s)
	tr.write(wire.HarnessCodex, other, lines[0])
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(tr.path(wire.HarnessCodex, other), future, future); err != nil {
		t.Fatal(err)
	}
	scan(t, s)
	if got, _ := s.RateLimits(wire.HarnessCodex); got.At != codexLimitsAt(5) {
		t.Fatalf("rate limits from %d, want the newer event %d", got.At, codexLimitsAt(5))
	}
	// A newer event in the other rollout then wins.
	tr.appendTo(wire.HarnessCodex, other, lines[6])
	scan(t, s)
	if got, _ := s.RateLimits(wire.HarnessCodex); got.At != codexLimitsAt(6) {
		t.Fatalf("rate limits from %d, want %d", got.At, codexLimitsAt(6))
	}
}
