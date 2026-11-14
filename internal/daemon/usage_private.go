package daemon

import (
	"context"
	"os"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// Private API (非公開 API): when usage.private_api is on for claude or
// codex, a goroutine calls the harness's undocumented usage API for its
// limit windows, only while the app is in front and watching
// (presence.set) or when the usage screen is opened (usage.get
// refresh_limits), as usage.APISchedule allows. What it measures goes
// through recordLimits with source private_api; a failure becomes each
// window's last error.

const defaultPrivateAPITick = 15 * time.Second

// privateAPIHarnesses are the harnesses with a private API, in the order
// they are called.
var privateAPIHarnesses = []string{wire.HarnessClaude, wire.HarnessCodex}

// runPrivateAPI looks every PrivateAPITick, and when nudged, for an
// account whose private API is due, until shutdown.
func (d *Daemon) runPrivateAPI() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-d.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	t := time.NewTicker(d.opts.PrivateAPITick)
	defer t.Stop()
	sched := map[string]*usage.APISchedule{}
	omp := &ompState{}
	for {
		d.mu.Lock()
		foreground := d.appForegroundLocked()
		opened := d.apiOpened
		d.apiOpened = false
		d.mu.Unlock()
		d.callPrivateAPI(ctx, sched, foreground, opened)
		d.callOmp(ctx, omp, foreground, opened)
		select {
		case <-d.shutdown:
			return
		case <-t.C:
		case <-d.apiNudge:
		}
	}
}

// callPrivateAPI calls the private API of each account that is on and due.
func (d *Daemon) callPrivateAPI(ctx context.Context, sched map[string]*usage.APISchedule, foreground, opened bool) {
	d.mu.Lock()
	on := d.eff.Usage.PrivateAPI
	d.mu.Unlock()
	for _, h := range privateAPIHarnesses {
		if !on.On(h) {
			continue
		}
		s := sched[h]
		if s == nil {
			s = &usage.APISchedule{}
			sched[h] = s
		}
		now := d.opts.Now()
		if !s.Due(now, foreground, opened) {
			continue
		}
		a := d.privateAPIAccount(h)
		m, aerr := d.opts.PrivateAPI.Measure(ctx, a, now)
		if ctx.Err() != nil {
			return
		}
		next := s.Done(now, aerr)
		if aerr != nil {
			d.logf("daemon: private API: %s (next call %s)", aerr.Message, next.Format(time.RFC3339))
		}
		if d.recordPrivateAPI(h, a.Dir, m, aerr, now, next) {
			d.usageChanged()
		}
	}
}

// recordPrivateAPI records what a call of h's private API brought — the
// windows measured, or the failure aerr with the next call at next —
// unless the API was turned off for h while it was in flight
// (privateAPIConfigured then dropped the errors: none may come back).
// It reports whether a value changed.
func (d *Daemon) recordPrivateAPI(h, dir string, m usage.Measurement, aerr *usage.APIError, now, next time.Time) bool {
	d.apiMu.Lock()
	defer d.apiMu.Unlock()
	d.mu.Lock()
	on := d.eff.Usage.PrivateAPI.On(h)
	d.mu.Unlock()
	if !on {
		return false
	}
	if aerr == nil {
		return d.recordLimits(h, dir, m)
	}
	changed, err := d.limits.RecordError(h, dir, wire.UsageLimitError{
		Code: aerr.Code, Message: aerr.Message, At: now.UnixMilli(), RetryAt: next.UnixMilli(),
	})
	if err != nil {
		d.logf("daemon: usage limits: %v", err)
	}
	return changed
}

// privateAPIAccount is the account h's private API measures: Claude's
// configuration directory (harness.ClaudeConfigDir: $CLAUDE_CONFIG_DIR
// cleaned, the variable as set naming its Keychain item, else ~/.claude),
// Codex's $CODEX_HOME or ~/.codex — the accounts the statusLine and the
// rollouts record under.
func (d *Daemon) privateAPIAccount(h string) usage.Account {
	if h == wire.HarnessClaude {
		dir, env := harness.ClaudeConfigDir(d.layout.Home, os.Getenv)
		return usage.Account{Harness: h, Dir: dir, ConfigEnv: env}
	}
	return usage.Account{Harness: h, Dir: d.roots[h]}
}

// refreshLimits asks for the calls the opened usage screen allows.
func (d *Daemon) refreshLimits() {
	d.mu.Lock()
	d.apiOpened = true
	d.mu.Unlock()
	d.nudgePrivateAPI()
}

func (d *Daemon) nudgePrivateAPI() {
	select {
	case d.apiNudge <- struct{}{}:
	default:
	}
}

// privateAPIConfigured follows a change of usage.private_api: a harness
// turned off loses its errors, one turned on is looked at now.
func (d *Daemon) privateAPIConfigured(before, after wire.UsagePrivateAPI) {
	changed, nudge := false, false
	d.apiMu.Lock()
	for _, h := range privateAPIHarnesses {
		switch {
		case before.On(h) && !after.On(h):
			c, err := d.limits.ClearErrors(h)
			if err != nil {
				d.logf("daemon: usage limits: %v", err)
			}
			changed = changed || c
		case !before.On(h) && after.On(h):
			nudge = true
		}
	}
	d.apiMu.Unlock()
	if nudge {
		d.nudgePrivateAPI()
	}
	if changed {
		d.usageChanged()
	}
}
