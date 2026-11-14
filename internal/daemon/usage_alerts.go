package daemon

import (
	"time"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// The notifications of the limit windows (上限の枠, #1562), reason
// usage_limit: whenever a window's value is taken (recordLimits, whatever
// measured it) and at every usage scan (a reset time passing), the
// windows are compared with what was notified (usage.Alerts): 80% (warn)
// and 100% (error) once per window and reset time, then reset (info) once
// after 100%. A notification has no session: it is emitted as an event
// (the app shows it in-app) and pushed as a schedule's is.

// checkLimitAlerts notifies what the limit windows call for now.
func (d *Daemon) checkLimitAlerts() {
	d.alertMu.Lock()
	defer d.alertMu.Unlock()
	now := d.opts.Now()
	as, err := d.alerts.Check(d.limits.Report(now), now)
	if err != nil {
		d.logf("daemon: usage alerts: %v", err)
	}
	if len(as) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	for _, a := range as {
		d.notifyLimitLocked(a, now)
	}
}

// notifyLimitLocked emits a as a notification event and pushes it when
// notify.reasons.usage_limit is on and no app is in the foreground.
func (d *Daemon) notifyLimitLocked(a usage.Alert, now time.Time) {
	cfg := d.eff.Notify
	n := wire.NotificationData{
		Title:  notify.UsageLimitTitle(cfg.Lang, a.Harness, a.Window),
		Body:   notify.UsageLimitBody(cfg.Lang, a.Threshold, time.UnixMilli(a.ResetsAt), now),
		Level:  wire.LevelInfo,
		Reason: wire.ReasonUsageLimit, Harness: a.Harness, Window: a.Window, ResetsAt: a.ResetsAt, ConfigDir: a.ConfigDir,
	}
	switch a.Threshold {
	case usage.AlertFull:
		n.Level = wire.LevelError
	case usage.AlertNearly:
		n.Level = wire.LevelWarn
	}
	d.emitLocked("", wire.EventNotification, n)
	if !notify.Enabled(cfg) || !notify.Wants(cfg, n) || d.appForegroundLocked() {
		return
	}
	d.digest.Add(n, time.Duration(cfg.DigestWindowMs)*time.Millisecond)
}
