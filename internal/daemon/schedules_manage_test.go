//go:build !windows

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

func (e *scheduleEnv) entry(id string) (wire.ScheduleEntry, bool) {
	e.t.Helper()
	var sl wire.ScheduleListResult
	call(e.t, e.c, wire.MethodScheduleList, nil, &sl)
	for _, en := range sl.Schedules {
		if en.Schedule.ID == id {
			return en, true
		}
	}
	return wire.ScheduleEntry{}, false
}

// session is session id as sessions.list has it.
func (e *scheduleEnv) session(id string) (wire.Session, bool) {
	e.t.Helper()
	var r wire.SessionsListResult
	call(e.t, e.c, wire.MethodSessionsList, nil, &r)
	for _, s := range r.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return wire.Session{}, false
}

// watcher is a connection watching the daemon (as a bridge does).
func (e *scheduleEnv) watcher() chan *wire.Msg {
	e.t.Helper()
	c, ch := e.client()
	call(e.t, c, wire.MethodWatch, wire.WatchParams{}, nil)
	return ch
}

// changed waits for schedule.changed of id satisfying pred.
func changed(t *testing.T, ch chan *wire.Msg, id string, what string, pred func(wire.ScheduleChanged) bool) wire.ScheduleChanged {
	t.Helper()
	var got wire.ScheduleChanged
	await(t, ch, what, func(m *wire.Msg) bool {
		var c wire.ScheduleChanged
		if m.Method != wire.NotifyScheduleChanged || json.Unmarshal(m.Params, &c) != nil || c.ID != id || !pred(c) {
			return false
		}
		got = c
		return true
	})
	return got
}

// startRunning lets schedule id's time come (now) and registers the run's
// holder, which stays running.
func (e *scheduleEnv) startRunning(at time.Time) (spawned, *rpc.Client) {
	e.t.Helper()
	e.clock.Set(at)
	sp := e.awaitSpawn()
	return sp, e.holderOf(sp)
}

var tokyoNine = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC) // 09:00 in Tokyo

func dailyNine() wire.ScheduleWhen { return wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"} }

// Every change of a schedule reaches the watchers as schedule.changed:
// creating, pausing, resuming and updating it carry its entry, removing
// it none.
func TestScheduleChangedReachesWatchers(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	w := e.watcher()
	cwd := t.TempDir()
	id := e.create(commandSchedule(cwd, "true", dailyNine())).Schedule.ID
	changed(t, w, id, "created", func(c wire.ScheduleChanged) bool { return c.Entry != nil && c.Entry.NextAt == tokyoNine.UnixMilli() })

	var en wire.ScheduleEntry
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, &en)
	if en.Schedule.State != wire.SchedulePaused || en.NextAt != 0 {
		t.Errorf("paused entry %+v", en)
	}
	changed(t, w, id, "paused", func(c wire.ScheduleChanged) bool {
		return c.Entry != nil && c.Entry.Schedule.State == wire.SchedulePaused && c.Entry.NextAt == 0
	})
	call(t, e.c, wire.MethodScheduleResume, wire.ScheduleRef{ID: id}, &en)
	if en.Schedule.State != wire.ScheduleActive || en.NextAt != tokyoNine.UnixMilli() {
		t.Errorf("resumed entry %+v", en)
	}
	changed(t, w, id, "resumed", func(c wire.ScheduleChanged) bool {
		return c.Entry != nil && c.Entry.Schedule.State == wire.ScheduleActive
	})

	s := commandSchedule(cwd, "make test", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "10:00"})
	s.Name = "renamed"
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: s}, &en)
	if en.Schedule.ID != id || en.Schedule.Name != "renamed" || en.Schedule.Command != "make test" || en.NextAt != tokyoNine.Add(time.Hour).UnixMilli() {
		t.Errorf("updated entry %+v", en)
	}
	changed(t, w, id, "updated", func(c wire.ScheduleChanged) bool { return c.Entry != nil && c.Entry.Schedule.Name == "renamed" })

	call(t, e.c, wire.MethodScheduleRemove, wire.ScheduleRemoveParams{ID: id}, nil)
	changed(t, w, id, "removed", func(c wire.ScheduleChanged) bool { return c.Entry == nil })
	if _, ok := e.entry(id); ok {
		t.Error("removed schedule still listed")
	}
	for _, m := range []string{wire.MethodSchedulePause, wire.MethodScheduleResume, wire.MethodScheduleRunNow} {
		if err := e.c.Call(t.Context(), m, wire.ScheduleRef{ID: id}, nil); errCode(err) != wire.ErrNotFound {
			t.Errorf("%s of a removed schedule: %v", m, err)
		}
	}
}

