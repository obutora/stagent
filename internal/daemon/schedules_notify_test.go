//go:build !windows

package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/wire"
)

// startScheduleNotifyDaemon is startScheduleDaemon pushing to a fake ntfy
// server (pushEnv: host label "box", English).
func startScheduleNotifyDaemon(t *testing.T, at time.Time, mutate func(*wire.Config)) (*scheduleEnv, <-chan ntfyReq) {
	t.Helper()
	clock := &fakeClock{t: at}
	spawns := make(chan spawned, 8)
	e, reqs := pushEnv(t, mutate, Options{
		Now:          clock.Now,
		ScheduleTick: 10 * time.Millisecond,
		LoginEnv:     func() ([]string, error) { return nil, nil },
		SpawnHolder: func(exe string, args []string, dir string, env []string, logPath string) (int, bool, error) {
			spawns <- spawned{exe, args, dir, env}
			return 0, false, nil
		},
	})
	c, _ := e.client()
	return &scheduleEnv{testEnv: e, clock: clock, spawns: spawns, c: c}, reqs
}

// runDay lets day n's 9:00 come and ends its run with code.
func (e *scheduleEnv) runDay(n, code int) spawned {
	e.t.Helper()
	sp, _ := e.startRunning(tokyoNine.Add(time.Duration(n)*24*time.Hour + time.Second))
	e.ends(sp, code)
	return sp
}

const quiet = 200 * time.Millisecond

// Under problems (the default), three failures in a row push only the
// first; the success after them pushes fixed. Both carry the schedule's
// sequence ID, so the second replaces the first. The run's session pushes
// no exited.
func TestScheduleNotifyProblemsOnlyFirstThenFixed(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	id := e.create(commandSchedule(t.TempDir(), "exit 2", dailyNine())).Schedule.ID
	seq := notify.SequenceID("schedule_" + id)

	e.runDay(0, 2)
	r := wantPush(t, reqs, "first failure", seq, "Failed (exit code 2)")
	if r.title != "box · nightly" {
		t.Errorf("title %q", r.title)
	}
	noReq(t, reqs, "the run's exited", quiet)
	e.runDay(1, 2)
	e.runDay(2, 2)
	noReq(t, reqs, "failures going on", quiet)
	e.runDay(3, 0)
	wantPush(t, reqs, "fixed", seq, "Succeeded again: fixed")
	e.runDay(4, 0)
	noReq(t, reqs, "a success after a success", quiet)
}

// Always pushes successes too; off pushes nothing; notify.reasons.schedule
// off pushes no schedule's.
func TestScheduleNotifyAlwaysOffAndReasonOff(t *testing.T) {
	t.Run("always", func(t *testing.T) {
		e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
		s := commandSchedule(t.TempDir(), "true", dailyNine())
		s.Notify = wire.ScheduleNotifyAlways
		id := e.create(s).Schedule.ID
		e.runDay(0, 0)
		wantPush(t, reqs, "success", notify.SequenceID("schedule_"+id), "Succeeded")
		e.runDay(1, 0)
		wantPush(t, reqs, "success again", notify.SequenceID("schedule_"+id), "Succeeded")
	})
	t.Run("off", func(t *testing.T) {
		e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
		s := commandSchedule(t.TempDir(), "exit 2", dailyNine())
		s.Notify = wire.ScheduleNotifyOff
		e.create(s)
		e.runDay(0, 2)
		noReq(t, reqs, "notify off", quiet)
	})
	t.Run("reason off", func(t *testing.T) {
		e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), func(c *wire.Config) {
			c.Notify.Reasons.Schedule = new(false)
		})
		s := commandSchedule(t.TempDir(), "exit 2", dailyNine())
		s.Notify = wire.ScheduleNotifyAlways
		e.create(s)
		e.runDay(0, 2)
		noReq(t, reqs, "notify.reasons.schedule off", quiet)
	})
}

