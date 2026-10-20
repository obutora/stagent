package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// times is when a repeating schedule runs as the values of a cron
// expression's fields allow them (a bit per value): every rule but once is
// one, cron the one written out.
type times struct {
	minute, hour, dom, month, dow uint64
	// domAny and dowAny: the field starts with "*". When neither does, a
	// day runs when its day of the month or its weekday is in the set
	// (cron's rule); otherwise when both are.
	domAny, dowAny bool
}

// cronDays is how many days on Next looks for a time: a 29 February comes
// at most 8 years after another.
const cronDays = 8*366 + 1

type cronField struct {
	name     string
	min, max int
	names    []string // the values from min on by name (jan, sun)
}

var cronFields = [5]cronField{
	{name: "minute", max: 59},
	{name: "hour", max: 23},
	{name: "day of month", min: 1, max: 31},
	{name: "month", min: 1, max: 12, names: []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	{name: "weekday", max: 7, names: []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// parseCron reads a five-field cron expression: each field a list of
// "*", n or n-m, each optionally "/step" (n/step is n to the field's
// last value); months and weekdays may be named (jan, mon); weekday 7 is
// Sunday as 0 is.
func parseCron(expr string) (times, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return times{}, fmt.Errorf("when.cron %q is not 5 fields (minute hour day-of-month month weekday)", expr)
	}
	var sets [5]uint64
	for i, f := range fields {
		set, err := cronFields[i].parse(f)
		if err != nil {
			return times{}, fmt.Errorf("when.cron %q: %w", expr, err)
		}
		sets[i] = set
	}
	c := times{minute: sets[0], hour: sets[1], dom: sets[2], month: sets[3], dow: sets[4],
		domAny: fields[2][0] == '*', dowAny: fields[4][0] == '*'}
	if c.dow&(1<<7) != 0 {
		c.dow = c.dow&^(1<<7) | 1
	}
	if !c.domAny && c.dowAny && !c.someMonthHasDay() {
		return times{}, fmt.Errorf("when.cron %q never comes: none of its months has that day", expr)
	}
	return c, nil
}

func (f cronField) parse(s string) (uint64, error) {
	var set uint64
	for item := range strings.SplitSeq(s, ",") {
		base, stepText, stepped := strings.Cut(item, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 || !digits(stepText) {
				return 0, fmt.Errorf("%s step %q is not 1 or more", f.name, stepText)
			}
			step = n
		}
		lo, hi := f.min, f.max
		if base != "*" {
			from, to, ranged := strings.Cut(base, "-")
			var err error
			if lo, err = f.value(from); err != nil {
				return 0, err
			}
			switch {
			case ranged:
				if hi, err = f.value(to); err != nil {
					return 0, err
				}
				if hi < lo {
					return 0, fmt.Errorf("%s range %s is backwards", f.name, base)
				}
			case !stepped:
				hi = lo
			}
		}
		for v := lo; v <= hi; v += step {
			set |= 1 << v
		}
	}
	return set, nil
}

func (f cronField) value(s string) (int, error) {
	for i, name := range f.names {
		if strings.EqualFold(s, name) {
			return f.min + i, nil
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || !digits(s) {
		return 0, fmt.Errorf("%s %q is not a number", f.name, s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%s %d is not %d … %d", f.name, n, f.min, f.max)
	}
	return n, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// someMonthHasDay: one of c's months has one of its days of the month (29
// February counts).
func (c times) someMonthHasDay() bool {
	for mo := 1; mo <= 12; mo++ {
		if c.month&(1<<mo) == 0 {
			continue
		}
		last := time.Date(2028, time.Month(mo)+1, 0, 0, 0, 0, 0, time.UTC).Day() // 2028 is a leap year
		if c.dom&(1<<(last+1)-1) != 0 {
			return true
		}
	}
	return false
}

// timesOf is w (not once) as times.
func timesOf(w wire.ScheduleWhen) times {
	if w.Every == wire.EveryCron {
		c, _ := parseCron(w.Cron)
		return c
	}
	all := times{dom: 1<<32 - 2, month: 1<<13 - 2, dow: 1<<7 - 1, domAny: true, dowAny: true}
	if w.Every == wire.EveryHours {
		for h := 0; h < 24; h += w.Hours {
			all.hour |= 1 << h
		}
		all.minute = 1 << w.Minute
		return all
	}
	h, m, _ := clock(w.Time)
	all.hour, all.minute = 1<<h, 1<<m
	switch w.Every {
	case wire.EveryWeekdays:
		all.dow = 1<<6 - 2
	case wire.EveryWeekly:
		all.dow = 0
		for _, d := range w.Days {
			all.dow |= 1 << d
		}
	}
	return all
}

// onDay: c runs on the day of day.
func (c times) onDay(day time.Time) bool {
	if c.month&(1<<int(day.Month())) == 0 {
		return false
	}
	dom := c.dom&(1<<day.Day()) != 0
	dow := c.dow&(1<<int(day.Weekday())) != 0
	if c.domAny || c.dowAny {
		return dom && dow
	}
	return dom || dow
}

// next is c's first time after after on the wall clock of loc.
func (c times) next(loc *time.Location, after time.Time) (time.Time, bool) {
	y, mo, d := after.In(loc).Date()
	for i := range cronDays {
		if !c.onDay(time.Date(y, mo, d+i, 12, 0, 0, 0, loc)) {
			continue
		}
		if t, ok := c.nextOnDay(loc, y, mo, d+i, after); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// nextOnDay is c's first time after after on day y-mo-d. On a day the
// clock changes, a time it skipped comes later than its wall clock says,
// so every time of the day is looked at.
func (c times) nextOnDay(loc *time.Location, y int, mo time.Month, d int, after time.Time) (time.Time, bool) {
	start := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	_, zoneEnd := start.ZoneBounds()
	changes := !zoneEnd.IsZero() && zoneEnd.Before(time.Date(y, mo, d+1, 0, 0, 0, 0, loc))
	fromH, fromM := 0, 0
	if l := after.In(loc); !changes && l.Year() == start.Year() && l.YearDay() == start.YearDay() {
		fromH, fromM = l.Hour(), l.Minute()
	}
	var best time.Time
	for h := fromH; h < 24; h++ {
		if c.hour&(1<<h) == 0 {
			continue
		}
		m := 0
		if h == fromH {
			m = fromM
		}
		for ; m < 60; m++ {
			if c.minute&(1<<m) == 0 {
				continue
			}
			t := wallTime(loc, y, mo, d, h, m)
			if !t.After(after) {
				continue
			}
			if !changes {
				return t, true
			}
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
	}
	return best, !best.IsZero()
}

// wallTime is h:m on y-mo-d on the wall clock of loc. A time a daylight
// saving change skips is the time it turns into (2:30 → 3:30); one it
// repeats is the first (1:30 EDT, not EST).
func wallTime(loc *time.Location, y int, mo time.Month, d, h, m int) time.Time {
	t := time.Date(y, mo, d, h, m, 0, 0, loc)
	if t.Hour() == h && t.Minute() == m {
		return t
	}
	// time.Date put the skipped time before the change; move it on by
	// as much as the clock skipped.
	ty, tmo, td := t.Date()
	skipped := time.Date(y, mo, d, h, m, 0, 0, time.UTC).Sub(time.Date(ty, tmo, td, t.Hour(), t.Minute(), 0, 0, time.UTC))
	if skipped > 0 {
		t = t.Add(skipped)
	}
	return t
}
