package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// omp の記録: when omp is installed, the private API goroutine also asks
// omp for the limit windows of the Claude and Codex accounts it is logged
// in to. `omp usage --history --json` (omp's own snapshots, no call to a
// provider) is read every OmpHistoryEvery; `omp usage --json` (omp calls
// the providers) only when the private API would be called — the app in
// front at most every 15 minutes, the usage screen opened 5 minutes after
// the last (usage.APISchedule). Windows go through recordLimits with
// source omp_usage; a failed live run becomes the error of the windows of
// the accounts omp measured.

// defaultOmpHistoryEvery is how often omp's snapshots are read.
const defaultOmpHistoryEvery = 2 * time.Minute

// ompState is what the goroutine remembers between looks.
type ompState struct {
	live      usage.APISchedule
	historyAt time.Time       // the last read of the snapshots
	measured  map[string]bool // the harnesses omp has told windows of
}

// callOmp runs omp's usage command when due.
func (d *Daemon) callOmp(ctx context.Context, s *ompState, foreground, opened bool) {
	if d.opts.OmpBin == nil {
		return
	}
	now := d.opts.Now()
	historyDue := s.historyAt.IsZero() || now.Sub(s.historyAt) >= d.opts.OmpHistoryEvery
	liveDue := s.live.Due(now, foreground, opened)
	if !historyDue && !liveDue {
		return
	}
	bin := d.opts.OmpBin()
	if bin == "" {
		return
	}
	o := d.opts.Omp
	o.Bin = bin
	if historyDue {
		s.historyAt = now
		ms, aerr := o.History(ctx)
		if ctx.Err() != nil {
			return
		}
		if aerr != nil {
			d.logf("daemon: omp usage --history: %s", aerr.Message)
		}
		d.recordOmp(s, ms)
	}
	if !liveDue {
		return
	}
	ms, aerr := o.Live(ctx)
	if ctx.Err() != nil {
		return
	}
	next := s.live.Done(now, aerr)
	if aerr == nil {
		d.recordOmp(s, ms)
		return
	}
	d.logf("daemon: omp usage: %s (next run %s)", aerr.Message, next.Format(time.RFC3339))
	changed := false
	for _, h := range privateAPIHarnesses {
		if !s.measured[h] {
			continue
		}
		c, err := d.limits.RecordError(h, d.privateAPIAccount(h).Dir, wire.UsageLimitError{
			Code: aerr.Code, Message: aerr.Message, At: now.UnixMilli(), RetryAt: next.UnixMilli(),
		})
		if err != nil {
			d.logf("daemon: usage limits: %v", err)
		}
		changed = changed || c
	}
	if changed {
		d.usageChanged()
	}
}

// recordOmp records what omp told under the accounts the statusLine, the
// rollouts and the private API record under (privateAPIAccount): omp's
// Claude login is taken for the host's Claude account, its Codex login
// for the Codex one.
func (d *Daemon) recordOmp(s *ompState, ms []usage.OmpMeasurement) {
	changed := false
	for _, m := range ms {
		if s.measured == nil {
			s.measured = map[string]bool{}
		}
		s.measured[m.Harness] = true
		changed = d.recordLimits(m.Harness, d.privateAPIAccount(m.Harness).Dir, m.Measurement) || changed
	}
	if changed {
		d.usageChanged()
	}
}

// findOmp is the omp executable: PATH, then the per-user install
// directories (a launchd or systemd daemon's PATH often lacks
// ~/.local/bin); "" when omp is not installed.
func findOmp(home string) string {
	if p, err := exec.LookPath("omp"); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	name := "omp"
	if runtime.GOOS == "windows" {
		name = "omp.exe"
	}
	for _, dir := range paths.ToolDirs(runtime.GOOS, home, os.Getenv) {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0o111 != 0) {
			return p
		}
	}
	return ""
}
