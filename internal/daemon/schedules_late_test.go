//go:build !windows

package daemon

import (
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// restart stops the daemon and starts another on the same home and clock,
// with the same login environment and holders (recorded in e.spawns), as a
// host that was off or a daemon that was stopped comes back at the clock's
// time.
func (e *scheduleEnv) restart() *scheduleEnv {
	e.t.Helper()
	e.stop()
	o := e.d.opts
	te := startDaemonAt(e.t, fastConfig(), Options{
		Now: o.Now, ScheduleTick: o.ScheduleTick, LoginEnv: o.LoginEnv, SpawnHolder: o.SpawnHolder,
	})
	c, _ := te.client()
	return &scheduleEnv{testEnv: te, clock: e.clock, spawns: e.spawns, c: c}
}

// endRun ends the run of a spawned holder with exit code 0 and waits for
// its record.
func (e *scheduleEnv) endRun(id string, sp spawned) wire.ScheduleRun {
	e.t.Helper()
	e.holderOf(sp).Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	return e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") && r.Result != wire.RunRunning })
}

// onceAt is once at t on the wall clock of Asia/Tokyo (commandSchedule's).
func onceAt(t time.Time) wire.ScheduleWhen {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	t = t.In(tokyo)
	return wire.ScheduleWhen{Every: wire.EveryOnce, Date: t.Format("2006-01-02"), Time: t.Format("15:04")}
}

// A once schedule five minutes on runs at its time, once; it then has no
// next time and stays.
func TestScheduleOnceRunsOnce(t *testing.T) {
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	s := commandSchedule(t.TempDir(), "true", onceAt(start.Add(5*time.Minute)))
	entry := e.create(s)
	id := entry.Schedule.ID
	if entry.NextAt != start.Add(5*time.Minute).UnixMilli() {
		t.Fatalf("next_at %v", time.UnixMilli(entry.NextAt).UTC())
	}
	e.noSpawn(50 * time.Millisecond)

	e.clock.Set(start.Add(5*time.Minute + 10*time.Second))
	sp := e.awaitSpawn()
	run := e.endRun(id, sp)
	if run.Trigger != wire.RunTriggerScheduled || run.ScheduledAt != start.Add(5*time.Minute).UnixMilli() || run.Result != wire.RunSucceeded {
		t.Errorf("run %+v", run)
	}
	en, ok := e.entry(id)
	if !ok || en.NextAt != 0 || en.Schedule.State != wire.ScheduleActive {
		t.Errorf("after its run: %+v (there %v)", en, ok)
	}

	e.clock.Set(start.Add(48 * time.Hour))
	e.noSpawn(100 * time.Millisecond)
	if n := len(e.runs(id)); n != 1 {
		t.Errorf("%d runs, want 1", n)
	}
	// A daemon started again does not run it again either.
	e = e.restart()
	e.noSpawn(100 * time.Millisecond)
	if en, _ := e.entry(id); en.NextAt != 0 {
		t.Errorf("after restart: next_at %v", time.UnixMilli(en.NextAt).UTC())
	}
}

// Three hourly times missed while the daemon was stopped run once when it
// starts again, late, as the newest of them; a daemon started again after
// that does not run the same time twice.
func TestScheduleMissedTimesRunOnceLate(t *testing.T) {
	start := time.Date(2026, 10, 20, 5, 30, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	s := commandSchedule(t.TempDir(), "true", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1})
	s.TZ = "UTC"
	id := e.create(s).Schedule.ID
	e.clock.Set(time.Date(2026, 10, 20, 6, 0, 5, 0, time.UTC))
	e.endRun(id, e.awaitSpawn())

	e.clock.Set(time.Date(2026, 10, 20, 6, 30, 0, 0, time.UTC))
	e.stop()
	// 7:00, 8:00 and 9:00 pass while it is stopped.
	e.clock.Set(time.Date(2026, 10, 20, 9, 52, 0, 0, time.UTC))
	e = e.restart()
	sp := e.awaitSpawn()
	e.noSpawn(100 * time.Millisecond)
	run := e.endRun(id, sp)
	nine := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if run.Trigger != wire.RunTriggerLate || run.ScheduledAt != nine.UnixMilli() ||
		run.StartedAt != time.Date(2026, 10, 20, 9, 52, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("late run %+v", run)
	}
	if n := len(e.runs(id)); n != 2 {
		t.Errorf("%d runs, want 2 (the 6:00 one and the late one)", n)
	}
	if en, _ := e.entry(id); en.NextAt != nine.Add(time.Hour).UnixMilli() {
		t.Errorf("next_at %v, want 10:00", time.UnixMilli(en.NextAt).UTC())
	}

	e.clock.Set(time.Date(2026, 10, 20, 9, 55, 0, 0, time.UTC))
	e = e.restart()
	e.noSpawn(100 * time.Millisecond)
	if n := len(e.runs(id)); n != 2 {
		t.Errorf("%d runs after another restart, want 2", n)
	}
	e.clock.Set(time.Date(2026, 10, 20, 10, 0, 1, 0, time.UTC))
	if r := e.endRun(id, e.awaitSpawn()); r.Trigger != wire.RunTriggerScheduled {
		t.Errorf("10:00 run %+v", r)
	}
}

// The wall clock jumping past a time (the host slept) runs it on the next
// comparison, late.
func TestScheduleRunsLateAfterSleep(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "true", dailyNine())).Schedule.ID
	e.clock.Set(tokyoNine.Add(52 * time.Minute))
	run := e.endRun(id, e.awaitSpawn())
	if run.Trigger != wire.RunTriggerLate || run.ScheduledAt != tokyoNine.UnixMilli() {
		t.Errorf("run %+v", run)
	}
}

// A once schedule whose time passes while it is paused does not run, and
// is recorded so; resumed, it has no next time. A daemon started again does
// not record it twice.
func TestSchedulePausedOncePasses(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "true", onceAt(tokyoNine))).Schedule.ID
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, nil)

	e.clock.Set(tokyoNine.Add(time.Minute))
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return true })
	if run.Result != wire.RunSkippedPaused || run.ScheduledAt != tokyoNine.UnixMilli() || run.SessionID != "" || run.EndedAt == 0 {
		t.Errorf("run %+v", run)
	}
	e.noSpawn(100 * time.Millisecond)

	e = e.restart()
	e.noSpawn(100 * time.Millisecond)
	var en wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleResume, wire.ScheduleRef{ID: id}, &en)
	if en.NextAt != 0 || en.Schedule.State != wire.ScheduleActive {
		t.Errorf("resumed: %+v", en)
	}
	e.noSpawn(100 * time.Millisecond)
	if n := len(e.runs(id)); n != 1 {
		t.Errorf("%d runs, want 1", n)
	}
}

// A once schedule whose time passed while the daemon was stopped runs
// late when it starts again.
func TestScheduleMissedOnceRunsLate(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "true", onceAt(tokyoNine))).Schedule.ID
	e.stop()
	e.clock.Set(tokyoNine.Add(3 * time.Hour))
	e = e.restart()
	run := e.endRun(id, e.awaitSpawn())
	if run.Trigger != wire.RunTriggerLate || run.ScheduledAt != tokyoNine.UnixMilli() {
		t.Errorf("run %+v", run)
	}
	if en, _ := e.entry(id); en.NextAt != 0 {
		t.Errorf("next_at %v", time.UnixMilli(en.NextAt).UTC())
	}
}
