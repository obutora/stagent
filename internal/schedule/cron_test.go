package schedule

import (
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func cronWhen(expr string) wire.ScheduleWhen {
	return wire.ScheduleWhen{Every: wire.EveryCron, Cron: expr}
}

func wantTimes(t *testing.T, name string, got, want []time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if !got[i].Equal(want[i]) {
			t.Errorf("%s: [%d] = %v, want %v", name, i, got[i], want[i])
		}
	}
}

// Cron expressions name times the choices cannot: the 1st of every month,
// a quarter past every hour, every 15 minutes.
func TestNextCron(t *testing.T) {
	hostIn(t, "UTC")
	tokyo := mustLoad(t, "Asia/Tokyo")
	// Tuesday 2026-10-20 14:07 JST.
	after := time.Date(2026, 10, 20, 14, 7, 0, 0, tokyo)
	at := func(y int, mo time.Month, d, h, m int) time.Time { return time.Date(y, mo, d, h, m, 0, 0, tokyo) }
	for _, c := range []struct {
		expr string
		want []time.Time
	}{
		{"0 3 1 * *", []time.Time{at(2026, 11, 1, 3, 0), at(2026, 12, 1, 3, 0), at(2027, 1, 1, 3, 0)}},
		{"*/15 * * * *", []time.Time{at(2026, 10, 20, 14, 15), at(2026, 10, 20, 14, 30), at(2026, 10, 20, 14, 45)}},
		{"15 * * * *", []time.Time{at(2026, 10, 20, 14, 15), at(2026, 10, 20, 15, 15), at(2026, 10, 20, 16, 15)}},
		{"* * * * *", []time.Time{at(2026, 10, 20, 14, 8), at(2026, 10, 20, 14, 9), at(2026, 10, 20, 14, 10)}},
		{"0 9 * * mon-fri", []time.Time{at(2026, 10, 21, 9, 0), at(2026, 10, 22, 9, 0), at(2026, 10, 23, 9, 0)}},
		{"0 9,18 * * 0,7", []time.Time{at(2026, 10, 25, 9, 0), at(2026, 10, 25, 18, 0), at(2026, 11, 1, 9, 0)}},
		{"30 23 * DEC *", []time.Time{at(2026, 12, 1, 23, 30), at(2026, 12, 2, 23, 30), at(2026, 12, 3, 23, 30)}},
		{"0 0 29 2 *", []time.Time{at(2028, 2, 29, 0, 0), at(2032, 2, 29, 0, 0), at(2036, 2, 29, 0, 0)}},
		{"10-40/15 8 * * *", []time.Time{at(2026, 10, 21, 8, 10), at(2026, 10, 21, 8, 25), at(2026, 10, 21, 8, 40)}},
		// Both the day of the month and the weekday restricted: either
		// one (the 1st, or a Friday).
		{"0 6 1 * 5", []time.Time{at(2026, 10, 23, 6, 0), at(2026, 10, 30, 6, 0), at(2026, 11, 1, 6, 0)}},
	} {
		w := cronWhen(c.expr)
		if err := ValidateWhen(w); err != nil {
			t.Errorf("%s: %v", c.expr, err)
			continue
		}
		wantTimes(t, c.expr, NextN(w, tokyo, after, 3), c.want)
	}
}

func TestValidateCron(t *testing.T) {
	for expr, want := range map[string]string{
		"61 * * * *":    "minute 61 is not 0 … 59",
		"* 24 * * *":    "hour 24 is not 0 … 23",
		"0 0 0 * *":     "day of month 0 is not 1 … 31",
		"0 0 * 13 *":    "month 13 is not 1 … 12",
		"0 0 * * 8":     "weekday 8 is not 0 … 7",
		"* * * *":       "5 fields",
		"* * * * * *":   "5 fields",
		"":              "5 fields",
		"*/0 * * * *":   "step",
		"5-1 * * * *":   "range",
		"x * * * *":     "minute",
		"0 0 31 2 *":    "never",
		"0 0 30 2,4 *":  "",
		"@daily":        "5 fields",
		"1,,2 * * * *":  "minute",
		"0 0 * * sun-x": "weekday",
	} {
		err := ValidateWhen(cronWhen(expr))
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", expr, err)
		case want != "" && err == nil:
			t.Errorf("%q accepted", expr)
		case want != "" && !strings.Contains(err.Error(), want):
			t.Errorf("%q: %v, want it to say %q", expr, err, want)
		}
	}
}

// On America/New_York's daylight saving days a cron schedule runs each
// wall-clock time once: one the change skips at the time it turns into
// (2:30 → 3:30 EDT), one it repeats the first time (1:30 EDT).
func TestNextCronDaylightSaving(t *testing.T) {
	hostIn(t, "UTC")
	ny := mustLoad(t, "America/New_York")
	utc := func(mo time.Month, d, h, m int) time.Time { return time.Date(2026, mo, d, h, m, 0, 0, time.UTC) }

	// 2026-03-08 2:00 EST becomes 3:00 EDT (7:00 UTC).
	spring := utc(3, 8, 4, 0) // 23:00 EST on the 7th
	wantTimes(t, "30 2 spring", NextN(cronWhen("30 2 * * *"), ny, spring, 2),
		[]time.Time{utc(3, 8, 7, 30), utc(3, 9, 6, 30)}) // 3:30 EDT, then 2:30 EDT
	wantTimes(t, "*/30 spring", NextN(cronWhen("*/30 * * * *"), ny, utc(3, 8, 5, 45), 4),
		[]time.Time{utc(3, 8, 6, 0), utc(3, 8, 6, 30), utc(3, 8, 7, 0), utc(3, 8, 7, 30)}) // 1:00 1:30 EST, 3:00 3:30 EDT
	// From inside the hour the change made: 3:10 EDT, next 3:30.
	wantTimes(t, "*/30 from 3:10 EDT", NextN(cronWhen("*/30 * * * *"), ny, utc(3, 8, 7, 10), 2),
		[]time.Time{utc(3, 8, 7, 30), utc(3, 8, 8, 0)})

	// 2026-11-01 2:00 EDT becomes 1:00 EST (6:00 UTC).
	fall := utc(11, 1, 3, 0) // 23:00 EDT on 31 Oct
	wantTimes(t, "30 1 fall", NextN(cronWhen("30 1 * * *"), ny, fall, 2),
		[]time.Time{utc(11, 1, 5, 30), utc(11, 2, 6, 30)}) // 1:30 EDT once, then 1:30 EST
	wantTimes(t, "*/30 fall", NextN(cronWhen("*/30 * * * *"), ny, utc(11, 1, 4, 15), 4),
		[]time.Time{utc(11, 1, 4, 30), utc(11, 1, 5, 0), utc(11, 1, 5, 30), utc(11, 1, 7, 0)}) // 0:30 1:00 1:30 EDT, 2:00 EST

	// The choices follow the same rule: daily 2:30 runs at 3:30 EDT.
	wantTimes(t, "daily 02:30 spring", NextN(wire.ScheduleWhen{Every: wire.EveryDaily, Time: "02:30"}, ny, spring, 1),
		[]time.Time{utc(3, 8, 7, 30)})
}
