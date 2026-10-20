package daemon

import (
	"time"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/wire"
)

// The notifications of schedules (#1084), reason schedule: the last result
// of each time (its retries done), as the schedule's Notify says. A
// problem is a run that failed, timed out, had actions refused, could not
// be judged or was skipped for an overlap, and the schedule stopping after
// failures; running late and a once time passing while paused are only
// recorded. Under problems (the default), while problems go on only the
// first and the stop are notified, then the first success as fixed. The
// notification has no session: its push replaces the schedule's previous
// one (notify.ReplaceKey) and links to the run (r=) or, for a stop, a fix
// or a skip, the schedule alone.

const reasonSchedule = "schedule"

// runProblem reports whether r, the last run of a time, is a problem.
func runProblem(r wire.ScheduleRun) bool {
	switch r.Result {
	case wire.RunFailed, wire.RunTimedOut, wire.RunDenied, wire.RunUndetermined, wire.RunSkippedOverlap:
		return true
	}
	return r.Unjudged
}

// runBody is the fixed phrase of r's notification in lang; fixed: r is the
// success after a problem. Nothing of the run's output goes in.
func runBody(lang, harness string, r wire.ScheduleRun, fixed bool) string {
	switch {
	case r.Result == wire.RunFailed && r.OutsideGUI && r.AuthFailed:
		return notify.Text(lang, notify.PhraseScheduleGUILogin, harness)
	case r.Result == wire.RunTimedOut:
		return notify.Text(lang, notify.PhraseScheduleTimedOut)
	case r.Result == wire.RunFailed && r.ExitCode != nil:
		return notify.Text(lang, notify.PhraseScheduleFailedCode, *r.ExitCode)
	case r.Result == wire.RunFailed:
		return notify.Text(lang, notify.PhraseScheduleFailed)
	case r.Result == wire.RunDenied:
		return notify.Text(lang, notify.PhraseScheduleDenied, r.Denials)
	case r.Result == wire.RunSkippedOverlap:
		return notify.Text(lang, notify.PhraseScheduleSkipped)
	case r.Result == wire.RunUndetermined || r.Unjudged:
		return notify.Text(lang, notify.PhraseScheduleUndetermined)
	case fixed:
		return notify.Text(lang, notify.PhraseScheduleFixed)
	}
	return notify.Text(lang, notify.PhraseScheduleSucceeded)
}

// notifyRunLocked follows r, the last run of a time of s.
func (d *Daemon) notifyRunLocked(s *wire.Schedule, r wire.ScheduleRun) {
	problem := runProblem(r)
	fixed := !problem && s.ProblemSent
	var send bool
	switch s.Notify {
	case wire.ScheduleNotifyAlways:
		send = true
	case wire.ScheduleNotifyOff:
	default:
		send = problem && !s.ProblemSent || fixed
	}
	if s.ProblemSent != problem {
		s.ProblemSent = problem
		d.saveSchedulesOrLogLocked(s)
	}
	if !send {
		return
	}
	n := wire.NotificationData{Level: wire.LevelInfo, Body: runBody(d.eff.Notify.Lang, s.Harness, r, fixed)}
	if problem {
		n.Level = wire.LevelWarn
	}
	// A fix and a skip open the schedule (its history), not the run.
	if !fixed && r.Result != wire.RunSkippedOverlap {
		n.RunID = r.RunID
	}
	d.notifyScheduleLocked(s, n, r.SessionID)
}

// notifyAutoStoppedLocked: s stopped after failures (saved by the caller).
func (d *Daemon) notifyAutoStoppedLocked(s *wire.Schedule) {
	s.ProblemSent = true
	if s.Notify == wire.ScheduleNotifyOff {
		return
	}
	d.notifyScheduleLocked(s, wire.NotificationData{
		Level: wire.LevelWarn, Body: notify.Text(d.eff.Notify.Lang, notify.PhraseScheduleAutoStopped, s.FailedInRow),
	}, "")
}

// notifyScheduleLocked emits n about s as a notification event (the app
// shows it in-app when its 定期実行 is on) and pushes it as notifyLocked
// would: when notify.reasons.schedule is on, no app is in the foreground
// and nobody is present at the run's session (sessionID, "" for none).
func (d *Daemon) notifyScheduleLocked(s *wire.Schedule, n wire.NotificationData, sessionID string) {
	n.Title, n.Reason, n.ScheduleID = s.Name, reasonSchedule, s.ID
	d.emitLocked("", wire.EventNotification, n)
	cfg := d.eff.Notify
	if !notify.Enabled(cfg) || !notify.Wants(cfg, n) || d.appForegroundLocked() {
		return
	}
	if ss := d.sessions[sessionID]; ss != nil && d.presentLocked(ss, time.Now()) {
		return
	}
	d.digest.Add(n, time.Duration(cfg.DigestWindowMs)*time.Millisecond)
}
