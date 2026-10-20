//go:build !windows

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// signalHolder answers session.signal for a run's session as its holder
// would, recording each signal; the session ends (holder.ended through h)
// when end returns true for it, with that exit code.
func signalHolder(t *testing.T, e *scheduleEnv, sp spawned, h *rpc.Client, end func(sig string) (int, bool)) chan string {
	t.Helper()
	signals := make(chan string, 8)
	ln, err := ipc.Listen(e.layout.HolderAddr(sp.flag("id")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go rpc.Serve(context.Background(), c, func(ctx context.Context, _ *rpc.Conn, m *wire.Msg) (any, error) {
				var p wire.SignalParams
				if m.Method != wire.MethodSessionSignal || rpc.Decode(m, &p) != nil || p.ID != sp.flag("id") {
					return nil, wire.Errorf(wire.ErrBadRequest, "unexpected %s", m.Method)
				}
				signals <- p.Signal
				if code, ok := end(p.Signal); ok {
					h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: p.ID, ExitCode: code})
				}
				return struct{}{}, nil
			})
		}
	}()
	return signals
}

func awaitSignal(t *testing.T, signals chan string, want string) {
	t.Helper()
	select {
	case sig := <-signals:
		if sig != want {
			t.Fatalf("signal %s, want %s", sig, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s", want)
	}
}

func noSignal(t *testing.T, signals chan string, d time.Duration) {
	t.Helper()
	select {
	case sig := <-signals:
		t.Fatalf("signalled %s early", sig)
	case <-time.After(d):
	}
}

// ends ends a spawned run's session with exit code code.
func (e *scheduleEnv) ends(sp spawned, code int) {
	e.t.Helper()
	e.holderOf(sp).Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: code})
	e.awaitRun(sp.flag("schedule"), func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") && r.Result != wire.RunRunning })
}

// A run still running at its schedule's time limit (15 minutes) is sent
// terminate; one that ignores it, kill 10 seconds later. It is recorded as
// timed out.
func TestScheduleTimeoutStopsRun(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "trap '' TERM; sleep 3600", dailyNine())
	s.TimeoutMin = new(15)
	id := e.create(s).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning })
	signals := signalHolder(t, e, sp, h, func(sig string) (int, bool) { return 137, sig == wire.SignalKill })

	e.clock.Set(tokyoNine.Add(14 * time.Minute))
	noSignal(t, signals, 100*time.Millisecond)
	e.clock.Set(tokyoNine.Add(15*time.Minute + 2*time.Second))
	awaitSignal(t, signals, wire.SignalTerminate)
	e.clock.Set(tokyoNine.Add(15*time.Minute + 8*time.Second))
	noSignal(t, signals, 100*time.Millisecond)
	e.clock.Set(tokyoNine.Add(15*time.Minute + 13*time.Second))
	awaitSignal(t, signals, wire.SignalKill)

	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunTimedOut || run.ExitCode == nil || *run.ExitCode != 137 || !strings.Contains(run.Error, "15m") {
		t.Errorf("run %+v", run)
	}
	e.noSpawn(100 * time.Millisecond) // no retries by default
}

