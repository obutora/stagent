//go:build !windows

package daemon

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/scrollback"
	"github.com/obutora/stagent/internal/wire"
)

// writeOutput leaves output in session id's scrollback as its holder
// would.
func writeOutput(t *testing.T, e *testEnv, id, output string) {
	t.Helper()
	sb, err := scrollback.Open(e.layout.SessionDataDir(id), 0)
	if err != nil {
		t.Fatal(err)
	}
	sb.Write([]byte(output))
	sb.Close()
}

// An ended run's output is read back from its session's scrollback on
// disk, and its record keeps the end of it as plain text; once retention
// deleted it, schedule.output says it is gone and the summary is left.
func TestScheduleOutputOfAnEndedRun(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "make test", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})).Schedule.ID
	e.clock.Set(start.Add(2 * time.Minute))
	sp := e.awaitSpawn()
	h := e.holderOf(sp)
	output := "$ make test\r\n\x1b[31mFAIL\x1b[0m cart\r\nmake: *** Error 2\r\n"
	writeOutput(t, e.testEnv, sp.flag("id"), output)
	h.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 2})
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Summary != "$ make test\nFAIL cart\nmake: *** Error 2" {
		t.Errorf("summary %q", run.Summary)
	}

	var out wire.ScheduleOutputResult
	call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: run.RunID}, &out)
	if string(out.Data) != output || out.Start != 0 || out.First != 0 || out.End != int64(len(output)) || out.Gone {
		t.Errorf("output %+v", out)
	}
	call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: run.RunID, Before: 7, MaxBytes: 5}, &out)
	if string(out.Data) != "make " || out.Start != 2 || out.End != 7 {
		t.Errorf("page %+v", out)
	}

	os.RemoveAll(e.layout.SessionDataDir(sp.flag("id")))
	out = wire.ScheduleOutputResult{}
	call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: run.RunID}, &out)
	if !out.Gone || len(out.Data) != 0 {
		t.Errorf("deleted output %+v", out)
	}
	if got := e.runs(id)[0].Summary; got != run.Summary {
		t.Errorf("summary after the output went %q", got)
	}

	for _, p := range []wire.ScheduleOutputParams{{ID: id, RunID: "ffffffffffffffff"}, {ID: "ffffffffffffffff", RunID: run.RunID}} {
		err := e.c.Call(context.Background(), wire.MethodScheduleOutput, p, nil)
		if errCode(err) != wire.ErrNotFound {
			t.Errorf("%+v: %v, want not_found", p, err)
		}
	}
}

// A running run's output so far is read the same way.
func TestScheduleOutputOfARunningRun(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	id := e.create(commandSchedule(t.TempDir(), "sleep 60", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})).Schedule.ID
	e.clock.Set(start.Add(2 * time.Minute))
	sp := e.awaitSpawn()
	e.holderOf(sp)
	sb, err := scrollback.Open(e.layout.SessionDataDir(sp.flag("id")), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	sb.Write([]byte("working\r\n"))
	var out wire.ScheduleOutputResult
	call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: sp.flag("run")}, &out)
	if string(out.Data) != "working\r\n" || out.Gone {
		t.Errorf("output %+v", out)
	}
}

// A run that never had a session (its holder could not start) has no
// output.
func TestScheduleOutputOfARunWithoutSession(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, fmt.Errorf("no such file"))
	id := e.create(commandSchedule(t.TempDir(), "true", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})).Schedule.ID
	e.clock.Set(start.Add(2 * time.Minute))
	e.awaitSpawn()
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	var out wire.ScheduleOutputResult
	call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: run.RunID}, &out)
	if !out.Gone || run.Summary != "" {
		t.Errorf("output %+v, summary %q", out, run.Summary)
	}
}

// Each try of a retried run keeps its own output: schedule.output reads the
// newest try's, or the try session_id names; a session of no try of the
// run is not_found.
func TestScheduleOutputOfEachTry(t *testing.T) {
	start := time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)
	e := startScheduleDaemon(t, start, nil, nil)
	s := commandSchedule(t.TempDir(), "make test", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"})
	s.Retries, s.RetryDelayMin = 1, 1
	id := e.create(s).Schedule.ID
	e.clock.Set(start.Add(2 * time.Minute))
	first := e.awaitSpawn()
	writeOutput(t, e.testEnv, first.flag("id"), "first try\r\n")
	e.ends(first, 2)
	e.clock.Set(start.Add(4 * time.Minute))
	second := e.awaitSpawn()
	writeOutput(t, e.testEnv, second.flag("id"), "second try\r\n")
	e.ends(second, 0)

	for _, c := range []struct{ session, want string }{{"", "second try\r\n"}, {second.flag("id"), "second try\r\n"}, {first.flag("id"), "first try\r\n"}} {
		var out wire.ScheduleOutputResult
		call(t, e.c, wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: id, RunID: first.flag("run"), SessionID: c.session}, &out)
		if string(out.Data) != c.want || out.Gone {
			t.Errorf("output of session %q: %+v", c.session, out)
		}
	}
	err := e.c.Call(context.Background(), wire.MethodScheduleOutput,
		wire.ScheduleOutputParams{ID: id, RunID: first.flag("run"), SessionID: "ffffffffffffffff"}, nil)
	if errCode(err) != wire.ErrNotFound {
		t.Errorf("a session of no try: %v, want not_found", err)
	}
}

// retentionEnv is a daemon with retention cfg and schedule id, whose runs
// the test adds: n runs, newest last, each a session whose scrollback of
// size bytes was last written age ago (age[i] for run i).
type retentionEnv struct {
	*testEnv
	id string
}

