package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/loginenv"
	"github.com/obutora/stagent/internal/proc"
	"github.com/obutora/stagent/internal/schedagent"
	"github.com/obutora/stagent/internal/schedule"
	"github.com/obutora/stagent/internal/wire"
)

// Schedules (定期実行) live in <root>/state/schedules.json and their runs
// in <root>/state/schedule-runs/<id>.jsonl (schedule.RunLog), written by
// the daemon only. Every ScheduleTick the daemon compares the wall clock
// with each schedule's next time, on the wall clock of the schedule's own
// time zone, and starts the ones that came as detached holders (`stagent
// run --detached --schedule --run`). A run's session tells its end
// (holder.ended → endLocked → scheduleRunEndedLocked).
//
// The comparison is repeated rather than waited for with one long timer:
// a Go timer may not advance while the machine sleeps, the wall clock
// does.

const (
	defaultScheduleTick = 30 * time.Second
	// defaultRunStartGrace is how long a run's holder has to register
	// (and, after a daemon restart, a running run's holder to come back).
	defaultRunStartGrace = 30 * time.Second
	// A run's terminal; attaching from the app resizes it.
	runCols, runRows = 120, 40
	// runErrorMax bounds ScheduleRun.Error.
	runErrorMax = 300
	// lateAfter: a run that starts longer after its time is late.
	lateAfter = time.Minute
	// runLoginEnvWithin: how long a run waits for the login shell's
	// environment, both tries in all (#1082), before it starts with the
	// daemon's own.
	runLoginEnvWithin = 5 * time.Second
)

// schedulesFile is the document stored in schedules.json.
type schedulesFile struct {
	Schedules []*wire.Schedule `json:"schedules"`
}

// pendingRun is a run whose end is not recorded yet.
type pendingRun struct {
	scheduleID string
	run        wire.ScheduleRun
	// since: when it was started, or found running at the daemon's start
	// (restored: its end can no longer be told unless its session comes
	// back); pid: its holder's, 0 until spawned or when not known.
	since    time.Time
	restored bool
	pid      int
	logPath  string
	// termAt: when it was sent terminate at its time limit (limit), killed
	// whether kill followed; removing: it is being stopped because its
	// schedule is being removed (stop_run), so its end is no result of
	// the schedule's: not notified, not counted, not retried.
	termAt   time.Time
	limit    time.Duration
	killed   bool
	removing bool
}

// schedulesServed: schedule.* and the runs work on Linux and macOS only
// (wire.SchedulesServedOn; the hosts the bridge announces them on).
var schedulesServed = wire.SchedulesServedOn(runtime.GOOS)

// loadSchedules reads schedules.json (none: no schedules).
func loadSchedules(path string) ([]*wire.Schedule, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f schedulesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(f.Schedules, func(s *wire.Schedule) bool { return s == nil || s.ID == "" }), nil
}

// saveSchedulesLocked writes schedules.json (0600, atomically).
func (d *Daemon) saveSchedulesLocked() error {
	b, err := json.MarshalIndent(schedulesFile{Schedules: d.schedules}, "", "  ")
	if err == nil {
		err = writeFileAtomic(d.layout.Schedules, b)
	}
	return err
}

// saveSchedulesOrLogLocked is saveSchedulesLocked (all of schedules.json)
// after a change of s the daemon keeps in memory either way (its counts,
// its last time): a failure is logged.
func (d *Daemon) saveSchedulesOrLogLocked(s *wire.Schedule) {
	if err := d.saveSchedulesLocked(); err != nil {
		d.logf("daemon: schedule %s: %v", s.ID, err)
	}
}

// now is the wall clock schedules are compared with, without a monotonic
// reading: comparisons then use the wall clock, which goes on while the
// machine sleeps.
func (d *Daemon) now() time.Time { return d.opts.Now().Round(0) }

// restoreSchedulesLocked arranges each schedule's next time from its last
// one on (the times that came meanwhile were missed and run once, late),
// takes back the runs a previous daemon left running and the retry it left
// waiting.
func (d *Daemon) restoreSchedulesLocked() {
	now := d.now()
	for _, s := range d.schedules {
		d.planLocked(s, d.sinceLocked(s, now))
		running, err := d.runs.Running(s.ID)
		if err != nil {
			d.logf("daemon: schedule %s runs: %v", s.ID, err)
		}
		for _, r := range running {
			d.pendingRuns[r.RunID] = &pendingRun{scheduleID: s.ID, run: r, since: time.Now(), restored: true}
		}
		if len(running) == 0 {
			d.restoreRetryLocked(s)
		}
	}
}