// Two updates: the later one is what the schedule is. An update keeps the
// schedule's id, state, created_at and the members this version does not
// know, and refuses what create refuses.
func TestScheduleUpdateLastWriteWins(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	cwd := t.TempDir()
	created := e.create(commandSchedule(cwd, "true", dailyNine()))
	id := created.Schedule.ID
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, nil)
	for _, cmd := range []string{"echo one", "echo two"} {
		s := commandSchedule(cwd, cmd, dailyNine())
		s.ID, s.State, s.CreatedAt = "ffffffffffffffff", wire.ScheduleActive, 7
		s.Extra = map[string]json.RawMessage{"future": json.RawMessage(`"always"`)}
		call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: s}, nil)
	}
	en, _ := e.entry(id)
	if en.Schedule.Command != "echo two" || en.Schedule.ID != id || en.Schedule.State != wire.SchedulePaused ||
		en.Schedule.CreatedAt != created.Schedule.CreatedAt || string(en.Schedule.Extra["future"]) != `"always"` {
		t.Errorf("after two updates %+v", en.Schedule)
	}
	for name, p := range map[string]wire.ScheduleUpdateParams{
		"missing cwd": {ID: id, Schedule: commandSchedule(filepath.Join(cwd, "gone"), "true", dailyNine())},
		"bad time":    {ID: id, Schedule: commandSchedule(cwd, "true", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "25:00"})},
	} {
		if err := e.c.Call(t.Context(), wire.MethodScheduleUpdate, p, nil); errCode(err) != wire.ErrBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := e.c.Call(t.Context(), wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: "0123456789abcdef", Schedule: commandSchedule(cwd, "true", dailyNine())}, nil)
	if errCode(err) != wire.ErrNotFound {
		t.Errorf("update of an unknown schedule: %v", err)
	}
}

// A paused schedule's time passes without a run, and is not made up for
// on resume: the schedule runs from its next time on. run_now works while
// it is paused.
func TestSchedulePausedTimesPass(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Hour), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "true", dailyNine())).Schedule.ID
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: id}, nil)

	e.clock.Set(tokyoNine.Add(time.Minute))
	e.noSpawn(100 * time.Millisecond)
	if runs := e.runs(id); len(runs) != 0 {
		t.Errorf("runs while paused: %+v", runs)
	}

	var run wire.ScheduleRun
	call(t, e.c, wire.MethodScheduleRunNow, wire.ScheduleRef{ID: id}, &run)
	sp := e.awaitSpawn()
	if run.Trigger != wire.RunTriggerNow || run.Result != wire.RunRunning || run.RunID != sp.flag("run") || run.SessionID != sp.flag("id") {
		t.Errorf("run_now while paused: %+v (holder %v)", run, sp.args)
	}
	e.holderOf(sp).Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunSucceeded })
	if en, _ := e.entry(id); en.Schedule.State != wire.SchedulePaused {
		t.Errorf("run_now resumed the schedule: %+v", en.Schedule)
	}

	e.clock.Set(tokyoNine.Add(2 * time.Hour))
	var en wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleResume, wire.ScheduleRef{ID: id}, &en)
	if en.NextAt != tokyoNine.Add(24*time.Hour).UnixMilli() {
		t.Errorf("resumed: next_at %v", time.UnixMilli(en.NextAt).UTC())
	}
	e.noSpawn(100 * time.Millisecond)
	e.clock.Set(tokyoNine.Add(24*time.Hour + time.Second))
	sp = e.awaitSpawn()
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") }); r.ScheduledAt != tokyoNine.Add(24*time.Hour).UnixMilli() {
		t.Errorf("run after resume %+v", r)
	}
}

