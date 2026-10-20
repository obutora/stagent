package schedule

import (
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func mustLoad(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// hostIn makes the host's own clock (time.Local) name, as on a host set to
// another zone than the phone.
func hostIn(t *testing.T, name string) {
	t.Helper()
	old := time.Local
	time.Local = mustLoad(t, name)
	t.Cleanup(func() { time.Local = old })
}

// A 9:00 schedule made on a phone in Asia/Tokyo runs at the phone's 9:00
// on a host whose clock is UTC (0:00 UTC), not at the host's 9:00.
func TestNextUsesScheduleZoneNotHosts(t *testing.T) {
	hostIn(t, "UTC")
	tokyo := mustLoad(t, "Asia/Tokyo")
	after := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC) // 08:59 JST on the 20th
	next, ok := Next(wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}, tokyo, after)
	if !ok {
		t.Fatal("no next time")
	}
	want := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Errorf("next = %v, want %v (09:00 Asia/Tokyo)", next.UTC(), want)
	}
}

func TestNextTimes(t *testing.T) {
	hostIn(t, "America/New_York")
	tokyo := mustLoad(t, "Asia/Tokyo")
	// Monday 2026-10-19 14:30 JST.
	after := time.Date(2026, 10, 19, 14, 30, 0, 0, tokyo)
	jst := func(day, h, m int) time.Time { return time.Date(2026, 10, day, h, m, 0, 0, tokyo) }
	for _, c := range []struct {
		name string
		when wire.ScheduleWhen
		want []time.Time
	}{
		{"daily later today", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "18:00"}, []time.Time{jst(19, 18, 0), jst(20, 18, 0), jst(21, 18, 0)}},
		{"daily passed today", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}, []time.Time{jst(20, 9, 0), jst(21, 9, 0), jst(22, 9, 0)}},
		{"daily at the same minute is next day", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "14:30"}, []time.Time{jst(20, 14, 30), jst(21, 14, 30), jst(22, 14, 30)}},
		// Fri 23rd, then Mon 26th, Tue 27th.
		{"weekdays skip the weekend", wire.ScheduleWhen{Every: wire.EveryWeekdays, Time: "09:00"}, []time.Time{jst(20, 9, 0), jst(21, 9, 0), jst(22, 9, 0)}},
		{"weekly", wire.ScheduleWhen{Every: wire.EveryWeekly, Time: "09:00", Days: []int{0, 3}}, []time.Time{jst(21, 9, 0), jst(25, 9, 0), jst(28, 9, 0)}},
		{"every 6 hours", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 6, Minute: 15}, []time.Time{jst(19, 18, 15), jst(20, 0, 15), jst(20, 6, 15)}},
		{"every 5 hours restarts at midnight", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 5}, []time.Time{jst(19, 15, 0), jst(19, 20, 0), jst(20, 0, 0)}},
	} {
		got := NextN(c.when, tokyo, after, 3)
		if len(got) != len(c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if !got[i].Equal(c.want[i]) {
				t.Errorf("%s: [%d] = %v, want %v", c.name, i, got[i].In(tokyo), c.want[i])
			}
		}
	}
	fri := time.Date(2026, 10, 23, 10, 0, 0, 0, tokyo)
	got := NextN(wire.ScheduleWhen{Every: wire.EveryWeekdays, Time: "09:00"}, tokyo, fri, 2)
	if len(got) != 2 || !got[0].Equal(jst(26, 9, 0)) || !got[1].Equal(jst(27, 9, 0)) {
		t.Errorf("weekdays after Friday 10:00: %v", got)
	}
}