// The time limit is 60 minutes unless the schedule says otherwise; 0 is
// none.
func TestScheduleTimeoutDefaultAndNone(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	cwd := t.TempDir()
	en := e.create(commandSchedule(cwd, "sleep 7200", dailyNine()))
	if en.Schedule.TimeoutMin == nil || *en.Schedule.TimeoutMin != 60 || en.Schedule.Retries != 0 ||
		en.Schedule.RetryDelayMin != 5 || en.Schedule.StopAfterFailures != 0 {
		t.Errorf("defaults %+v", en.Schedule)
	}
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	signals := signalHolder(t, e, sp, h, func(string) (int, bool) { return 143, true })
	e.clock.Set(tokyoNine.Add(59 * time.Minute))
	noSignal(t, signals, 100*time.Millisecond)
	e.clock.Set(tokyoNine.Add(60*time.Minute + time.Second))
	awaitSignal(t, signals, wire.SignalTerminate)
	if r := e.awaitRun(en.Schedule.ID, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning }); r.Result != wire.RunTimedOut {
		t.Errorf("run %+v", r)
	}

	none := commandSchedule(cwd, "sleep 7200", dailyNine())
	none.TimeoutMin = new(0)
	en = e.create(none)
	if en.Schedule.TimeoutMin == nil || *en.Schedule.TimeoutMin != 0 {
		t.Errorf("no limit %+v", en.Schedule.TimeoutMin)
	}
	sp, h = e.startRunning(tokyoNine.Add(24*time.Hour + time.Second))
	if sp.flag("schedule") != en.Schedule.ID { // the first schedule runs too
		e.ends(sp, 0)
		sp = e.awaitSpawn()
		h = e.holderOf(sp)
	}
	signals = signalHolder(t, e, sp, h, func(string) (int, bool) { return 143, true })
	e.clock.Set(tokyoNine.Add(24*time.Hour + 10*time.Hour))
	noSignal(t, signals, 100*time.Millisecond)

	for name, bad := range map[string]func(*wire.Schedule){
		"timeout":     func(s *wire.Schedule) { s.TimeoutMin = new(-1) },
		"retries":     func(s *wire.Schedule) { s.Retries = 4 },
		"retry delay": func(s *wire.Schedule) { s.Retries, s.RetryDelayMin = 1, 61 },
		"stop after":  func(s *wire.Schedule) { s.StopAfterFailures = -1 },
	} {
		s := commandSchedule(cwd, "true", dailyNine())
		bad(&s)
		if err := e.c.Call(t.Context(), wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: s}, nil); errCode(err) != wire.ErrBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A schedule with 2 retries runs a failing command 3 times: the retries
// come after the retry delay and go on in the line of their time (one run,
// three tries: retry 1/2 and 2/2 after the first), each in its own
// session.
func TestScheduleRetries(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 2, 1
	id := e.create(s).Schedule.ID

	first, _ := e.startRunning(tokyoNine.Add(time.Second))
	e.ends(first, 1)
	e.noSpawn(100 * time.Millisecond)
	if en, _ := e.entry(id); en.RetryAt != tokyoNine.Add(time.Second+time.Minute).UnixMilli() || en.LastRun == nil || en.LastRun.Result != wire.RunFailed {
		t.Errorf("waiting to retry %+v", en)
	}
	e.clock.Set(tokyoNine.Add(time.Minute + 2*time.Second))
	second := e.awaitSpawn()
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning }); r.Retry != 1 || len(r.Tries) != 1 ||
		r.SessionID != second.flag("id") {
		t.Errorf("retry 1 running %+v", r)
	}
	e.ends(second, 1)
	e.clock.Set(tokyoNine.Add(2*time.Minute + 3*time.Second))
	third := e.awaitSpawn()
	e.ends(third, 1)
	e.clock.Set(tokyoNine.Add(time.Hour))
	e.noSpawn(100 * time.Millisecond)

	runs := e.runs(id)
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	r := runs[0]
	if r.RunID != first.flag("run") || second.flag("run") != r.RunID || third.flag("run") != r.RunID ||
		r.Trigger != wire.RunTriggerScheduled || r.ScheduledAt != tokyoNine.UnixMilli() || r.Retries != 2 {
		t.Errorf("run %+v", r)
	}
	tries := append(r.Tries, r.ScheduleTry)
	if len(tries) != 3 {
		t.Fatalf("tries %+v", tries)
	}
	for i, sp := range []spawned{first, second, third} {
		if tr := tries[i]; tr.Retry != i || tr.SessionID != sp.flag("id") || tr.Result != wire.RunFailed || tr.ExitCode == nil || *tr.ExitCode != 1 {
			t.Errorf("try %d %+v", i, tr)
		}
	}
	if en, _ := e.entry(id); en.RetryAt != 0 || en.Schedule.FailedInRow != 1 {
		t.Errorf("after the last retry %+v", en)
	}
}

