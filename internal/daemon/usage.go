package daemon

import (
	"path/filepath"
	"time"

	"github.com/obutora/stagent/internal/install"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// Usage (使用量): a goroutine scans the transcripts in the background
// (usage.Scanner, which keeps its own lock), so usage.get answers from
// what was counted without waiting for file reads and no other method
// waits for the scan.

const (
	defaultUsageScanInterval = 10 * time.Second
	defaultUsageChangedEvery = 30 * time.Second
)

// runUsage scans at once, then every UsageScanInterval or when usage.get
// asks, until shutdown.
func (d *Daemon) runUsage() {
	t := time.NewTicker(d.opts.UsageScanInterval)
	defer t.Stop()
	var rolloutAt int64 // the event time of the codex limit windows last recorded
	for {
		st, err := d.usage.Scan(d.shutdown)
		if err != nil {
			d.logf("daemon: usage scan: %v", err)
		}
		changed := st.Changed
		if rl, ok := d.usage.RateLimits(wire.HarnessCodex); ok && rl.At > rolloutAt {
			rolloutAt = rl.At
			changed = d.recordLimits(wire.HarnessCodex, d.roots[wire.HarnessCodex], usage.Measurement{
				Source: wire.UsageSourceCodexRollout, Plan: rl.Plan, Windows: rl.Windows, At: time.UnixMilli(rl.At),
			}) || changed
		}
		if changed {
			d.usageChanged()
		}
		d.checkLimitAlerts() // a reset time may have passed
		select {
		case <-d.shutdown:
			return
		case <-t.C:
		case <-d.usageNudge:
		}
	}
}

// usageGet answers usage.get and asks for a scan, so the next
// usage.changed brings what the transcripts gained since the last one.
func (d *Daemon) usageGet(p wire.UsageGetParams) (wire.UsageGetResult, error) {
	select {
	case d.usageNudge <- struct{}{}:
	default:
	}
	r, err := d.usage.Report(p)
	if err == nil {
		r.Limits = d.limits.Report(d.opts.Now())
	}
	return r, err
}

// Bounds of usage.report, which coding agents' process trees may send
// (ADR 0004): it runs under claude.
const (
	maxReportPath       = 4096     // config_dir, transcript_path (bytes)
	maxReportSession    = 256      // session_id (bytes)
	maxReportRateLimits = 16 << 10 // rate_limits (bytes of JSON)
	// maxReportReset is how far from now a reported reset time may be:
	// the longest window is a week.
	maxReportReset = 8 * 24 * time.Hour
)

// usageReport takes the limit windows a statusLine relay read and sends
// usage.changed when one changed. Coding agents may send it, so it is
// taken only for a configuration directory the manifest records the relay
// in (a clean absolute path), within the bounds above, with every window's
// share 0–100 and reset time within maxReportReset of now; anything else
// changes nothing (bad_request).
func (d *Daemon) usageReport(p wire.UsageReportParams) error {
	dir := p.ConfigDir
	switch {
	case len(dir) > maxReportPath || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir:
		return wire.Errorf(wire.ErrBadRequest, "usage.report: config_dir must be a clean absolute path")
	case len(p.TranscriptPath) > maxReportPath || len(p.SessionID) > maxReportSession || len(p.RateLimits) > maxReportRateLimits:
		return wire.Errorf(wire.ErrBadRequest, "usage.report: a value is too long")
	case !install.StatusLineRelayed(d.layout.Manifest, dir):
		return wire.Errorf(wire.ErrBadRequest, "usage.report: no statusLine relay is installed in %s", dir)
	}
	now := d.opts.Now()
	ws := usage.ClaudeRateLimits(p.RateLimits, now)
	for _, w := range ws {
		if w.UsedPercent > 100 || time.UnixMilli(w.ResetsAt).Sub(now).Abs() > maxReportReset {
			return wire.Errorf(wire.ErrBadRequest, "usage.report: window %s out of range", w.ID)
		}
	}
	if len(ws) == 0 {
		return nil
	}
	if d.recordLimits(wire.HarnessClaude, dir, usage.Measurement{Source: wire.UsageSourceStatusLine, Windows: ws, At: now}) {
		d.usageChanged()
	}
	return nil
}

// recordLimits keeps what was measured for the account (h, configDir),
// notifies what the windows call for (usage_alerts.go) and reports
// whether a value changed. Every source records through it.
func (d *Daemon) recordLimits(h, configDir string, m usage.Measurement) bool {
	changed, err := d.limits.Record(h, configDir, m)
	if err != nil {
		d.logf("daemon: usage limits: %v", err)
	}
	d.checkLimitAlerts()
	return changed
}

// usageChanged sends usage.changed to the watchers, at most once per
// UsageChangedEvery: a change within that time is sent when it ends.
func (d *Daemon) usageChanged() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.usageTimer != nil {
		return
	}
	wait := d.opts.UsageChangedEvery - time.Since(d.usageSent)
	if wait <= 0 {
		d.sendUsageChangedLocked()
		return
	}
	d.usageTimer = time.AfterFunc(wait, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.usageTimer = nil
		if !d.closed {
			d.sendUsageChangedLocked()
		}
	})
}

func (d *Daemon) sendUsageChangedLocked() {
	d.usageSent = time.Now()
	done, at := d.usage.ScannedAt()
	d.broadcastLocked(wire.NotifyUsageChanged, wire.UsageChanged{Scanning: !done, ScannedAt: at})
}