func TestValidate(t *testing.T) {
	ok := wire.Schedule{Name: "n", Kind: wire.ScheduleKindCommand, Command: "date", Cwd: "/tmp", TZ: "Asia/Tokyo",
		When: wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}}
	if err := Validate(ok); err != nil {
		t.Fatalf("valid schedule: %v", err)
	}
	for name, mod := range map[string]func(*wire.Schedule){
		"no name":        func(s *wire.Schedule) { s.Name = " " },
		"agent kind":     func(s *wire.Schedule) { s.Kind = "agent" },
		"no command":     func(s *wire.Schedule) { s.Command = "" },
		"relative cwd":   func(s *wire.Schedule) { s.Cwd = "proj" },
		"no tz":          func(s *wire.Schedule) { s.TZ = "" },
		"Local tz":       func(s *wire.Schedule) { s.TZ = "Local" },
		"unknown tz":     func(s *wire.Schedule) { s.TZ = "Mars/Base" },
		"bad time":       func(s *wire.Schedule) { s.When.Time = "24:00" },
		"unknown every":  func(s *wire.Schedule) { s.When.Every = "monthly" },
		"weekly no days": func(s *wire.Schedule) { s.When = wire.ScheduleWhen{Every: wire.EveryWeekly, Time: "09:00"} },
		"weekly bad day": func(s *wire.Schedule) {
			s.When = wire.ScheduleWhen{Every: wire.EveryWeekly, Time: "09:00", Days: []int{7}}
		},
		"hours 0":   func(s *wire.Schedule) { s.When = wire.ScheduleWhen{Every: wire.EveryHours} },
		"hours 24":  func(s *wire.Schedule) { s.When = wire.ScheduleWhen{Every: wire.EveryHours, Hours: 24} },
		"minute 60": func(s *wire.Schedule) { s.When = wire.ScheduleWhen{Every: wire.EveryHours, Hours: 2, Minute: 60} },
	} {
		s := ok
		mod(&s)
		if err := Validate(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A once schedule's one time is its date and time on its zone's wall
// clock; once that time is not after `after`, there is no next time.
func TestNextOnce(t *testing.T) {
	hostIn(t, "UTC")
	tokyo := mustLoad(t, "Asia/Tokyo")
	w := wire.ScheduleWhen{Every: wire.EveryOnce, Date: "2026-10-21", Time: "09:30"}
	want := time.Date(2026, 10, 21, 0, 30, 0, 0, time.UTC)
	next, ok := Next(w, tokyo, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	if !ok || !next.Equal(want) {
		t.Errorf("next = %v %v, want %v", next.UTC(), ok, want)
	}
	if got := NextN(w, tokyo, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), 3); len(got) != 1 {
		t.Errorf("next 3 = %v, want the one time", got)
	}
	if next, ok := Next(w, tokyo, want); ok {
		t.Errorf("next after its time = %v, want none", next)
	}
	for _, bad := range []wire.ScheduleWhen{
		{Every: wire.EveryOnce, Time: "09:30"},
		{Every: wire.EveryOnce, Date: "2026-13-01", Time: "09:30"},
		{Every: wire.EveryOnce, Date: "2026/10/21", Time: "09:30"},
		{Every: wire.EveryOnce, Date: "2026-10-21"},
	} {
		if ValidateWhen(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := ValidateWhen(w); err != nil {
		t.Errorf("%+v: %v", w, err)
	}
}

// Latest is the newest of the times that came: of three hourly times, the
// third; of a weekly time missed for a year, last week's.
func TestLatest(t *testing.T) {
	utc := time.UTC
	hourly := wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1}
	at := time.Date(2026, 10, 20, 7, 0, 0, 0, utc)
	if got := Latest(hourly, utc, at, time.Date(2026, 10, 20, 9, 52, 0, 0, utc)); !got.Equal(time.Date(2026, 10, 20, 9, 0, 0, 0, utc)) {
		t.Errorf("hourly: %v", got)
	}
	if got := Latest(hourly, utc, at, at.Add(time.Minute)); !got.Equal(at) {
		t.Errorf("one time: %v", got)
	}
	weekly := wire.ScheduleWhen{Every: wire.EveryWeekly, Days: []int{1}, Time: "03:00"}
	at = time.Date(2025, 10, 20, 3, 0, 0, 0, utc) // a Monday
	if got := Latest(weekly, utc, at, time.Date(2026, 10, 21, 12, 0, 0, 0, utc)); !got.Equal(time.Date(2026, 10, 19, 3, 0, 0, 0, utc)) {
		t.Errorf("weekly: %v", got)
	}
}