func startRetentionDaemon(t *testing.T, r wire.RetentionConfig) *retentionEnv {
	t.Helper()
	cfg := fastConfig()
	cfg.Retention = r
	e := startDaemon(t, cfg, Options{
		ScheduleTick: time.Hour,
		LoginEnv:     func() ([]string, error) { return nil, nil },
		SpawnHolder:  func(string, []string, string, []string, string) (int, bool, error) { return 0, false, nil },
	})
	c, _ := e.client()
	var entry wire.ScheduleEntry
	call(t, c, wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: commandSchedule(t.TempDir(), "true",
		wire.ScheduleWhen{Every: wire.EveryWeekly, Days: []int{0}, Time: "03:00"})}, &entry)
	return &retentionEnv{testEnv: e, id: entry.Schedule.ID}
}

// addRun records an ended run of the schedule whose session's scrollback
// of size bytes was last written age ago, and returns that directory.
func (e *retentionEnv) addRun(n int, age time.Duration, size int) string {
	e.t.Helper()
	return e.addRetriedRun(n, 0, age, size)[0]
}

// addRetriedRun is addRun of a run tried 1+retries times, each try with a
// session of its own (their directories, oldest first).
func (e *retentionEnv) addRetriedRun(n, retries int, age time.Duration, size int) []string {
	e.t.Helper()
	r := wire.ScheduleRun{RunID: fmt.Sprintf("%016x", 0xb000+n), Retries: retries}
	var dirs []string
	for i := 0; i <= retries; i++ {
		sid := fmt.Sprintf("%016x", 0xa000+n*10+i)
		if i > 0 {
			r.Tries = append(r.Tries, r.ScheduleTry)
		}
		r.ScheduleTry = wire.ScheduleTry{Retry: i, StartedAt: int64(n), Result: wire.RunFailed, SessionID: sid}
		dirs = append(dirs, makeDataDir(e.testEnv, sid, age, size))
	}
	if err := e.d.runs.Add(e.id, r); err != nil {
		e.t.Fatal(err)
	}
	return dirs
}

func makeDataDir(e *testEnv, id string, age time.Duration, size int) string {
	e.t.Helper()
	dir := e.layout.SessionDataDir(id)
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, fmt.Sprintf("%020d.seg", 0))
	os.WriteFile(p, bytes.Repeat([]byte("x"), size), 0o600)
	mt := time.Now().Add(-age)
	os.Chtimes(p, mt, mt)
	os.Chtimes(dir, mt, mt)
	return dir
}

func exists(dir string) bool {
	_, err := os.Stat(dir)
	return err == nil
}

// Past scrollback_days, the output of a schedule's newest 5 runs stays and
// that of its older runs goes, as does any other session's.
func TestRetentionKeepsTheNewestFiveRunOutputs(t *testing.T) {
	e := startRetentionDaemon(t, wire.RetentionConfig{ScrollbackDays: 3, ScrollbackTotalMiB: 512})
	var dirs []string
	for n := 1; n <= 7; n++ {
		dirs = append(dirs, e.addRun(n, time.Duration(30-n)*24*time.Hour, 10))
	}
	other := makeDataDir(e.testEnv, "cccccccccccccccc", 4*24*time.Hour, 10)
	e.d.sweepNow()
	var kept []string
	for i, dir := range dirs {
		if exists(dir) {
			kept = append(kept, fmt.Sprint(i+1))
		}
	}
	if got := strings.Join(kept, ","); got != "3,4,5,6,7" {
		t.Errorf("runs whose output was kept: %s, want 3,4,5,6,7", got)
	}
	if exists(other) {
		t.Error("an old session that is no run was kept")
	}
}

// The 5 kept are times, not tries: each of the newest 5 runs keeps the
// output of all its tries.
func TestRetentionKeepsTheNewestFiveRunsWithTheirRetries(t *testing.T) {
	e := startRetentionDaemon(t, wire.RetentionConfig{ScrollbackDays: 3, ScrollbackTotalMiB: 512})
	var runs [][]string
	for n := 1; n <= 6; n++ {
		runs = append(runs, e.addRetriedRun(n, 2, time.Duration(30-n)*24*time.Hour, 10))
	}
	e.d.sweepNow()
	var kept []string
	for i, dirs := range runs {
		for j, dir := range dirs {
			if exists(dir) {
				kept = append(kept, fmt.Sprintf("%d#%d", i+1, j))
			}
		}
	}
	if got := strings.Join(kept, ","); got != "2#0,2#1,2#2,3#0,3#1,3#2,4#0,4#1,4#2,5#0,5#1,5#2,6#0,6#1,6#2" {
		t.Errorf("tries whose output was kept: %s, want all of runs 2 to 6", got)
	}
}

// The newest runs' output counts toward scrollback_total_mib: over it, the
// oldest output goes first, theirs included.
func TestRetentionTotalCountsRunOutputs(t *testing.T) {
	e := startRetentionDaemon(t, wire.RetentionConfig{ScrollbackDays: 3, ScrollbackTotalMiB: 1})
	var dirs []string
	for n := 1; n <= 5; n++ {
		dirs = append(dirs, e.addRun(n, time.Duration(10-n)*time.Hour, 400<<10))
	}
	e.d.sweepNow()
	var kept []string
	for i, dir := range dirs {
		if exists(dir) {
			kept = append(kept, fmt.Sprint(i+1))
		}
	}
	if got := strings.Join(kept, ","); got != "4,5" {
		t.Errorf("runs whose output was kept: %s, want 4,5 (1 MiB holds two of 400 KiB)", got)
	}
}
