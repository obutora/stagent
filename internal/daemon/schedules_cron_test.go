//go:build !windows

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// hostClockIn sets the host's own clock (time.Local) to zone name.
func hostClockIn(t *testing.T, name string) {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

// A cron schedule "0 3 1 * *" in Asia/Tokyo runs at 3:00 on the 1st of each
// month there; on a host whose clock is UTC its entry says so (the host's
// zone and both offsets at the next time).
func TestScheduleCronRunsAtItsTime(t *testing.T) {
	hostClockIn(t, "UTC")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	start := time.Date(2026, 11, 1, 2, 59, 0, 0, tokyo)
	e := startScheduleDaemon(t, start, nil, nil)

	entry := e.create(commandSchedule(t.TempDir(), "true", wire.ScheduleWhen{Every: wire.EveryCron, Cron: "0 3 1 * *"}))
	three := time.Date(2026, 11, 1, 3, 0, 0, 0, tokyo)
	if entry.NextAt != three.UnixMilli() {
		t.Fatalf("next_at %v, want %v", time.UnixMilli(entry.NextAt).In(tokyo), three)
	}
	if hc := entry.HostClock; hc == nil || hc.TZ != "UTC" || hc.At != three.UnixMilli() || hc.Offset != 0 || hc.TZOffset != 9*3600 {
		t.Errorf("host_clock %+v", hc)
	}
	id := entry.Schedule.ID
	e.noSpawn(50 * time.Millisecond)

	e.clock.Set(three.Add(5 * time.Second))
	e.awaitSpawn()
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return true })
	if run.ScheduledAt != three.UnixMilli() || run.Trigger != wire.RunTriggerScheduled {
		t.Errorf("run %+v", run)
	}
	var sl wire.ScheduleListResult
	call(t, e.c, wire.MethodScheduleList, nil, &sl)
	if dec := time.Date(2026, 12, 1, 3, 0, 0, 0, tokyo); len(sl.Schedules) != 1 || sl.Schedules[0].NextAt != dec.UnixMilli() {
		t.Errorf("list %+v, want next_at %v", sl.Schedules, dec)
	}
}

// schedule.preview answers a cron expression's next three times, its
// mistake as bad_request, and the host's clock only where it differs.
func TestSchedulePreviewCron(t *testing.T) {
	hostClockIn(t, "Asia/Tokyo")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	e := startScheduleDaemon(t, time.Date(2026, 10, 20, 14, 7, 0, 0, tokyo), nil, nil)
	cwd := t.TempDir()
	var p wire.SchedulePreview
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "true",
		wire.ScheduleWhen{Every: wire.EveryCron, Cron: "*/15 * * * *"})}, &p)
	want := []int64{
		time.Date(2026, 10, 20, 14, 15, 0, 0, tokyo).UnixMilli(),
		time.Date(2026, 10, 20, 14, 30, 0, 0, tokyo).UnixMilli(),
		time.Date(2026, 10, 20, 14, 45, 0, 0, tokyo).UnixMilli(),
	}
	if len(p.Next) != 3 || p.Next[0] != want[0] || p.Next[1] != want[1] || p.Next[2] != want[2] {
		t.Errorf("next %v, want %v", p.Next, want)
	}
	if p.HostClock != nil {
		t.Errorf("host_clock %+v on a host on the schedule's zone", p.HostClock)
	}

	err := e.c.Call(t.Context(), wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "true",
		wire.ScheduleWhen{Every: wire.EveryCron, Cron: "61 * * * *"})}, nil)
	if errCode(err) != wire.ErrBadRequest || !strings.Contains(err.Error(), "minute 61 is not 0 … 59") {
		t.Errorf("preview of 61 * * * *: %v", err)
	}

	s := commandSchedule(cwd, "true", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})
	s.TZ = "America/New_York"
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: s}, &p)
	if hc := p.HostClock; hc == nil || hc.TZ != "Asia/Tokyo" || hc.At != p.Next[0] || hc.Offset != 9*3600 || hc.TZOffset != -4*3600 {
		t.Errorf("host_clock %+v, next %v", hc, p.Next)
	}
}