// With 2 retries, the failures before the last are not pushed; the last
// result of the time is, once.
func TestScheduleNotifyLastRetryOnly(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 2, 1
	id := e.create(s).Schedule.ID
	e.runDay(0, 1)
	for i := 1; i <= 2; i++ {
		noReq(t, reqs, "a failure before a retry", quiet)
		e.clock.Set(tokyoNine.Add(time.Duration(i)*(time.Minute+2*time.Second) + time.Second))
		e.ends(e.awaitSpawn(), 1)
	}
	wantPush(t, reqs, "the last failure", notify.SequenceID("schedule_"+id), "Failed (exit code 1)")
	noReq(t, reqs, "after the last", quiet)
}

// Stopping after failures is pushed even while failures go on.
func TestScheduleNotifyAutoStopped(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.StopAfterFailures = 2
	id := e.create(s).Schedule.ID
	seq := notify.SequenceID("schedule_" + id)
	e.runDay(0, 1)
	wantPush(t, reqs, "first failure", seq, "Failed (exit code 1)")
	e.runDay(1, 1)
	wantPush(t, reqs, "stopped", seq, "Stopped after 2 failures in a row")
}

// With the app in the foreground, the run's end is an in-app notification
// event (schedule and run, no session) and nothing is pushed.
func TestScheduleNotifyForegroundInAppOnly(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	app, events := e.client()
	call(t, app, wire.MethodWatch, wire.WatchParams{}, nil)
	call(t, app, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	id := e.create(commandSchedule(t.TempDir(), "exit 2", dailyNine())).Schedule.ID
	sp := e.runDay(0, 2)
	var got wire.NotificationData
	await(t, events, "schedule notification", func(m *wire.Msg) bool {
		var ev wire.Event
		if m.Method != wire.NotifyEvent || json.Unmarshal(m.Params, &ev) != nil || ev.Kind != wire.EventNotification {
			return false
		}
		var n wire.NotificationData
		if json.Unmarshal(ev.Data, &n) != nil || n.Reason != reasonSchedule {
			return false
		}
		got = n
		return true
	})
	if got.ScheduleID != id || got.RunID != sp.flag("run") || got.SessionID != "" || got.Title != "nightly" ||
		got.Body != "Failed (exit code 2)" || got.Level != wire.LevelWarn {
		t.Errorf("event %+v", got)
	}
	noReq(t, reqs, "app in the foreground", quiet)
}

// A run stopped because its schedule is removed (stop_run) is not a
// failure: nothing is notified, the failures in a row do not go up (no
// stop after failures) and it is not retried.
func TestScheduleRemoveStopRunIsNoFailure(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	w := e.watcher()
	s := commandSchedule(t.TempDir(), "sleep 600", dailyNine())
	s.Notify, s.Retries, s.RetryDelayMin, s.StopAfterFailures = wire.ScheduleNotifyAlways, 1, 1, 1
	id := e.create(s).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning })
	fakeRunHolder(t, e, sp, h)

	call(t, e.c, wire.MethodScheduleRemove, wire.ScheduleRemoveParams{ID: id, StopRun: true}, nil)
	changed(t, w, id, "removed", func(c wire.ScheduleChanged) bool {
		if c.Entry != nil && (c.Entry.Schedule.FailedInRow > 0 || c.Entry.Schedule.AutoStopped || c.Entry.RetryAt > 0) {
			t.Errorf("the stopped run counted: %+v", c.Entry)
		}
		return c.Entry == nil
	})
	noReq(t, reqs, "the run stopped by remove", quiet)
	e.clock.Set(tokyoNine.Add(2 * time.Minute))
	e.noSpawn(100 * time.Millisecond)
}