// A retry waiting when the daemon stops waits again when it starts (from
// the run's line); a time whose retry was dropped (paused) does not.
func TestScheduleRetryWaitsOverRestart(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 2, 1
	id := e.create(s).Schedule.ID
	first, _ := e.startRunning(tokyoNine.Add(time.Second))
	e.ends(first, 1)

	e.clock.Set(tokyoNine.Add(30 * time.Second))
	e = e.restart()
	if en, _ := e.entry(id); en.RetryAt != tokyoNine.Add(time.Second+time.Minute).UnixMilli() {
		t.Errorf("retry_at after the restart %v", time.UnixMilli(en.RetryAt).UTC())
	}
	e.noSpawn(100 * time.Millisecond)
	e.clock.Set(tokyoNine.Add(time.Minute + 2*time.Second))
	retry := e.awaitSpawn()
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.SessionID == retry.flag("id") }); r.RunID != first.flag("run") || r.Retry != 1 {
		t.Errorf("retry after the restart %+v", r)
	}
	e.ends(retry, 1)

	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, nil)
	call(t, e.c, wire.MethodScheduleResume, wire.ScheduleRef{ID: id}, nil)
	e = e.restart()
	if en, _ := e.entry(id); en.RetryAt != 0 {
		t.Errorf("the dropped retry waits again: %+v", en)
	}
	e.clock.Set(tokyoNine.Add(10 * time.Minute))
	e.noSpawn(100 * time.Millisecond)
}

// A retry waiting when the schedule's next time comes is dropped: the
// next time runs. run_now drops it too.
func TestScheduleRetryDroppedAtNextTime(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "exit 1", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1})
	s.Retries, s.RetryDelayMin = 3, 15
	id := e.create(s).Schedule.ID

	first, _ := e.startRunning(tokyoNine.Add(time.Second))
	e.ends(first, 1)
	e.clock.Set(tokyoNine.Add(time.Hour + time.Second)) // past the retry (9:15) and the next time (10:00)
	next := e.awaitSpawn()
	r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == next.flag("run") })
	if r.Trigger != wire.RunTriggerScheduled || r.ScheduledAt != tokyoNine.Add(time.Hour).UnixMilli() {
		t.Errorf("run at the next time %+v", r)
	}
	e.ends(next, 1)
	e.noSpawn(100 * time.Millisecond)
	if en, _ := e.entry(id); en.RetryAt == 0 || en.Schedule.FailedInRow != 1 {
		t.Errorf("waiting to retry the 10:00 run: %+v", en)
	}

	var now wire.ScheduleRun
	call(t, e.c, wire.MethodScheduleRunNow, wire.ScheduleRef{ID: id}, &now)
	sp := e.awaitSpawn()
	if now.Trigger != wire.RunTriggerNow || sp.flag("run") != now.RunID {
		t.Errorf("run_now %+v", now)
	}
	if en, _ := e.entry(id); en.RetryAt != 0 || en.Schedule.FailedInRow != 2 {
		t.Errorf("run_now kept the retry: %+v", en)
	}
	// run_now's run is retried as well.
	e.ends(sp, 1)
	e.clock.Set(tokyoNine.Add(time.Hour + 16*time.Minute))
	retry := e.awaitSpawn()
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.SessionID == retry.flag("id") }); r.Trigger != wire.RunTriggerNow ||
		r.Retry != 1 || r.RunID != now.RunID || retry.flag("run") != now.RunID || len(r.Tries) != 1 {
		t.Errorf("retry of run_now %+v", r)
	}
}

// A timed-out run is retried; a run whose agent was refused something is
// not.
func TestScheduleRetriesTimedOutOnly(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "sleep 3600", dailyNine())
	s.TimeoutMin, s.Retries, s.RetryDelayMin = new(15), 1, 1
	id := e.create(s).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	signalHolder(t, e, sp, h, func(string) (int, bool) { return 143, true })
	e.clock.Set(tokyoNine.Add(15*time.Minute + 2*time.Second))
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunTimedOut })
	e.clock.Set(tokyoNine.Add(16*time.Minute + 3*time.Second))
	retry := e.awaitSpawn()
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.SessionID == retry.flag("id") }); r.Retry != 1 ||
		r.RunID != sp.flag("run") || len(r.Tries) != 1 || r.Tries[0].Result != wire.RunTimedOut {
		t.Errorf("retry %+v", r)
	}
}

