package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// The one reader of a limit window (上限の枠) as the sources write it:
// Claude Code's statusLine rate_limits, Codex's rollouts, and both
// private APIs.

// windowFields are the names the sources give a window's values: the
// share of its limit used, the reset (a time, or seconds from when it was
// measured) and its length.
type windowFields struct {
	UsedPercent       json.RawMessage `json:"used_percent"`    // codex
	UsedPercentage    json.RawMessage `json:"used_percentage"` // statusLine, codex
	Utilization       json.RawMessage `json:"utilization"`     // Claude's API, statusLine
	ResetsAt          json.RawMessage `json:"resets_at"`
	ResetAt           json.RawMessage `json:"reset_at"` // Codex's API
	ResetsInSeconds   json.RawMessage `json:"resets_in_seconds"`
	ResetAfterSeconds json.RawMessage `json:"reset_after_seconds"` // Codex's API
	WindowMinutes     json.RawMessage `json:"window_minutes"`
	LimitWindowSecs   json.RawMessage `json:"limit_window_seconds"`
}

// readWindow reads one window measured at at: the share (used_percent,
// else used_percentage, else utilization; a number or a string holding
// one), the reset (resets_at or reset_at: unix seconds, milliseconds when
// that large, or RFC 3339; else resets_in_seconds or reset_after_seconds
// from at) and the length (window_minutes, else limit_window_seconds),
// which names it (wire.UsageWindowOfLength); id names a window without a
// length. ok is false when raw is not an object or lacks the share or the
// reset, or the share is negative.
func readWindow(raw json.RawMessage, id string, at time.Time) (Window, bool) {
	var f windowFields
	if len(raw) == 0 || json.Unmarshal(raw, &f) != nil {
		return Window{}, false
	}
	p, ok := firstNumber(f.UsedPercent, f.UsedPercentage, f.Utilization)
	reset, resetOK := resetTime(f.ResetsAt)
	if !resetOK {
		reset, resetOK = resetTime(f.ResetAt)
	}
	if !resetOK {
		if s, ok := firstNumber(f.ResetsInSeconds, f.ResetAfterSeconds); ok && s >= 0 {
			reset, resetOK = at.UnixMilli()+int64(s*1000), true
		}
	}
	if !ok || !resetOK || p < 0 {
		return Window{}, false
	}
	if m, ok := number(f.WindowMinutes); ok && m > 0 {
		id = wire.UsageWindowOfLength(m)
	} else if s, ok := number(f.LimitWindowSecs); ok && s > 0 {
		id = wire.UsageWindowOfLength(s / 60)
	}
	return Window{ID: id, UsedPercent: p, ResetsAt: reset}, true
}

// readWindows reads the windows ids of top, leaving out the ones
// readWindow cannot read.
func readWindows(top map[string]json.RawMessage, ids []string, at time.Time) []Window {
	var out []Window
	for _, id := range ids {
		if w, ok := readWindow(top[id], id, at); ok {
			out = append(out, w)
		}
	}
	return out
}

// readWindowPair reads Codex's two windows (the rollouts' primary and
// secondary, its API's primary_window and secondary_window): without a
// length the first is five_hour, the second seven_day.
func readWindowPair(primary, secondary json.RawMessage, at time.Time) []Window {
	var out []Window
	for i, raw := range [2]json.RawMessage{primary, secondary} {
		if w, ok := readWindow(raw, [2]string{wire.UsageWindowFiveHour, wire.UsageWindowSevenDay}[i], at); ok {
			out = append(out, w)
		}
	}
	return out
}

// ClaudeRateLimits reads the windows of Claude Code's statusLine
// rate_limits, received at at: five_hour and seven_day (readWindow). A
// window it cannot read is left out, as is anything else (spend_limit).
func ClaudeRateLimits(raw json.RawMessage, at time.Time) []Window {
	var rl map[string]json.RawMessage
	if json.Unmarshal(raw, &rl) != nil {
		return nil
	}
	return readWindows(rl, []string{wire.UsageWindowFiveHour, wire.UsageWindowSevenDay}, at)
}

// number reads a finite JSON number, or a string holding one.
func number(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = []byte(s)
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

// firstNumber is the first of raws that number reads.
func firstNumber(raws ...json.RawMessage) (float64, bool) {
	for _, raw := range raws {
		if f, ok := number(raw); ok {
			return f, true
		}
	}
	return 0, false
}

// resetTime reads a reset time as unix ms: a number of seconds (or of
// milliseconds when it is that large), or an RFC 3339 string.
func resetTime(raw json.RawMessage) (int64, bool) {
	if f, ok := number(raw); ok && f > 0 {
		if f >= 1e12 {
			return int64(f), true
		}
		return int64(f * 1000), true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, false
	}
	return t.UnixMilli(), true
}