// A run that fails after its schedule was paused is not retried while it
// is paused: that failure is its time's last result, notified and
// counted.
func TestSchedulePausedRunIsNotRetried(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 2, 1
	id := e.create(s).Schedule.ID
	sp, _ := e.startRunning(tokyoNine.Add(time.Second))
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, nil)
	e.ends(sp, 1)
	wantPush(t, reqs, "the failure while paused", notify.SequenceID("schedule_"+id), "Failed (exit code 1)")
	if en, _ := e.entry(id); en.RetryAt != 0 || en.Schedule.FailedInRow != 1 {
		t.Errorf("after the failure while paused %+v", en)
	}
	e.clock.Set(tokyoNine.Add(5 * time.Minute))
	e.noSpawn(100 * time.Millisecond)
}

// Pausing a schedule waiting to retry drops the retry: the failure is its
// time's last result, notified and counted.
func TestSchedulePauseDropsTheRetry(t *testing.T) {
	e, reqs := startScheduleNotifyDaemon(t, tokyoNine.Add(-time.Minute), nil)
	s := commandSchedule(t.TempDir(), "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 2, 1
	id := e.create(s).Schedule.ID
	e.runDay(0, 1)
	noReq(t, reqs, "a failure before a retry", quiet)
	var paused wire.ScheduleEntry
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, &paused)
	if paused.RetryAt != 0 || paused.Schedule.FailedInRow != 1 {
		t.Errorf("paused %+v", paused)
	}
	wantPush(t, reqs, "the failure whose retry was dropped", notify.SequenceID("schedule_"+id), "Failed (exit code 1)")
	e.clock.Set(tokyoNine.Add(5 * time.Minute))
	e.noSpawn(100 * time.Millisecond)
}

func TestRunProblemAndBody(t *testing.T) {
	code := 3
	for _, c := range []struct {
		r       wire.ScheduleTry
		fixed   bool
		problem bool
		body    string
	}{
		{wire.ScheduleTry{Result: wire.RunSucceeded}, false, false, "成功しました"},
		{wire.ScheduleTry{Result: wire.RunSucceeded}, true, false, "直りました（成功しました）"},
		{wire.ScheduleTry{Result: wire.RunFailed, ExitCode: &code}, false, true, "失敗しました（終了コード 3）"},
		{wire.ScheduleTry{Result: wire.RunFailed}, false, true, "失敗しました"},
		{wire.ScheduleTry{Result: wire.RunTimedOut, ExitCode: &code}, false, true, "時間切れで止めました"},
		{wire.ScheduleTry{Result: wire.RunDenied, Denials: 3}, false, true, "操作を 3 件断られました"},
		{wire.ScheduleTry{Result: wire.RunUndetermined}, false, true, "結果を判定できませんでした"},
		{wire.ScheduleTry{Result: wire.RunSucceeded, Unjudged: true}, false, true, "結果を判定できませんでした"},
		{wire.ScheduleTry{Result: wire.RunSkippedOverlap}, false, true, "前の実行が終わっていないので飛ばしました"},
		{wire.ScheduleTry{Result: wire.RunFailed, ExitCode: &code, OutsideGUI: true, AuthFailed: true}, false, true,
			"GUI にログインしていないので claude が認証できませんでした"},
		{wire.ScheduleTry{Result: wire.RunFailed, ExitCode: &code, AuthFailed: true}, false, true, "失敗しました（終了コード 3）"},
	} {
		r := wire.ScheduleRun{ScheduleTry: c.r}
		if got := runProblem(r); got != c.problem {
			t.Errorf("runProblem(%+v) = %v", c.r, got)
		}
		if got := runBody(wire.LangJa, "claude", r, c.fixed); got != c.body {
			t.Errorf("runBody(%+v, %v) = %q, want %q", c.r, c.fixed, got, c.body)
		}
	}
	for _, r := range []string{wire.RunSkippedPaused, wire.RunRunning} {
		if runProblem(wire.ScheduleRun{ScheduleTry: wire.ScheduleTry{Result: r}}) {
			t.Errorf("%s is a problem", r)
		}
	}
}