// A schedule set to stop after 3 failures in a row stops (paused,
// auto_stopped) at the third time that failed, retries included; a
// success in between starts the count again, resuming clears it.
func TestScheduleStopsAfterFailures(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	w := e.watcher()
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin, s.StopAfterFailures = 1, 1, 3
	id := e.create(s).Schedule.ID

	day := func(n int, code int) {
		t.Helper()
		at := tokyoNine.Add(time.Duration(n) * 24 * time.Hour)
		sp, _ := e.startRunning(at.Add(time.Second))
		e.ends(sp, code)
		if code == 0 {
			return
		}
		e.clock.Set(at.Add(time.Minute + 2*time.Second))
		e.ends(e.awaitSpawn(), code)
	}
	day(0, 1)
	day(1, 0)
	day(2, 1)
	day(3, 1)
	if en, _ := e.entry(id); en.Schedule.State != wire.ScheduleActive || en.Schedule.FailedInRow != 2 || en.Schedule.AutoStopped {
		t.Fatalf("after two failures %+v", en.Schedule)
	}
	day(4, 1)
	en, _ := e.entry(id)
	if en.Schedule.State != wire.SchedulePaused || !en.Schedule.AutoStopped || en.Schedule.FailedInRow != 3 || en.NextAt != 0 {
		t.Fatalf("after three failures %+v", en)
	}
	changed(t, w, id, "auto-stopped", func(c wire.ScheduleChanged) bool { return c.Entry != nil && c.Entry.Schedule.AutoStopped })
	e.clock.Set(tokyoNine.Add(5*24*time.Hour + time.Second))
	e.noSpawn(100 * time.Millisecond)

	var resumed wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleResume, wire.ScheduleRef{ID: id}, &resumed)
	if resumed.Schedule.State != wire.ScheduleActive || resumed.Schedule.AutoStopped || resumed.Schedule.FailedInRow != 0 {
		t.Errorf("resumed %+v", resumed.Schedule)
	}
	// An update keeps the count; it is the daemon's.
	upd := commandSchedule(s.Cwd, "exit 1", dailyNine())
	upd.StopAfterFailures, upd.FailedInRow, upd.AutoStopped = 3, 9, true
	var updated wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: upd}, &updated)
	if updated.Schedule.FailedInRow != 0 || updated.Schedule.AutoStopped {
		t.Errorf("updated %+v", updated.Schedule)
	}
}

// The run whose failure stopped the schedule says so in its record, with
// how many failed in a row then, also when it is a run whose retry the
// next time dropped.
func TestScheduleAutoStopIsRecorded(t *testing.T) {
	t.Run("last try", func(t *testing.T) {
		e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
		s := commandSchedule(t.TempDir(), "exit 1", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1})
		s.StopAfterFailures = 2
		id := e.create(s).Schedule.ID
		first, _ := e.startRunning(tokyoNine.Add(time.Second))
		e.ends(first, 1)
		second, _ := e.startRunning(tokyoNine.Add(time.Hour + time.Second))
		e.ends(second, 1)
		runs := e.runs(id)
		if len(runs) != 2 || runs[0].AutoStopped != 2 || runs[1].AutoStopped != 0 {
			t.Errorf("runs %+v", runs)
		}
	})
	t.Run("dropped retry", func(t *testing.T) {
		e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
		s := commandSchedule(t.TempDir(), "exit 1", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1})
		s.StopAfterFailures, s.Retries, s.RetryDelayMin = 1, 1, 15
		id := e.create(s).Schedule.ID
		first, _ := e.startRunning(tokyoNine.Add(time.Second))
		e.ends(first, 1)
		e.clock.Set(tokyoNine.Add(time.Hour + time.Second)) // past the retry (9:15) and the next time (10:00)
		r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.AutoStopped > 0 })
		if r.RunID != first.flag("run") || r.AutoStopped != 1 || len(e.runs(id)) != 1 {
			t.Errorf("runs %+v", e.runs(id))
		}
		e.noSpawn(100 * time.Millisecond)
	})
}
