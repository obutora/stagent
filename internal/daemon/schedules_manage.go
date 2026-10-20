package daemon

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/schedule"
	"github.com/obutora/stagent/internal/wire"
)

// schedule.update, schedule.remove, schedule.pause, schedule.resume and
// schedule.run_now; schedule.changed tells the watchers each change of a
// schedule or of its newest run. Two apps changing one schedule: the last
// change wins (they are applied in turn under Daemon.mu).

const (
	// A run schedule.remove stops (stop_run) gets runStopHangupWait after
	// hangup, then runStopKillWait after kill.
	runStopHangupWait = 5 * time.Second
	runStopKillWait   = 3 * time.Second
	runStopPoll       = 50 * time.Millisecond
)

// scheduleChange serves the methods that change a schedule. schedule.remove
// with stop_run answers once the run stopped, off the connection's handler.
func (d *Daemon) scheduleChange(cs *connState, m *wire.Msg) (any, error) {
	switch m.Method {
	case wire.MethodScheduleUpdate:
		var p wire.ScheduleUpdateParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return d.scheduleUpdate(p)
	case wire.MethodScheduleRemove:
		var p wire.ScheduleRemoveParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		if !p.StopRun {
			return d.scheduleRemove(p.ID, false)
		}
		id := m.ID
		go func() {
			res, err := d.scheduleRemove(p.ID, true)
			cs.c.ReplyResult(id, res, err)
		}()
		return rpc.Async, nil
	}
	var p wire.ScheduleRef
	if err := rpc.Decode(m, &p); err != nil {
		return nil, err
	}
	switch m.Method {
	case wire.MethodSchedulePause:
		return d.scheduleSetState(m.Method, p.ID, wire.SchedulePaused)
	case wire.MethodScheduleResume:
		return d.scheduleSetState(m.Method, p.ID, wire.ScheduleActive)
	default: // wire.MethodScheduleRunNow
		return d.scheduleRunNow(p.ID)
	}
}

func notFound(method, id string) error {
	return wire.Errorf(wire.ErrNotFound, "%s: no schedule %q", method, id)
}

// scheduleChangedLocked tells the watchers s as it is now and returns its
// entry.
func (d *Daemon) scheduleChangedLocked(s *wire.Schedule) wire.ScheduleEntry {
	e := d.entryLocked(s)
	d.broadcastLocked(wire.NotifyScheduleChanged, wire.ScheduleChanged{ID: s.ID, Entry: &e})
	return e
}

// scheduleIDChangedLocked is scheduleChangedLocked of schedule id, if it
// is still there.
func (d *Daemon) scheduleIDChangedLocked(id string) {
	if s := d.scheduleLocked(id); s != nil {
		d.scheduleChangedLocked(s)
	}
}

// runningLocked: a run of schedule id has not ended.
func (d *Daemon) runningLocked(id string) bool {
	for _, p := range d.pendingRuns {
		if p.scheduleID == id {
			return true
		}
	}
	return false
}

// skipOverlapLocked records that s's time at came while its previous run
// was still running.
func (d *Daemon) skipOverlapLocked(s *wire.Schedule, at, now time.Time) {
	run := newRun(s, at, now, wire.RunTriggerScheduled, wire.RunSkippedOverlap)
	d.addRunLocked(s, run)
	d.notifyRunLocked(s, run)
	d.scheduleChangedLocked(s)
}

// scheduleUpdate replaces what a schedule runs and when. A running run
// goes on as it started; the next one runs the new schedule. A time that
// came and was not compared yet is dealt with first, as the schedule was.
func (d *Daemon) scheduleUpdate(p wire.ScheduleUpdateParams) (wire.ScheduleEntry, error) {
	const method = wire.MethodScheduleUpdate
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
	d.mu.Lock()
	defer d.mu.Unlock()
	cur := d.scheduleLocked(p.ID)
	if cur == nil {
		return wire.ScheduleEntry{}, notFound(method, p.ID)
	}
	now := d.now()
	if start, ok := d.dueLocked(cur, now); ok {
		go d.startRun(start)
	}
	prev := *cur
	s.ID, s.State, s.CreatedAt = cur.ID, cur.State, cur.CreatedAt
	s.FailedInRow, s.AutoStopped = cur.FailedInRow, cur.AutoStopped
	s.ProblemSent = cur.ProblemSent
	// The times before the update are not made up for.
	s.LastAt = now.UnixMilli()
	// The members this version does not know stay unless the update
	// carries them.
	extra := maps.Clone(cur.Extra)
	if len(s.Extra) > 0 {
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		maps.Copy(extra, s.Extra)
	}
	s.Extra = extra
	*cur = s
	if err := d.saveSchedulesLocked(); err != nil {
		*cur = prev
		return wire.ScheduleEntry{}, wire.Errorf(wire.ErrInternal, "schedules: %v", err)
	}
	d.planLocked(cur, now)
	return d.scheduleChangedLocked(cur), nil
}

// scheduleSetState pauses (its times pass without a run) or resumes (from
// its next time on, the times passed meanwhile are not made up for) a
// schedule.
func (d *Daemon) scheduleSetState(method, id, state string) (wire.ScheduleEntry, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.scheduleLocked(id)
	if s == nil {
		return wire.ScheduleEntry{}, notFound(method, id)
	}
	if s.State != state {
		prev := *s
		s.State = state
		if state == wire.ScheduleActive {
			// A fresh count of failures; the times passed while it was
			// paused are not made up for.
			s.FailedInRow, s.AutoStopped = 0, false
			s.LastAt = d.now().UnixMilli()
		}
		if err := d.saveSchedulesLocked(); err != nil {
			*s = prev
			return wire.ScheduleEntry{}, wire.Errorf(wire.ErrInternal, "schedules: %v", err)
		}
		if state == wire.SchedulePaused {
			// The retry it waits to run is dropped: the failure is its
			// time's last result.
			d.dropRetryLocked(s)
		}
		d.planLocked(s, d.now())
	}
	return d.scheduleChangedLocked(s), nil
}