// restoreRetryLocked has s wait again for the retry of its newest run: one
// whose newest try failed with retries left, of a time not dealt with
// since (LastAt: no pause and resume, update, or later time came after
// it) while s is active.
func (d *Daemon) restoreRetryLocked(s *wire.Schedule) {
	r, ok, err := d.runs.Last(s.ID)
	if err != nil || !ok || s.State != wire.ScheduleActive || !runFailed(r.Result) || r.Retry >= r.Retries ||
		r.ScheduledAt < s.LastAt {
		return
	}
	d.retryWaits[s.ID] = &retryWait{at: time.UnixMilli(r.EndedAt).Add(schedule.RetryDelay(*s)), run: r}
}

// sinceLocked is when s's times start counting: its last time (LastAt),
// or now for a schedule without one.
func (d *Daemon) sinceLocked(s *wire.Schedule, now time.Time) time.Time {
	if s.LastAt > 0 {
		return time.UnixMilli(s.LastAt)
	}
	return now
}

// planLocked sets when s runs next after after (none for a schedule this
// version cannot run). A paused once schedule's time is kept, to record
// that it passed (skipped_paused).
func (d *Daemon) planLocked(s *wire.Schedule, after time.Time) {
	delete(d.schedDue, s.ID)
	if s.Kind != wire.ScheduleKindCommand && s.Kind != wire.ScheduleKindAgent {
		return
	}
	if s.State != wire.ScheduleActive && (s.State != wire.SchedulePaused || s.When.Every != wire.EveryOnce) {
		return
	}
	loc, err := schedule.Location(s.TZ)
	if err != nil {
		return
	}
	if t, ok := schedule.Next(s.When, loc, after); ok {
		d.schedDue[s.ID] = t
	}
}