// A time that comes while the previous run is still running is skipped
// and recorded so; run_now is refused (busy) until the run ends.
func TestScheduleOverlapSkips(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	s := commandSchedule(t.TempDir(), "sleep 90", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 1})
	s.TimeoutMin = new(0) // the hour that passes is not its time limit
	id := e.create(s).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	first := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning })

	err := e.c.Call(t.Context(), wire.MethodScheduleRunNow, wire.ScheduleRef{ID: id}, nil)
	if errCode(err) != wire.ErrBusy {
		t.Errorf("run_now while running: %v", err)
	}

	e.clock.Set(tokyoNine.Add(time.Hour + time.Second))
	skipped := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunSkippedOverlap })
	if skipped.ScheduledAt != tokyoNine.Add(time.Hour).UnixMilli() || skipped.Trigger != wire.RunTriggerScheduled ||
		skipped.SessionID != "" || skipped.EndedAt == 0 {
		t.Errorf("skipped run %+v", skipped)
	}
	e.noSpawn(100 * time.Millisecond)
	runs := e.runs(id)
	if len(runs) != 2 || runs[1].RunID != first.RunID || runs[1].Result != wire.RunRunning {
		t.Errorf("runs %+v", runs)
	}

	if en, _ := e.entry(id); en.LastRun == nil || en.LastRun.RunID != first.RunID {
		t.Errorf("last_run while running %+v", en.LastRun)
	}
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if runs := e.runs(id); runs[1].Result == wire.RunSucceeded {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("first run did not end: %+v", runs)
		}
	}
	call(t, e.c, wire.MethodScheduleRunNow, wire.ScheduleRef{ID: id}, nil)
	e.awaitSpawn()
}

// An update while a run is running leaves the run as it is (its record
// keeps the directory it started in); the next run runs the new command
// in the new directory.
func TestScheduleUpdateWhileRunning(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), []string{"SHELL=/bin/sh"}, nil)
	cwd, moved := t.TempDir(), t.TempDir()
	id := e.create(commandSchedule(cwd, "echo old", dailyNine())).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: commandSchedule(moved, "echo new", dailyNine())}, nil)
	if s, ok := e.session(sp.flag("id")); !ok || s.State == wire.StateExited {
		t.Fatalf("the run's session on update: %+v", s)
	}
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") && r.Result == wire.RunSucceeded })
	if got := sp.command(); !slices.Equal(got, []string{"/bin/sh", "-c", "echo old"}) || sp.flag("cwd") != cwd || r.Cwd != cwd {
		t.Errorf("running run's command %v in %s, recorded in %q", got, sp.flag("cwd"), r.Cwd)
	}
	e.clock.Set(tokyoNine.Add(24*time.Hour + time.Second))
	next := e.awaitSpawn()
	if got := next.command(); !slices.Equal(got, []string{"/bin/sh", "-c", "echo new"}) || next.flag("cwd") != moved {
		t.Errorf("next run's command %v in %s", got, next.flag("cwd"))
	}
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == next.flag("run") }); r.Cwd != moved {
		t.Errorf("next run recorded in %q", r.Cwd)
	}
}

// A retry after an update runs in the directory its time started in, the
// one its record keeps.
func TestScheduleRetryRunsInItsRunsDirectory(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	cwd, moved := t.TempDir(), t.TempDir()
	s := commandSchedule(cwd, "exit 1", dailyNine())
	s.Retries, s.RetryDelayMin = 1, 1
	id := e.create(s).Schedule.ID
	first, _ := e.startRunning(tokyoNine.Add(time.Second))
	e.ends(first, 1)
	upd := commandSchedule(moved, "exit 1", dailyNine())
	upd.Retries, upd.RetryDelayMin = 1, 1
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: upd}, nil)
	e.clock.Set(tokyoNine.Add(time.Minute + 2*time.Second))
	if retry := e.awaitSpawn(); retry.flag("cwd") != cwd {
		t.Errorf("retry in %s, want %s", retry.flag("cwd"), cwd)
	}
}

