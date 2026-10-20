//go:build !windows

package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// fakeClock is the wall clock schedules see; tests move it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

// spawned is one holder the daemon started for a schedule run.
type spawned struct {
	exe  string
	args []string
	dir  string
	env  []string
}

// flag returns the value of --name in a `stagent run` command line.
func (s spawned) flag(name string) string {
	i := slices.Index(s.args, "--"+name)
	if i < 0 || i+1 >= len(s.args) {
		return ""
	}
	return s.args[i+1]
}

// command is what the holder runs (after "--").
func (s spawned) command() []string {
	i := slices.Index(s.args, "--")
	if i < 0 {
		return nil
	}
	return s.args[i+1:]
}

type scheduleEnv struct {
	*testEnv
	clock  *fakeClock
	spawns chan spawned
	c      *rpc.Client
}

// startScheduleDaemon runs a daemon whose schedule clock is clock, whose
// login shell's environment is login and whose holders are only recorded.
func startScheduleDaemon(t *testing.T, at time.Time, login []string, spawnErr error) *scheduleEnv {
	t.Helper()
	clock := &fakeClock{t: at}
	spawns := make(chan spawned, 8)
	e := startDaemon(t, fastConfig(), Options{
		Now:          clock.Now,
		ScheduleTick: 10 * time.Millisecond,
		LoginEnv:     func() ([]string, error) { return login, nil },
		SpawnHolder: func(exe string, args []string, dir string, env []string, logPath string) (int, bool, error) {
			spawns <- spawned{exe, args, dir, env}
			return 0, false, spawnErr
		},
	})
	c, _ := e.client()
	return &scheduleEnv{testEnv: e, clock: clock, spawns: spawns, c: c}
}

func (e *scheduleEnv) create(s wire.Schedule) wire.ScheduleEntry {
	e.t.Helper()
	var entry wire.ScheduleEntry
	call(e.t, e.c, wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: s}, &entry)
	return entry
}

func (e *scheduleEnv) runs(id string) []wire.ScheduleRun {
	e.t.Helper()
	var res wire.ScheduleRunsResult
	call(e.t, e.c, wire.MethodScheduleRuns, wire.ScheduleRunsParams{ID: id}, &res)
	return res.Runs
}

func (e *scheduleEnv) awaitSpawn() spawned {
	e.t.Helper()
	select {
	case s := <-e.spawns:
		return s
	case <-time.After(5 * time.Second):
		e.t.Fatal("no holder started")
	}
	return spawned{}
}

func (e *scheduleEnv) noSpawn(d time.Duration) {
	e.t.Helper()
	select {
	case s := <-e.spawns:
		e.t.Fatalf("holder started early: %v", s.args)
	case <-time.After(d):
	}
}