func (d *Daemon) scheduleLocked(id string) *wire.Schedule {
	for _, s := range d.schedules {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// runSchedules compares the clock with the schedules every ScheduleTick
// until Shutdown.
func (d *Daemon) runSchedules() {
	tick := time.NewTicker(d.opts.ScheduleTick)
	defer tick.Stop()
	for {
		d.tickSchedules()
		select {
		case <-d.shutdown:
			return
		case <-tick.C:
		}
	}
}

// runStart is a run to start: its schedule as it was when it came due.
type runStart struct {
	s   wire.Schedule
	run wire.ScheduleRun
}

// tickSchedules starts the runs whose time came and reviews the runs that
// did not start.
func (d *Daemon) tickSchedules() {
	now := d.now()
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	var starts []runStart
	for _, s := range d.schedules {
		if start, ok := d.dueLocked(s, now); ok {
			starts = append(starts, start)
		}
	}
	starts = append(starts, d.retriesDueLocked(now)...)
	term, kill := d.timeoutsLocked(now)
	d.reviewRunsLocked()
	d.mu.Unlock()
	for _, r := range starts {
		go d.startRun(r)
	}
	if len(term)+len(kill) > 0 {
		go d.stopTimedOut(term, kill)
	}
}

// dueLocked deals with s's time when it came (by now): a run to start
// (true), or recorded as skipped, or none when it dropped the retry s
// waited to run and that stopped s.
func (d *Daemon) dueLocked(s *wire.Schedule, now time.Time) (runStart, bool) {
	at, ok := d.schedDue[s.ID]
	if !ok || now.Before(at) {
		return runStart{}, false
	}
	at = d.passedLocked(s, at, now)
	// Its next time drops the retry it waits to run.
	if d.dropRetryLocked(s) {
		d.scheduleChangedLocked(s)
		return runStart{}, false
	}
	if s.State == wire.SchedulePaused {
		d.skipPausedLocked(s, at, now)
		return runStart{}, false
	}
	if d.runningLocked(s.ID) {
		d.skipOverlapLocked(s, at, now)
		return runStart{}, false
	}
	trigger := wire.RunTriggerScheduled
	if now.Sub(at) > lateAfter {
		trigger = wire.RunTriggerLate
	}
	start := d.addRunLocked(s, newRun(s, at, now, trigger, wire.RunRunning))
	d.scheduleChangedLocked(s)
	return start, true
}

// passedLocked deals with s's times from at up to now, which came: they
// count as one, the newest (returned). It is kept as s's last time
// (schedules.json), so that a daemon started again neither runs it again
// nor misses the ones after it, and s runs next after it.
func (d *Daemon) passedLocked(s *wire.Schedule, at, now time.Time) time.Time {
	if loc, err := schedule.Location(s.TZ); err == nil {
		at = schedule.Latest(s.When, loc, at, now)
	}
	s.LastAt = at.UnixMilli()
	d.saveSchedulesOrLogLocked(s)
	d.planLocked(s, at)
	return at
}

// skipPausedLocked records that paused once schedule s's time at passed.
func (d *Daemon) skipPausedLocked(s *wire.Schedule, at, now time.Time) {
	d.addRunLocked(s, newRun(s, at, now, wire.RunTriggerScheduled, wire.RunSkippedPaused))
	d.scheduleChangedLocked(s)
}

// newRun is a run of s due at at, started (running) or skipped (result
// skipped_*) now.
func newRun(s *wire.Schedule, at, now time.Time, trigger, result string) wire.ScheduleRun {
	run := wire.ScheduleRun{
		RunID: wire.NewSessionID(), ScheduledAt: at.UnixMilli(), Trigger: trigger, Cwd: s.Cwd,
		ScheduleTry: wire.ScheduleTry{StartedAt: now.UnixMilli(), Result: result},
	}
	if result == wire.RunRunning {
		run.SessionID, run.Retries = wire.NewSessionID(), s.Retries
	} else {
		run.EndedAt = now.UnixMilli()
	}
	if s.Kind == wire.ScheduleKindAgent {
		run.Harness = s.Harness
	}
	if loc, err := schedule.Location(s.TZ); err == nil {
		run.TZOffset = schedule.OffsetAt(loc, at)
	}
	return run
}

// addRunLocked records run as s's newest; a running one waits for its end
// and is to start as s is now (startLocked).
func (d *Daemon) addRunLocked(s *wire.Schedule, run wire.ScheduleRun) runStart {
	if err := d.runs.Add(s.ID, run); err != nil {
		d.logf("daemon: schedule %s runs: %v", s.ID, err)
	}
	if run.Result != wire.RunRunning {
		return runStart{*s, run}
	}
	return d.startLocked(s, run)
}

// startLocked has run's newest try, recorded running, wait for its end
// (pendingRuns); it is to start as s is now.
func (d *Daemon) startLocked(s *wire.Schedule, run wire.ScheduleRun) runStart {
	d.pendingRuns[run.RunID] = &pendingRun{scheduleID: s.ID, run: run, since: time.Now()}
	return runStart{*s, run}
}

// startRun starts the holder of a run: `stagent run --detached` with the
// login shell's environment taken now (loginenv, 5 s at most; the daemon's
// own when that fails), $SHELL -c '<command>' in the schedule's directory
// on a 120×40 terminal.
func (d *Daemon) startRun(r runStart) {
	env := os.Environ()
	if login, err := d.opts.LoginEnv(); err != nil {
		d.logf("daemon: schedule %s: login shell environment: %v (using the daemon's)", r.s.ID, err)
	} else {
		env = loginenv.Merge(env, login)
	}
	sid := r.run.SessionID
	logPath := filepath.Join(d.layout.LogDir, sid+".log")
	args := []string{
		"run", "--detached", "--id", sid,
		"--cols", strconv.Itoa(runCols), "--rows", strconv.Itoa(runRows),
		"--cwd", r.s.Cwd, "--schedule", r.s.ID, "--run", r.run.RunID, "--",
	}
	exe, err := daemonclient.Exe()
	args = append(args, d.runCommand(exe, env, r.s, sid)...)
	pid, outsideGUI := 0, false
	if err == nil {
		d.mu.Lock()
		closed := d.closed
		d.mu.Unlock()
		if closed {
			return
		}
		pid, outsideGUI, err = d.opts.SpawnHolder(exe, args, d.layout.Home, env, logPath)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.endRunLocked(r.run.RunID, wire.RunFailed, nil, "start: "+err.Error())
		return
	}
	if p := d.pendingRuns[r.run.RunID]; p != nil {
		p.pid, p.logPath = pid, logPath
		if outsideGUI {
			d.runOutsideGUILocked(p)
		}
	}
}

// runArgv is what a schedule's run executes: $SHELL -c '<command>', the
// shell of env (/bin/sh when unset).
// An agent schedule's is its agent's (schedagent.Argv; HOME of env).
func runArgv(env []string, s wire.Schedule) []string {
	if s.Kind == wire.ScheduleKindAgent {
		return schedagent.Argv(s, loginenv.Get(env, "HOME"))
	}
	sh := loginenv.Get(env, "SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	return []string{sh, "-c", s.Command}
}

// reviewRunsLocked ends the runs that will not tell their end: one whose
// holder exited without registering, or did not register within
// RunStartGrace (failed), and one left running by a previous daemon whose
// session did not come back (undetermined).
func (d *Daemon) reviewRunsLocked() {
	for id, p := range d.pendingRuns {
		if s := d.sessions[p.run.SessionID]; s != nil {
			continue // its end comes from the session
		}
		late := time.Since(p.since) > d.opts.RunStartGrace
		switch {
		case p.restored && late:
			d.endRunLocked(id, wire.RunUndetermined, nil, "")
		case p.restored:
		case p.pid > 0 && !proc.Alive(p.pid):
			d.endRunLocked(id, wire.RunFailed, nil, "holder exited during start"+logTail(p.logPath))
		case late:
			d.endRunLocked(id, wire.RunFailed, nil, "holder did not start within "+d.opts.RunStartGrace.String()+logTail(p.logPath))
		}
	}
}

// scheduleRunEndedLocked records the end of a run's session (its newest
// try's; an older try's ended before): a command run succeeded when its
// exit code is 0. A lost session has no exit code.
func (d *Daemon) scheduleRunEndedLocked(s *session, exitCode int, lost bool) {
	p := d.pendingRuns[s.s.RunID]
	if p == nil || p.run.SessionID != s.s.ID {
		return
	}
	if p.run.Harness != "" {
		d.agentRunEndedLocked(p, s.s.ID, exitCode, lost)
		return
	}
	if lost {
		d.endRunLocked(s.s.RunID, wire.RunFailed, nil, "session lost")
		return
	}
	result := wire.RunFailed
	if exitCode == 0 {
		result = wire.RunSucceeded
	}
	d.endRunLocked(s.s.RunID, result, &exitCode, "")
}

// endRunLocked records how a pending run ended.
func (d *Daemon) endRunLocked(runID, result string, exitCode *int, msg string) {
	p := d.pendingRuns[runID]
	if p == nil {
		return
	}
	delete(d.pendingRuns, runID)
	r := p.run
	r.EndedAt = d.now().UnixMilli()
	result, msg = timedOutLocked(p, result, msg)
	r.Result, r.ExitCode, r.Error = result, exitCode, clipLine(msg, runErrorMax)
	if result == wire.RunTimedOut {
		// Killed at its time limit, an agent run leaves no outcome: it
		// timed out, not "could not be judged".
		r.Unjudged = false
	}
	if r.Summary == "" { // an agent run's is its last reply
		r.Summary = d.runSummary(r.SessionID)
	}
	if err := d.runs.Update(p.scheduleID, r); err != nil {
		d.logf("daemon: schedule %s runs: %v", p.scheduleID, err)
	}
	d.runEndedLocked(p.scheduleID, p, r)
	d.scheduleIDChangedLocked(p.scheduleID)
}

// logTail is the end of a holder's log for a run's error ("" when empty).
func logTail(path string) string {
	if path == "" {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	const n = 300
	if len(b) > n {
		b = b[len(b)-n:]
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return ": " + s
	}
	return ""
}

// ---------------------------------------------------------------------------
// schedule.list, schedule.create, schedule.runs, schedule.preview

func (d *Daemon) entryLocked(s *wire.Schedule) wire.ScheduleEntry {
	e := wire.ScheduleEntry{Schedule: *s}
	if t, ok := d.schedDue[s.ID]; ok && s.State == wire.ScheduleActive {
		e.NextAt = t.UnixMilli()
	}
	if r, ok, err := d.runs.Last(s.ID); err != nil {
		d.logf("daemon: schedule %s runs: %v", s.ID, err)
	} else if ok {
		e.LastRun = &r
	}
	// A run still running stays the newest run while times skipped
	// meanwhile (skipped_overlap) are recorded after it.
	for _, p := range d.pendingRuns {
		if p.scheduleID == s.ID {
			r := p.run
			e.LastRun = &r
			break
		}
	}
	if w := d.retryWaits[s.ID]; w != nil {
		e.RetryAt = w.at.UnixMilli()
	}
	if loc, err := schedule.Location(s.TZ); err == nil {
		now := d.now()
		e.TZOffset = schedule.OffsetAt(loc, now)
		if e.NextAt > 0 {
			off := schedule.OffsetAt(loc, time.UnixMilli(e.NextAt))
			e.NextOffset = &off
		}
		// The host's clock at its next time; paused, at the next time it
		// would have.
		at, ok := d.schedDue[s.ID]
		if !ok {
			at, ok = schedule.Next(s.When, loc, now)
		}
		if ok {
			e.HostClock = schedule.HostClockAt(loc, at)
		}
	}
	return e
}

func (d *Daemon) scheduleList() wire.ScheduleListResult {
	d.mu.Lock()
	defer d.mu.Unlock()
	res := wire.ScheduleListResult{Schedules: make([]wire.ScheduleEntry, 0, len(d.schedules))}
	for _, s := range d.schedules {
		res.Schedules = append(res.Schedules, d.entryLocked(s))
	}
	return res
}

// scheduleCreate saves a new schedule. Its directory must be there.
func (d *Daemon) scheduleCreate(p wire.ScheduleCreateParams) (wire.ScheduleEntry, error) {
	const method = wire.MethodScheduleCreate
	s := p.Schedule
	s.Name = strings.TrimSpace(s.Name)
	s.Cwd = d.layout.ExpandHome(s.Cwd)
	s = schedule.Normalize(s)
	if err := schedule.Validate(s); err != nil {
		return wire.ScheduleEntry{}, wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
	}
	if st, err := os.Stat(s.Cwd); err != nil || !st.IsDir() {
		return wire.ScheduleEntry{}, wire.Errorf(wire.ErrBadRequest, "%s: cwd %q is not a directory", method, s.Cwd)
	}
	now := d.now()
	s.ID, s.State, s.CreatedAt = wire.NewSessionID(), wire.ScheduleActive, now.UnixMilli()
	s.LastAt = now.UnixMilli()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.schedules = append(d.schedules, &s)
	if err := d.saveSchedulesLocked(); err != nil {
		d.schedules = d.schedules[:len(d.schedules)-1]
		return wire.ScheduleEntry{}, wire.Errorf(wire.ErrInternal, "schedules: %v", err)
	}
	d.planLocked(&s, now)
	return d.scheduleChangedLocked(&s), nil
}

func (d *Daemon) scheduleRuns(p wire.ScheduleRunsParams) (wire.ScheduleRunsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scheduleLocked(p.ID) == nil {
		return wire.ScheduleRunsResult{}, notFound(wire.MethodScheduleRuns, p.ID)
	}
	runs, err := d.runs.List(p.ID)
	if err != nil {
		return wire.ScheduleRunsResult{}, wire.Errorf(wire.ErrInternal, "schedule runs: %v", err)
	}
	if runs == nil {
		runs = []wire.ScheduleRun{}
	}
	return wire.ScheduleRunsResult{Runs: runs}, nil
}

// schedulePreview answers what s would run and when, without saving it.
// The shell is the daemon's $SHELL, as the login shell it starts to take
// a run's environment.
func (d *Daemon) schedulePreview(p wire.ScheduleCreateParams) (wire.SchedulePreview, error) {
	const method = wire.MethodSchedulePreview
	s := p.Schedule
	s.Cwd = d.layout.ExpandHome(s.Cwd)
	if err := schedule.ValidateRun(s); err != nil {
		return wire.SchedulePreview{}, wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
	}
	if !filepath.IsAbs(s.Cwd) {
		return wire.SchedulePreview{}, wire.Errorf(wire.ErrBadRequest, "%s: cwd %q is not absolute", method, s.Cwd)
	}
	loc, _ := schedule.Location(s.TZ)
	now := d.now()
	res := wire.SchedulePreview{
		Argv: runArgv(os.Environ(), s), Cwd: s.Cwd, Next: []int64{}, NextOffsets: []int{},
		TZOffset: schedule.OffsetAt(loc, now), Warnings: []wire.ScheduleWarning{},
	}
	for _, t := range schedule.NextN(s.When, loc, now, 3) {
		res.Next = append(res.Next, t.UnixMilli())
		res.NextOffsets = append(res.NextOffsets, schedule.OffsetAt(loc, t))
	}
	if len(res.Next) > 0 {
		res.HostClock = schedule.HostClockAt(loc, time.UnixMilli(res.Next[0]))
	}
	if st, err := os.Stat(s.Cwd); err != nil || !st.IsDir() {
		res.Warnings = append(res.Warnings, wire.ScheduleWarning{Code: wire.WarnCwdMissing, Message: s.Cwd + " is not a directory on this host"})
	}
	d.mu.Lock()
	d.previewPlaceLocked(&res, s)
	d.mu.Unlock()
	previewGUILogin(&res, s)
	return res, nil
}