// A time that came before an update but was not compared yet (the next
// comparison is up to ScheduleTick away) runs as the schedule was, not
// dropped; the times after the update run the new schedule.
func TestScheduleUpdateRunsATimeThatCame(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh")
	clock := &fakeClock{t: tokyoNine.Add(-time.Minute)}
	spawns := make(chan spawned, 8)
	te := startDaemon(t, fastConfig(), Options{
		Now:          clock.Now,
		ScheduleTick: time.Hour, // no comparison but the first
		LoginEnv:     func() ([]string, error) { return []string{"SHELL=/bin/sh"}, nil },
		SpawnHolder: func(exe string, args []string, dir string, env []string, logPath string) (int, bool, error) {
			spawns <- spawned{exe, args, dir, env}
			return 0, false, nil
		},
	})
	c, _ := te.client()
	e := &scheduleEnv{testEnv: te, clock: clock, spawns: spawns, c: c}
	cwd := t.TempDir()
	id := e.create(commandSchedule(cwd, "echo old", dailyNine())).Schedule.ID
	e.clock.Set(tokyoNine.Add(10 * time.Second))
	var updated wire.ScheduleEntry
	call(t, e.c, wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: id, Schedule: commandSchedule(cwd, "echo new", dailyNine())}, &updated)
	sp := e.awaitSpawn()
	if got := sp.command(); !slices.Equal(got, []string{"/bin/sh", "-c", "echo old"}) {
		t.Errorf("the time that came ran %v", got)
	}
	if r := e.awaitRun(id, func(r wire.ScheduleRun) bool { return true }); r.ScheduledAt != tokyoNine.UnixMilli() ||
		r.Trigger != wire.RunTriggerScheduled || r.RunID != sp.flag("run") {
		t.Errorf("run %+v", r)
	}
	if updated.NextAt != tokyoNine.Add(24*time.Hour).UnixMilli() {
		t.Errorf("next_at %v", time.UnixMilli(updated.NextAt).UTC())
	}
}

// fakeRunHolder answers session.signal for a run's session as its holder
// would: the session ends (holder.ended through h).
func fakeRunHolder(t *testing.T, e *scheduleEnv, sp spawned, h *rpc.Client) chan string {
	t.Helper()
	signals := make(chan string, 4)
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
				h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: p.ID, ExitCode: 129})
				return struct{}{}, nil
			})
		}
	}()
	return signals
}

// Removing a schedule with stop_run while its run is running stops the
// run's session first; the schedule and its runs are gone.
func TestScheduleRemoveStopsRun(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "sleep 600", dailyNine())).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning })
	signals := fakeRunHolder(t, e, sp, h)

	call(t, e.c, wire.MethodScheduleRemove, wire.ScheduleRemoveParams{ID: id, StopRun: true}, nil)
	select {
	case sig := <-signals:
		if sig != wire.SignalHangup {
			t.Errorf("signal %s, want hangup", sig)
		}
	default:
		t.Fatal("the run's session was not stopped")
	}
	if s, ok := e.session(sp.flag("id")); ok && s.State != wire.StateExited {
		t.Errorf("run's session %+v", s)
	}
	if _, ok := e.entry(id); ok {
		t.Error("schedule still listed")
	}
	if err := e.c.Call(t.Context(), wire.MethodScheduleRuns, wire.ScheduleRunsParams{ID: id}, nil); errCode(err) != wire.ErrNotFound {
		t.Errorf("runs of the removed schedule: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.layout.ScheduleRuns, id+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("runs file: %v", err)
	}
}

// Without stop_run the run goes on; the schedule and its runs are gone.
func TestScheduleRemoveLeavesRun(t *testing.T) {
	e := startScheduleDaemon(t, tokyoNine.Add(-time.Minute), nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "sleep 600", dailyNine())).Schedule.ID
	sp, h := e.startRunning(tokyoNine.Add(time.Second))
	e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result == wire.RunRunning })
	signals := fakeRunHolder(t, e, sp, h)
	call(t, e.c, wire.MethodScheduleRemove, wire.ScheduleRemoveParams{ID: id}, nil)
	select {
	case sig := <-signals:
		t.Errorf("signalled %s without stop_run", sig)
	case <-time.After(100 * time.Millisecond):
	}
	if s, ok := e.session(sp.flag("id")); !ok || s.State == wire.StateExited {
		t.Errorf("run's session %+v", s)
	}
	if _, err := os.Stat(filepath.Join(e.layout.ScheduleRuns, id+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("runs file: %v", err)
	}
	// Its end is not recorded anywhere.
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(e.layout.ScheduleRuns, id+".jsonl")); !os.IsNotExist(err) {
		t.Errorf("runs file after the run ended: %v", err)
	}
}