// awaitRun waits until the newest run of schedule id satisfies pred.
func (e *scheduleEnv) awaitRun(id string, pred func(wire.ScheduleRun) bool) wire.ScheduleRun {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		runs := e.runs(id)
		if len(runs) > 0 && pred(runs[0]) {
			return runs[0]
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("runs of %s: %+v", id, runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// holderOf registers the session a spawned holder would and returns its
// connection.
func (e *scheduleEnv) holderOf(sp spawned) *rpc.Client {
	e.t.Helper()
	s := testSession(sp.flag("id"))
	s.Harness, s.Command, s.Cwd = wire.HarnessOther, sp.command(), sp.flag("cwd")
	s.ScheduleID, s.RunID = sp.flag("schedule"), sp.flag("run")
	return e.register(s)
}

func commandSchedule(cwd, command string, when wire.ScheduleWhen) wire.Schedule {
	return wire.Schedule{Name: "nightly", Kind: wire.ScheduleKindCommand, Command: command, Cwd: cwd,
		When: when, TZ: "Asia/Tokyo"}
}

// A daily 9:00 Asia/Tokyo schedule on a host whose clock is UTC starts at
// 0:00 UTC as a detached holder (120×40, its cwd, $SHELL -c, the login
// shell's environment), tied to the schedule and run; its exit is the
// run's result.
func TestScheduleRunsAtItsTime(t *testing.T) {
	oldLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = oldLocal })
	t.Setenv("SHELL", "/bin/zsh")
	cwd := t.TempDir()
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC) // 08:59 in Tokyo
	e := startScheduleDaemon(t, start, []string{"PATH=/login/bin:/usr/bin", "SHELL=/bin/zsh"}, nil)

	entry := e.create(commandSchedule(cwd, "date; exit 2", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}))
	nine := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if entry.NextAt != nine.UnixMilli() {
		t.Fatalf("next_at %v, want %v", time.UnixMilli(entry.NextAt).UTC(), nine)
	}
	id := entry.Schedule.ID
	e.noSpawn(50 * time.Millisecond)

	e.clock.Set(nine.Add(10 * time.Second))
	sp := e.awaitSpawn()
	if sp.args[0] != "run" || !slices.Contains(sp.args, "--detached") || sp.flag("cols") != "120" || sp.flag("rows") != "40" ||
		sp.flag("cwd") != cwd || sp.flag("schedule") != id || sp.flag("run") == "" {
		t.Errorf("holder args %v", sp.args)
	}
	if got := sp.command(); !slices.Equal(got, []string{"/bin/zsh", "-c", "date; exit 2"}) {
		t.Errorf("command %v", got)
	}
	if !slices.Contains(sp.env, "PATH=/login/bin:/usr/bin") {
		t.Errorf("env lacks the login shell's PATH: %v", sp.env)
	}
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return true })
	if run.Result != wire.RunRunning || run.ScheduledAt != nine.UnixMilli() || run.Trigger != wire.RunTriggerScheduled ||
		run.SessionID != sp.flag("id") || run.RunID != sp.flag("run") {
		t.Errorf("started run %+v", run)
	}

	h := e.holderOf(sp)
	var list wire.SessionsListResult
	call(t, e.c, wire.MethodSessionsList, nil, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ScheduleID != id || list.Sessions[0].RunID != run.RunID {
		t.Errorf("sessions %+v", list.Sessions)
	}
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 2})
	run = e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunFailed || run.ExitCode == nil || *run.ExitCode != 2 || run.EndedAt == 0 {
		t.Errorf("ended run %+v", run)
	}

	var sl wire.ScheduleListResult
	call(t, e.c, wire.MethodScheduleList, nil, &sl)
	if len(sl.Schedules) != 1 || sl.Schedules[0].LastRun == nil || sl.Schedules[0].LastRun.Result != wire.RunFailed ||
		sl.Schedules[0].NextAt != nine.Add(24*time.Hour).UnixMilli() {
		t.Errorf("list %+v", sl.Schedules)
	}

	// The next day it exits 0.
	e.clock.Set(nine.Add(24*time.Hour + time.Second))
	sp = e.awaitSpawn()
	h = e.holderOf(sp)
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 0})
	run = e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.RunID == sp.flag("run") && r.Result != wire.RunRunning })
	if run.Result != wire.RunSucceeded || run.ExitCode == nil || *run.ExitCode != 0 {
		t.Errorf("second run %+v", run)
	}
	if n := len(e.runs(id)); n != 2 {
		t.Errorf("%d runs recorded, want 2", n)
	}
}

// A holder that cannot be started fails the run with the reason.
func TestScheduleRunStartFails(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, errors.New("no such file"))
	entry := e.create(commandSchedule(t.TempDir(), "true", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}))
	e.clock.Set(start.Add(2 * time.Minute))
	e.awaitSpawn()
	run := e.awaitRun(entry.Schedule.ID, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunFailed || run.ExitCode != nil || !strings.Contains(run.Error, "no such file") {
		t.Errorf("run %+v", run)
	}
}

