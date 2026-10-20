package daemon

import (
	"errors"
	"os"
	"slices"

	"github.com/obutora/stagent/internal/schedule"
	"github.com/obutora/stagent/internal/scrollback"
	"github.com/obutora/stagent/internal/wire"
)

// keptRunOutputs is how many of each schedule's newest runs (those with a
// session; each with all its tries) keep their output past
// scrollback_days.
const keptRunOutputs = 5

// scheduleOutput answers a page of the output of a run's try (its newest,
// or the one of p.SessionID) from its session's scrollback on disk (gone
// when retention deleted it or the try had no session).
func (d *Daemon) scheduleOutput(p wire.ScheduleOutputParams) (wire.ScheduleOutputResult, error) {
	const method = wire.MethodScheduleOutput
	d.mu.Lock()
	if d.scheduleLocked(p.ID) == nil {
		d.mu.Unlock()
		return wire.ScheduleOutputResult{}, notFound(method, p.ID)
	}
	runs, err := d.runs.List(p.ID)
	d.mu.Unlock()
	if err != nil {
		return wire.ScheduleOutputResult{}, wire.Errorf(wire.ErrInternal, "schedule runs: %v", err)
	}
	i := slices.IndexFunc(runs, func(r wire.ScheduleRun) bool { return r.RunID == p.RunID })
	if i < 0 {
		return wire.ScheduleOutputResult{}, wire.Errorf(wire.ErrNotFound, "%s: no run %q of schedule %q", method, p.RunID, p.ID)
	}
	sid := runs[i].SessionID
	if p.SessionID != "" {
		if !runs[i].HasSession(p.SessionID) {
			return wire.ScheduleOutputResult{}, wire.Errorf(wire.ErrNotFound, "%s: no try of run %q in session %q", method, p.RunID, p.SessionID)
		}
		sid = p.SessionID
	}
	gone := wire.ScheduleOutputResult{ScrollbackResult: wire.ScrollbackResult{Data: []byte{}}, Gone: true}
	if sid == "" {
		return gone, nil
	}
	data, start, end, first, err := scrollback.ReadDir(d.layout.SessionDataDir(sid), p.Before, p.MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return gone, nil
	}
	if err != nil {
		return wire.ScheduleOutputResult{}, wire.Errorf(wire.ErrInternal, "%s: %v", method, err)
	}
	return wire.ScheduleOutputResult{ScrollbackResult: wire.ScrollbackResult{Data: data, Start: start, End: end, First: first}}, nil
}

// runSummary is the end of session sid's output as a run's summary ("" when
// there is none).
func (d *Daemon) runSummary(sid string) string {
	if sid == "" {
		return ""
	}
	data, start, _, _, err := scrollback.ReadDir(d.layout.SessionDataDir(sid), 0, schedule.SummaryRead)
	if err != nil {
		return ""
	}
	return schedule.OutputSummary(data, start == 0)
}

// keptRunOutputsLocked are the sessions whose output retention keeps past
// scrollback_days: those of all the tries of each schedule's newest
// keptRunOutputs runs with one.
func (d *Daemon) keptRunOutputsLocked() map[string]bool {
	kept := map[string]bool{}
	for _, s := range d.schedules {
		runs, err := d.runs.List(s.ID)
		if err != nil {
			d.logf("daemon: schedule %s runs: %v", s.ID, err)
			continue
		}
		n := 0
		for _, r := range runs {
			had := false
			keep := func(sid string) {
				if sid != "" {
					kept[sid], had = true, true
				}
			}
			keep(r.SessionID)
			for _, t := range r.Tries {
				keep(t.SessionID)
			}
			if !had {
				continue
			}
			if n++; n == keptRunOutputs {
				break
			}
		}
	}
	return kept
}