// scheduleRunNow starts a run of a schedule now, paused or not, unless its
// previous run is still running (busy).
func (d *Daemon) scheduleRunNow(id string) (wire.ScheduleRun, error) {
	const method = wire.MethodScheduleRunNow
	d.mu.Lock()
	s := d.scheduleLocked(id)
	switch {
	case s == nil:
		d.mu.Unlock()
		return wire.ScheduleRun{}, notFound(method, id)
	case d.runningLocked(id):
		d.mu.Unlock()
		return wire.ScheduleRun{}, wire.Errorf(wire.ErrBusy, "%s: the previous run of %q is still running", method, id)
	case s.Kind != wire.ScheduleKindCommand && s.Kind != wire.ScheduleKindAgent:
		d.mu.Unlock()
		return wire.ScheduleRun{}, wire.Errorf(wire.ErrUnsupported, "%s: this stagent cannot run a %q schedule", method, s.Kind)
	}
	d.dropRetryLocked(s) // run_now comes before the retry s waits to run
	now := d.now()
	start := d.addRunLocked(s, newRun(s, now, now, wire.RunTriggerNow, wire.RunRunning))
	d.scheduleChangedLocked(s)
	d.mu.Unlock()
	go d.startRun(start)
	return start.run, nil
}

// scheduleRemove removes a schedule and its runs. With stopRun its running
// run's session is stopped first (busy when it did not stop); without, the
// run goes on and its end is no longer recorded.
func (d *Daemon) scheduleRemove(id string, stopRun bool) (struct{}, error) {
	const method = wire.MethodScheduleRemove
	d.mu.Lock()
	if d.scheduleLocked(id) == nil {
		d.mu.Unlock()
		return struct{}{}, notFound(method, id)
	}
	if stopRun && d.runningLocked(id) {
		delete(d.schedDue, id) // no new run while this one stops
		d.markRemovingLocked(id, true)
		d.mu.Unlock()
		stopped := d.stopRuns(id)
		d.mu.Lock()
		s := d.scheduleLocked(id)
		if s == nil { // removed meanwhile
			d.mu.Unlock()
			return struct{}{}, nil
		}
		if !stopped {
			d.markRemovingLocked(id, false) // it goes on, as the schedule's
			d.planLocked(s, d.now())
			d.mu.Unlock()
			return struct{}{}, wire.Errorf(wire.ErrBusy, "%s: the run of %q did not stop", method, id)
		}
	}
	defer d.mu.Unlock()
	i := slices.IndexFunc(d.schedules, func(s *wire.Schedule) bool { return s.ID == id })
	s := d.schedules[i]
	d.schedules = slices.Delete(d.schedules, i, i+1)
	if err := d.saveSchedulesLocked(); err != nil {
		d.schedules = slices.Insert(d.schedules, i, s)
		d.planLocked(s, d.now())
		return struct{}{}, wire.Errorf(wire.ErrInternal, "schedules: %v", err)
	}
	delete(d.schedDue, id)
	delete(d.retryWaits, id)
	for runID, p := range d.pendingRuns {
		if p.scheduleID == id {
			delete(d.pendingRuns, runID)
		}
	}
	if err := d.runs.Remove(id); err != nil {
		d.logf("daemon: schedule %s runs: %v", id, err)
	}
	d.broadcastLocked(wire.NotifyScheduleChanged, wire.ScheduleChanged{ID: id})
	return struct{}{}, nil
}

// markRemovingLocked marks schedule id's running runs as stopped for its
// removal (or not any more, when they did not stop).
func (d *Daemon) markRemovingLocked(id string, removing bool) {
	for _, p := range d.pendingRuns {
		if p.scheduleID == id {
			p.removing = removing
		}
	}
}

// stopRuns hangs up the sessions of schedule id's running runs, kills them
// when they are still running runStopHangupWait later, and tells whether
// they ended (their ends are recorded) within runStopKillWait after that.
func (d *Daemon) stopRuns(id string) bool {
	sessions := func() []string {
		d.mu.Lock()
		defer d.mu.Unlock()
		var ids []string
		for _, p := range d.pendingRuns {
			if p.scheduleID == id {
				ids = append(ids, p.run.SessionID)
			}
		}
		return ids
	}
	await := func(wait time.Duration) bool {
		deadline := time.Now().Add(wait)
		for {
			if len(sessions()) == 0 {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
			select {
			case <-d.shutdown:
				return false
			case <-time.After(runStopPoll):
			}
		}
	}
	d.signalSessions(sessions(), wire.SignalHangup)
	if await(runStopHangupWait) {
		return true
	}
	d.signalSessions(sessions(), wire.SignalKill)
	return await(runStopKillWait)
}

// signalSessions sends sig to the sessions ids through their holders.
func (d *Daemon) signalSessions(ids []string, sig string) {
	for _, id := range ids {
		conn, err := ipc.Dial(d.layout.HolderAddr(id), time.Second)
		if err != nil {
			d.logf("daemon: stop schedule run %s: %v", id, err)
			continue
		}
		c := rpc.NewClient(conn, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: sig}, nil); err != nil {
			d.logf("daemon: stop schedule run %s: %v", id, err)
		}
		cancel()
		c.Close()
	}
}