func TestSchedulePreview(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	start := time.Date(2026, 10, 19, 5, 30, 0, 0, time.UTC) // Monday 14:30 in Tokyo
	e := startScheduleDaemon(t, start, nil, nil)
	cwd := t.TempDir()
	var p wire.SchedulePreview
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "make test",
		wire.ScheduleWhen{Every: wire.EveryWeekdays, Time: "09:00"})}, &p)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	want := []int64{
		time.Date(2026, 10, 20, 9, 0, 0, 0, tokyo).UnixMilli(),
		time.Date(2026, 10, 21, 9, 0, 0, 0, tokyo).UnixMilli(),
		time.Date(2026, 10, 22, 9, 0, 0, 0, tokyo).UnixMilli(),
	}
	if !slices.Equal(p.Argv, []string{"/bin/bash", "-c", "make test"}) || p.Cwd != cwd || !slices.Equal(p.Next, want) || len(p.Warnings) != 0 {
		t.Errorf("preview %+v", p)
	}
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(filepath.Join(cwd, "gone"), "make test",
		wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})}, &p)
	if len(p.Warnings) != 1 || p.Warnings[0].Code != wire.WarnCwdMissing {
		t.Errorf("warnings %+v", p.Warnings)
	}
	var sl wire.ScheduleListResult
	call(t, e.c, wire.MethodScheduleList, nil, &sl)
	if len(sl.Schedules) != 0 {
		t.Errorf("preview saved a schedule: %+v", sl.Schedules)
	}
	err := e.c.Call(t.Context(), wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "x",
		wire.ScheduleWhen{Every: wire.EveryDaily, Time: "25:00"})}, nil)
	if errCode(err) != wire.ErrBadRequest {
		t.Errorf("preview of a bad time: %v", err)
	}
}

// The daemon tells the UTC offsets of a schedule's tz the app shows its
// times with: now, at each next time (across New York's change back to
// standard time on 2026-11-01) and at a run's time.
func TestScheduleOffsets(t *testing.T) {
	const tokyo, edt, est = 9 * 3600, -4 * 3600, -5 * 3600
	ny, _ := time.LoadLocation("America/New_York")
	start := time.Date(2026, 10, 30, 10, 0, 0, 0, ny) // Friday, EDT
	e := startScheduleDaemon(t, start, nil, nil)
	cwd := t.TempDir()
	daily := commandSchedule(cwd, "true", dailyNine())
	daily.TZ = "America/New_York"
	var p wire.SchedulePreview
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: daily}, &p)
	if p.TZOffset != edt || !slices.Equal(p.NextOffsets, []int{edt, est, est}) || len(p.Next) != 3 ||
		p.Next[1] != time.Date(2026, 11, 1, 9, 0, 0, 0, ny).UnixMilli() {
		t.Errorf("New York preview %+v", p)
	}
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "true", dailyNine())}, &p)
	if p.TZOffset != tokyo || !slices.Equal(p.NextOffsets, []int{tokyo, tokyo, tokyo}) {
		t.Errorf("Tokyo preview %+v", p)
	}
	once := commandSchedule(cwd, "true", wire.ScheduleWhen{Every: wire.EveryOnce, Date: "2026-10-01", Time: "09:00"})
	call(t, e.c, wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: once}, &p)
	if p.NextOffsets == nil || len(p.NextOffsets) != 0 {
		t.Errorf("preview without a next time %+v", p)
	}

	en := e.create(daily)
	if en.TZOffset != edt || en.NextOffset == nil || *en.NextOffset != edt {
		t.Errorf("entry %+v", en)
	}
	e.clock.Set(time.Date(2026, 10, 31, 9, 0, 1, 0, ny))
	e.endRun(en.Schedule.ID, e.awaitSpawn())
	e.clock.Set(time.Date(2026, 11, 1, 9, 0, 1, 0, ny))
	e.endRun(en.Schedule.ID, e.awaitSpawn())
	runs := e.runs(en.Schedule.ID)
	if len(runs) != 2 || runs[0].TZOffset != est || runs[1].TZOffset != edt {
		t.Errorf("runs %+v", runs)
	}
	en, _ = e.entry(en.Schedule.ID)
	if en.TZOffset != est || *en.NextOffset != est {
		t.Errorf("entry after the change %+v", en)
	}
	var paused wire.ScheduleEntry
	call(t, e.c, wire.MethodSchedulePause, wire.ScheduleRef{ID: en.Schedule.ID}, &paused)
	if paused.NextAt != 0 || paused.NextOffset != nil || paused.TZOffset != est {
		t.Errorf("paused %+v", paused)
	}
}

