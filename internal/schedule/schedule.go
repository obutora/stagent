// Package schedule is what the daemon knows about schedules (定期実行)
// besides running them: whether one is valid, when it runs, and the
// record of its runs.
package schedule

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/schedagent"
	"github.com/obutora/stagent/internal/wire"
)

const nameMax = 200

// Location is the time zone tz names: an IANA name, never "" or "Local"
// (a schedule never runs on the host's own clock).
func Location(tz string) (*time.Location, error) {
	if tz == "" || tz == "Local" {
		return nil, fmt.Errorf("tz %q is not an IANA time zone name", tz)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("tz %q: %v", tz, err)
	}
	return loc, nil
}

// Validate checks s as schedule.create takes it; the directory itself is
// not looked at.
func Validate(s wire.Schedule) error {
	name := strings.TrimSpace(s.Name)
	if name == "" {
		return errors.New("name is empty")
	}
	if utf8.RuneCountInString(name) > nameMax {
		return fmt.Errorf("name is longer than %d characters", nameMax)
	}
	if strings.ContainsAny(s.Name, "\n\r\x00") {
		return errors.New("name has a line break")
	}
	if !filepath.IsAbs(s.Cwd) {
		return fmt.Errorf("cwd %q is not absolute", s.Cwd)
	}
	if err := validateLimits(s); err != nil {
		return err
	}
	if !wire.ValidScheduleNotify(s.Notify) {
		return fmt.Errorf("notify %q is not problems, always or off", s.Notify)
	}
	return ValidateRun(s)
}

// Normalize drops what s's kind does not use, gives an agent schedule its
// permission (read when none), gives the time limit and retry delay their
// defaults and drops the failure count (the daemon's).
func Normalize(s wire.Schedule) wire.Schedule {
	switch s.Kind {
	case wire.ScheduleKindCommand:
		s.Harness, s.Prompt, s.Permission = "", "", ""
	case wire.ScheduleKindAgent:
		s.Command = ""
		s.Permission = schedagent.Permission(s)
	}
	if s.TimeoutMin == nil {
		s.TimeoutMin = new(DefaultTimeoutMin)
	}
	if s.RetryDelayMin == 0 {
		s.RetryDelayMin = DefaultRetryDelayMin
	}
	s.FailedInRow, s.AutoStopped = 0, false
	s.ProblemSent = false
	return s
}

// The time limit, retries and stopping after failures (#1083).
const (
	DefaultTimeoutMin    = 60
	DefaultRetryDelayMin = 5
	timeoutMinMax        = 7 * 24 * 60
	retriesMax           = 3
	retryDelayMinMax     = 60
	stopAfterFailuresMax = 100
)

func validateLimits(s wire.Schedule) error {
	if s.TimeoutMin != nil && (*s.TimeoutMin < 0 || *s.TimeoutMin > timeoutMinMax) {
		return fmt.Errorf("timeout_min %d is not 0 (none) … %d", *s.TimeoutMin, timeoutMinMax)
	}
	if s.Retries < 0 || s.Retries > retriesMax {
		return fmt.Errorf("retries %d is not 0 … %d", s.Retries, retriesMax)
	}
	if s.RetryDelayMin < 0 || s.RetryDelayMin > retryDelayMinMax {
		return fmt.Errorf("retry_delay_min %d is not 1 … %d", s.RetryDelayMin, retryDelayMinMax)
	}
	if s.StopAfterFailures < 0 || s.StopAfterFailures > stopAfterFailuresMax {
		return fmt.Errorf("stop_after_failures %d is not 0 (never) … %d", s.StopAfterFailures, stopAfterFailuresMax)
	}
	return nil
}

// Timeout is s's time limit (0: none).
func Timeout(s wire.Schedule) time.Duration {
	if s.TimeoutMin == nil {
		return DefaultTimeoutMin * time.Minute
	}
	return time.Duration(*s.TimeoutMin) * time.Minute
}

// RetryDelay is how long after a run that failed s waits to run again.
func RetryDelay(s wire.Schedule) time.Duration {
	if s.RetryDelayMin <= 0 {
		return DefaultRetryDelayMin * time.Minute
	}
	return time.Duration(s.RetryDelayMin) * time.Minute
}

