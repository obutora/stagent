package daemon

import (
	"context"
	"path/filepath"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// Antigravity usage: when Antigravity CLI is installed, the private API
// goroutine also asks agy for the limit windows of the account it is
// logged in to, `agy -p /usage --output-format json`, only when the
// private API would be called — the app in front at most every 15
// minutes, the usage screen opened 5 minutes after the last
// (usage.APISchedule) — whether or not usage.private_api is on. Windows
// go through recordLimits with source antigravity_usage under the
// account (antigravity, Antigravity CLI's home); a failed run becomes the
// error of its windows once the account was measured, and is only logged
// before.

// agyState is what the goroutine remembers between looks.
type agyState struct {
	sched usage.APISchedule
}

// callAgy runs agy's /usage when due.
func (d *Daemon) callAgy(ctx context.Context, s *agyState, foreground, opened bool) {
	if d.opts.AgyBin == nil {
		return
	}
	now := d.opts.Now()
	if !s.sched.Due(now, foreground, opened) {
		return
	}
	bin := d.opts.AgyBin()
	if bin == "" {
		return
	}
	a := d.opts.Agy
	a.Bin = bin
	m, aerr := a.Measure(ctx, d.opts.Now)
	if ctx.Err() != nil {
		return
	}
	next := s.sched.Done(now, aerr)
	dir := d.agyAccountDir()
	if aerr == nil {
		if d.recordLimits(wire.HarnessAntigravity, dir, m) {
			d.usageChanged()
		}
		return
	}
	d.logf("daemon: agy /usage: %s (next run %s)", aerr.Message, next.Format(time.RFC3339))
	// An account never measured (agy logged in with an API key, whose
	// /usage fails, or not at all) gets no windows with only an error.
	if !d.limits.Has(wire.HarnessAntigravity, dir) {
		return
	}
	changed, err := d.limits.RecordError(wire.HarnessAntigravity, dir, wire.UsageLimitError{
		Code: aerr.Code, Message: aerr.Message, At: now.UnixMilli(), RetryAt: next.UnixMilli(),
	})
	if err != nil {
		d.logf("daemon: usage limits: %v", err)
	}
	if changed {
		d.usageChanged()
	}
}

// agyAccountDir is the account Antigravity's windows are recorded under:
// Antigravity CLI's home (~/.gemini/antigravity-cli) as a clean path.
func (d *Daemon) agyAccountDir() string {
	return filepath.Clean(harness.AntigravityHome(d.opts.Layout.Home))
}
