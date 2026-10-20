package daemon

import (
	"time"

	"github.com/obutora/stagent/internal/schedule"
	"github.com/obutora/stagent/internal/wire"
)

// The time limit, retries and stopping after failures of schedules
// (#1083). Every ScheduleTick, a run still running at its schedule's time
// limit is sent terminate, and kill runTimeoutKillAfter later; its end is
// recorded as timed out. A run that failed or timed out is tried again (a
// retry, a new try in the run's line) RetryDelay after it ended, up to the
// run's Retries times, unless its schedule is paused; the schedule's next
// time, run_now or pausing it coming first drops the retries left. A time
// whose last try failed counts toward StopAfterFailures, which pauses the
// schedule (AutoStopped).

// runTimeoutKillAfter: how long a timed-out run has after terminate.
const runTimeoutKillAfter = 10 * time.Second

// retryWait is a retry a schedule waits to run: at, of run (the run that
// failed).
type retryWait struct {
	at  time.Time
	run wire.ScheduleRun
}

// timeoutsLocked marks the runs past their schedule's time limit and
// returns the sessions to send terminate (they just passed it) and kill
// (runTimeoutKillAfter after terminate).
func (d *Daemon) timeoutsLocked(now time.Time) (term, kill []string) {
	for _, p := range d.pendingRuns {
		if d.sessions[p.run.SessionID] == nil {
			continue // not started, or its end comes another way
		}
		if !p.termAt.IsZero() {
			if !p.killed && now.Sub(p.termAt) >= runTimeoutKillAfter {
				p.killed = true
				kill = append(kill, p.run.SessionID)
			}
			continue
		}
		s := d.scheduleLocked(p.scheduleID)
		if s == nil {
			continue
		}
		limit := schedule.Timeout(*s)
		if limit > 0 && now.Sub(time.UnixMilli(p.run.StartedAt)) >= limit {
			p.termAt, p.limit = now, limit
			term = append(term, p.run.SessionID)
		}
	}
	return term, kill
}

// stopTimedOut sends terminate and kill to the sessions timeoutsLocked
// returned. A look at the clock comes runTimeoutKillAfter after
// terminate, so kill does not wait for the next ScheduleTick.
func (d *Daemon) stopTimedOut(term, kill []string) {
	if len(term) > 0 {
		d.signalSessions(term, wire.SignalTerminate)
		time.AfterFunc(runTimeoutKillAfter+100*time.Millisecond, d.tickSchedules)
	}
	if len(kill) > 0 {
		d.signalSessions(kill, wire.SignalKill)
	}
}

// timedOutLocked turns the end of a run stopped at its time limit into
// timed out.
func timedOutLocked(p *pendingRun, result, msg string) (string, string) {
	if p.termAt.IsZero() || result == wire.RunUndetermined {
		return result, msg
	}
	return wire.RunTimedOut, "timed out after " + p.limit.String()
}

// runFailed: a run that ended so is retried and counts as a failure.
func runFailed(result string) bool {
	return result == wire.RunFailed || result == wire.RunTimedOut
}

// runEndedLocked follows the recorded end r of a run of schedule id: a
// retry when it failed, retries are left and s is not paused (stopped by
// hand or after failures), else the end of its time. A run stopped to
// remove its schedule ends nothing of it.
func (d *Daemon) runEndedLocked(id string, p *pendingRun, r wire.ScheduleRun) {
	s := d.scheduleLocked(id)
	if s == nil || p.removing {
		return
	}
	if runFailed(r.Result) && s.State == wire.ScheduleActive && r.Retry < r.Retries {
		d.retryWaits[id] = &retryWait{at: d.now().Add(schedule.RetryDelay(*s)), run: r}
		return
	}
	stopped := false
	switch {
	case runFailed(r.Result):
		stopped = d.timeFailedLocked(s, r)
	case r.Result == wire.RunSucceeded || r.Result == wire.RunDenied:
		if s.FailedInRow > 0 {
			s.FailedInRow = 0
			d.saveSchedulesOrLogLocked(s)
		}
	}
	if !stopped { // else its stop was notified
		d.notifyRunLocked(s, r)
	}
}

// timeFailedLocked counts a time of s whose last try failed (run r), and
// pauses s when it is the StopAfterFailures-th in a row: r's record then
// says so (auto_stopped). True when it paused s.
func (d *Daemon) timeFailedLocked(s *wire.Schedule, r wire.ScheduleRun) bool {
	s.FailedInRow++
	stopped := s.StopAfterFailures > 0 && s.FailedInRow >= s.StopAfterFailures && s.State == wire.ScheduleActive
	if stopped {
		s.State, s.AutoStopped = wire.SchedulePaused, true
		delete(d.retryWaits, s.ID)
		d.planLocked(s, d.now())
		d.logf("daemon: schedule %s stopped after %d failures in a row", s.ID, s.FailedInRow)
		r.AutoStopped = s.FailedInRow
		if err := d.runs.Update(s.ID, r); err != nil {
			d.logf("daemon: schedule %s runs: %v", s.ID, err)
		}
		d.notifyAutoStoppedLocked(s)
	}
	d.saveSchedulesOrLogLocked(s)
	return stopped
}

// dropRetryLocked drops the retry s waits to run, its time ending failed;
// true when that stopped s.
func (d *Daemon) dropRetryLocked(s *wire.Schedule) bool {
	w := d.retryWaits[s.ID]
	if w == nil {
		return false
	}
	delete(d.retryWaits, s.ID)
	stopped := d.timeFailedLocked(s, w.run)
	if !stopped {
		d.notifyRunLocked(s, w.run)
	}
	return stopped
}

// retriesDueLocked starts the retries whose time came (none of a paused
// schedule: pausing drops its retry), in the directory their time started
// in (the run's Cwd, the same for all its tries).
func (d *Daemon) retriesDueLocked(now time.Time) []runStart {
	var starts []runStart
	for _, s := range d.schedules {
		w := d.retryWaits[s.ID]
		if w == nil || now.Before(w.at) || s.State != wire.ScheduleActive || d.runningLocked(s.ID) {
			continue
		}
		delete(d.retryWaits, s.ID)
		run, err := d.runs.AddTry(s.ID, w.run, wire.ScheduleTry{
			Retry: w.run.Retry + 1, StartedAt: now.UnixMilli(), Result: wire.RunRunning, SessionID: wire.NewSessionID(),
		})
		if err != nil {
			d.logf("daemon: schedule %s runs: %v", s.ID, err)
		}
		start := d.startLocked(s, run)
		if run.Cwd != "" {
			start.s.Cwd = run.Cwd
		}
		d.scheduleChangedLocked(s)
		starts = append(starts, start)
	}
	return starts
}