// ValidateRun checks what schedule.preview needs: the kind, its command,
// the time zone and the times.
func ValidateRun(s wire.Schedule) error {
	switch s.Kind {
	case wire.ScheduleKindCommand:
		if strings.TrimSpace(s.Command) == "" {
			return errors.New("command is empty")
		}
		if strings.ContainsRune(s.Command, 0) {
			return errors.New("command contains NUL")
		}
	case wire.ScheduleKindAgent:
		if !slices.Contains(wire.ScheduleHarnesses, s.Harness) {
			return fmt.Errorf("harness %q is not %s", s.Harness, wire.ScheduleHarnessNames())
		}
		if strings.TrimSpace(s.Prompt) == "" {
			return errors.New("prompt is empty")
		}
		if strings.ContainsRune(s.Prompt, 0) {
			return errors.New("prompt contains NUL")
		}
		if !schedagent.ValidPermission(s.Permission) {
			return fmt.Errorf("permission %q is not read, write or all", s.Permission)
		}
	default:
		return fmt.Errorf("kind %q is not supported", s.Kind)
	}
	if _, err := Location(s.TZ); err != nil {
		return err
	}
	return ValidateWhen(s.When)
}

// ValidateWhen checks the times.
func ValidateWhen(w wire.ScheduleWhen) error {
	switch w.Every {
	case wire.EveryDaily, wire.EveryWeekdays:
		_, _, err := clock(w.Time)
		return err
	case wire.EveryWeekly:
		if _, _, err := clock(w.Time); err != nil {
			return err
		}
		if len(w.Days) == 0 {
			return errors.New("when.days is empty")
		}
		for _, d := range w.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("when.days: %d is not 0 (Sunday) … 6 (Saturday)", d)
			}
		}
		return nil
	case wire.EveryHours:
		if w.Hours < 1 || w.Hours > 23 {
			return fmt.Errorf("when.hours %d is not 1 … 23", w.Hours)
		}
		if w.Minute < 0 || w.Minute > 59 {
			return fmt.Errorf("when.minute %d is not 0 … 59", w.Minute)
		}
		return nil
	case wire.EveryOnce:
		if _, _, err := clock(w.Time); err != nil {
			return err
		}
		_, err := day(w.Date)
		return err
	case wire.EveryCron:
		_, err := parseCron(w.Cron)
		return err
	}
	return fmt.Errorf("when.every %q is not supported", w.Every)
}

// clock parses "HH:MM".
func clock(s string) (int, int, error) {
	digit := func(i int) (int, bool) { return int(s[i] - '0'), s[i] >= '0' && s[i] <= '9' }
	if len(s) == 5 && s[2] == ':' {
		h1, ok1 := digit(0)
		h2, ok2 := digit(1)
		m1, ok3 := digit(3)
		m2, ok4 := digit(4)
		h, m := h1*10+h2, m1*10+m2
		if ok1 && ok2 && ok3 && ok4 && h <= 23 && m <= 59 {
			return h, m, nil
		}
	}
	return 0, 0, fmt.Errorf("when.time %q is not HH:MM", s)
}

// Next is the first time after after that w names on the wall clock of
// loc; false when there is none (w is not valid). A wall-clock time a
// daylight saving change skips runs at the time it turns into (2:30 →
// 3:30); one it repeats runs once, the first time.
func Next(w wire.ScheduleWhen, loc *time.Location, after time.Time) (time.Time, bool) {
	if ValidateWhen(w) != nil {
		return time.Time{}, false
	}
	if w.Every == wire.EveryOnce {
		return nextOnce(w, loc, after)
	}
	return timesOf(w).next(loc, after)
}

// NextN is the next n times after after.
func NextN(w wire.ScheduleWhen, loc *time.Location, after time.Time, n int) []time.Time {
	var out []time.Time
	for range n {
		t, ok := Next(w, loc, after)
		if !ok {
			break
		}
		out = append(out, t)
		after = t
	}
	return out
}

// day parses "YYYY-MM-DD".
func day(s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil || t.Format(time.DateOnly) != s {
		return time.Time{}, fmt.Errorf("when.date %q is not YYYY-MM-DD", s)
	}
	return t, nil
}

// nextOnce is a once schedule's time when it is after after.
func nextOnce(w wire.ScheduleWhen, loc *time.Location, after time.Time) (time.Time, bool) {
	d, _ := day(w.Date)
	h, m, _ := clock(w.Time)
	t := wallTime(loc, d.Year(), d.Month(), d.Day(), h, m)
	if !t.After(after) {
		return time.Time{}, false
	}
	return t, true
}

// Latest is the newest time of w from at (a time of w not after now) up to
// now. The times missed while a host slept or its daemon was not running
// run once, as this one.
func Latest(w wire.ScheduleWhen, loc *time.Location, at, now time.Time) time.Time {
	latest := at
	// Look back from now over a week first (every time but cron's comes
	// again within one), further only when nothing came in it.
	for span := 8 * 24 * time.Hour; ; span *= 8 {
		from := now.Add(-span)
		if !from.After(latest) {
			from = latest
		}
		t, ok := Next(w, loc, from)
		if ok && !t.After(now) {
			for ok && !t.After(now) {
				latest = t
				t, ok = Next(w, loc, t)
			}
			return latest
		}
		if from.Equal(latest) {
			return latest
		}
	}
}