func TestScheduleCreateRefusesBadSchedules(t *testing.T) {
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	cwd := t.TempDir()
	daily := wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}
	for name, s := range map[string]wire.Schedule{
		"missing cwd": commandSchedule(filepath.Join(cwd, "gone"), "true", daily),
		"bad zone":    {Name: "n", Kind: wire.ScheduleKindCommand, Command: "true", Cwd: cwd, When: daily, TZ: "Local"},
		"agent kind":  {Name: "n", Kind: "agent", Cwd: cwd, When: daily, TZ: "UTC"},
	} {
		err := e.c.Call(t.Context(), wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: s}, nil)
		if errCode(err) != wire.ErrBadRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	err := e.c.Call(t.Context(), wire.MethodScheduleRuns, wire.ScheduleRunsParams{ID: "0123456789abcdef"}, nil)
	if errCode(err) != wire.ErrNotFound {
		t.Errorf("runs of an unknown schedule: %v", err)
	}
}

// schedules.json keeps the members this version does not know, and a
// daemon started again finds its schedules.
func TestSchedulesKeepUnknownMembers(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	cwd := t.TempDir()
	stateDir := filepath.Join(home, ".ssh-term", "agent", "state")
	os.MkdirAll(stateDir, 0o700)
	doc := `{"schedules":[{"id":"aaaaaaaaaaaaaaaa","name":"later","kind":"command","command":"true","cwd":"` + cwd +
		`","when":{"every":"daily","time":"03:00","jitter":5},"tz":"UTC","state":"active","created_at":1,"timeout_minutes":60,"notify":"always"}]}`
	os.WriteFile(filepath.Join(stateDir, "schedules.json"), []byte(doc), 0o600)

	clock := &fakeClock{t: time.Now()}
	e := startDaemonAt(t, fastConfig(), Options{Now: clock.Now, ScheduleTick: time.Hour})
	c, _ := e.client()
	var created wire.ScheduleEntry
	call(t, c, wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: commandSchedule(cwd, "date", wire.ScheduleWhen{Every: wire.EveryHours, Hours: 2})}, &created)

	b, _ := os.ReadFile(filepath.Join(stateDir, "schedules.json"))
	var f struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := json.Unmarshal(b, &f); err != nil || len(f.Schedules) != 2 {
		t.Fatalf("schedules.json: %s (%v)", b, err)
	}
	old := f.Schedules[0]
	if old["timeout_minutes"] != float64(60) || old["notify"] != "always" || old["when"].(map[string]any)["jitter"] != float64(5) {
		t.Errorf("unknown members lost: %v", old)
	}
	e.stop()

	e = startDaemonAt(t, fastConfig(), Options{Now: clock.Now, ScheduleTick: time.Hour})
	c, _ = e.client()
	var sl wire.ScheduleListResult
	call(t, c, wire.MethodScheduleList, nil, &sl)
	if len(sl.Schedules) != 2 || sl.Schedules[1].Schedule.ID != created.Schedule.ID || sl.Schedules[0].NextAt == 0 {
		t.Errorf("after restart: %+v", sl.Schedules)
	}
}

// A run still marked running whose session does not come back after a
// restart ends undetermined.
func TestScheduleRunLostOverRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	stateDir := filepath.Join(home, ".ssh-term", "agent", "state")
	os.MkdirAll(filepath.Join(stateDir, "schedule-runs"), 0o700)
	doc := `{"schedules":[{"id":"aaaaaaaaaaaaaaaa","name":"n","kind":"command","command":"true","cwd":"/","when":{"every":"daily","time":"03:00"},"tz":"UTC","state":"active","created_at":1}]}`
	os.WriteFile(filepath.Join(stateDir, "schedules.json"), []byte(doc), 0o600)
	os.WriteFile(filepath.Join(stateDir, "schedule-runs", "aaaaaaaaaaaaaaaa.jsonl"),
		[]byte(`{"run_id":"r1","scheduled_at":1,"started_at":1,"trigger":"scheduled","result":"running","session_id":"0123456789abcdef"}`+"\n"), 0o600)
	clock := &fakeClock{t: time.Now()}
	e := startDaemonAt(t, fastConfig(), Options{Now: clock.Now, ScheduleTick: 10 * time.Millisecond, RunStartGrace: 50 * time.Millisecond})
	c, _ := e.client()
	se := &scheduleEnv{testEnv: e, clock: clock, c: c}
	run := se.awaitRun("aaaaaaaaaaaaaaaa", func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunUndetermined || run.EndedAt == 0 {
		t.Errorf("run %+v", run)
	}
}
