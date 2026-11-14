package usage

import (
	"cmp"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Limit windows (上限の枠): the last value of every window of every
// account (a harness and its configuration directory), whatever measured
// it, kept in <root>/state/usage-limits.json.

// limitsVersion is the version of usage-limits.json; a file of another
// version is read as empty.
const limitsVersion = 1

// sameWindow is how far apart two reset times of one window may be: the
// value is rounded differently from one report to the next.
const sameWindow = 60 * time.Second

// Window is one measurement of a limit window.
type Window struct {
	ID          string  `json:"id"`           // wire.UsageWindow*, or "<minutes>m"
	UsedPercent float64 `json:"used_percent"` // share of the limit used, 0–100
	ResetsAt    int64   `json:"resets_at"`    // unix ms
}

// Limits holds the limit windows. Safe for concurrent use. The file is
// written after mu is released (Report never waits for the disk), from a
// snapshot taken under it; writeMu orders the writes, so an older snapshot
// never replaces a newer one.
type Limits struct {
	path string

	mu       sync.Mutex
	accounts []wire.UsageLimits
	taken    uint64 // snapshots taken (under mu)

	writeMu sync.Mutex
	written uint64 // the newest snapshot written (under writeMu)
}

// limitsSnapshot is the file's content as one change left it: number gen
// of the snapshots taken.
type limitsSnapshot struct {
	gen uint64
	b   []byte
}

type limitsFile struct {
	Version  int                `json:"version"`
	Accounts []wire.UsageLimits `json:"accounts"`
}

// OpenLimits reads the limit windows kept at path; a missing, unreadable
// or other-version file gives none.
func OpenLimits(path string) *Limits {
	l := &Limits{path: path}
	var f limitsFile
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &f) == nil && f.Version == limitsVersion {
		l.accounts = slices.DeleteFunc(f.Accounts, func(a wire.UsageLimits) bool { return a.Harness == "" })
	}
	return l
}

// Measurement is what one source measured for an account.
type Measurement struct {
	Source  string // wire.UsageSource*
	Plan    string // the account's plan, when the source tells it
	Windows []Window
	At      time.Time // when it was measured
}

// Record takes the windows one source measured for the account (h,
// configDir). A window replaces the one kept unless it is the same window
// (resets within sameWindow) measured earlier or with a lower share, or an
// older window: a session that reported last can hold an earlier value,
// and a restarted daemon records the newest rollout event again, however
// old. A window without a reset time (ResetsAt 0, or one kept with only an
// error) is always replaced. A window not among m.Windows keeps its
// value, as does the plan when m has none. A measurement of a source that
// can fail (the private API, omp) clears the account's errors (it
// succeeded); any other keeps them.
// changed is whether a share, a reset time, the plan or an error changed;
// the file is written whenever a value was taken.
func (l *Limits) Record(h, configDir string, m Measurement) (changed bool, err error) {
	var snap limitsSnapshot
	defer l.write(&snap, &err) // after mu is released
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.accountLocked(h, configDir)
	a := &l.accounts[i]
	taken := false
	for _, w := range m.Windows {
		kept := slices.IndexFunc(a.Windows, func(x wire.UsageLimitWindow) bool { return x.Window == w.ID })
		var keptErr *wire.UsageLimitError
		if kept >= 0 {
			k := a.Windows[kept]
			keptErr = k.Error
			d := time.Duration(w.ResetsAt-k.ResetsAt) * time.Millisecond
			timed := w.ResetsAt != 0 && k.ResetsAt != 0
			same := timed && d.Abs() <= sameWindow
			if timed && d < -sameWindow || same && (m.At.UnixMilli() < k.MeasuredAt || k.UsedPercent != nil && w.UsedPercent < *k.UsedPercent) {
				continue
			}
			changed = changed || k.UsedPercent == nil || *k.UsedPercent != w.UsedPercent || k.ResetsAt != w.ResetsAt
		} else {
			changed = true
			a.Windows = append(a.Windows, wire.UsageLimitWindow{Window: w.ID})
			kept = len(a.Windows) - 1
		}
		p := w.UsedPercent
		a.Windows[kept] = wire.UsageLimitWindow{Window: w.ID, UsedPercent: &p, ResetsAt: w.ResetsAt, Source: m.Source, MeasuredAt: m.At.UnixMilli(), Error: keptErr}
		taken = true
	}
	if m.Plan != "" && m.Plan != a.Plan && len(a.Windows) > 0 {
		a.Plan = m.Plan
		changed, taken = true, true
	}
	if (m.Source == wire.UsageSourcePrivateAPI || m.Source == wire.UsageSourceOmpUsage) && clearErrors(a) {
		changed, taken = true, true
	}
	return changed, l.settleLocked(i, taken, &snap)
}

// RecordError keeps e as the last error of every window of the account
// (h, configDir), keeping their values; an account without windows gets
// five_hour and seven_day with only the error. changed is whether a
// window's error code or retry time changed; the file is written either
// way.
func (l *Limits) RecordError(h, configDir string, e wire.UsageLimitError) (changed bool, err error) {
	var snap limitsSnapshot
	defer l.write(&snap, &err)
	l.mu.Lock()
	defer l.mu.Unlock()
	i := l.accountLocked(h, configDir)
	a := &l.accounts[i]
	if len(a.Windows) == 0 {
		for _, id := range []string{wire.UsageWindowFiveHour, wire.UsageWindowSevenDay} {
			a.Windows = append(a.Windows, wire.UsageLimitWindow{Window: id, Source: wire.UsageSourcePrivateAPI})
		}
	}
	for k := range a.Windows {
		w := &a.Windows[k]
		changed = changed || w.Error == nil || w.Error.Code != e.Code || w.Error.RetryAt != e.RetryAt
		e := e
		w.Error = &e
	}
	return changed, l.settleLocked(i, true, &snap)
}

// ClearErrors drops the errors of every account of harness h, and the
// windows that had only an error: the private API was turned off for it.
func (l *Limits) ClearErrors(h string) (changed bool, err error) {
	var snap limitsSnapshot
	defer l.write(&snap, &err)
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.accounts) - 1; i >= 0; i-- {
		if l.accounts[i].Harness == h && clearErrors(&l.accounts[i]) {
			changed = true
			if len(l.accounts[i].Windows) == 0 {
				l.accounts = slices.Delete(l.accounts, i, i+1)
			}
		}
	}
	if !changed {
		return false, nil
	}
	return true, l.snapshotLocked(&snap)
}

// clearErrors drops the errors of a, and its windows with only an error.
func clearErrors(a *wire.UsageLimits) bool {
	n := len(a.Windows)
	cleared := false
	a.Windows = slices.DeleteFunc(a.Windows, func(w wire.UsageLimitWindow) bool { return w.UsedPercent == nil && w.Error != nil })
	for k := range a.Windows {
		if a.Windows[k].Error != nil {
			a.Windows[k].Error, cleared = nil, true
		}
	}
	return cleared || len(a.Windows) != n
}

// accountLocked is the index of the account (h, configDir), added when
// missing.
func (l *Limits) accountLocked(h, configDir string) int {
	i := slices.IndexFunc(l.accounts, func(a wire.UsageLimits) bool { return a.Harness == h && a.ConfigDir == configDir })
	if i < 0 {
		l.accounts = append(l.accounts, wire.UsageLimits{Harness: h, ConfigDir: configDir})
		i = len(l.accounts) - 1
	}
	return i
}

// settleLocked drops account i when it has no window and takes a snapshot
// of the file when something was taken.
func (l *Limits) settleLocked(i int, taken bool, snap *limitsSnapshot) error {
	if len(l.accounts[i].Windows) == 0 {
		l.accounts = slices.Delete(l.accounts, i, i+1)
	}
	if !taken {
		return nil
	}
	return l.snapshotLocked(snap)
}

// snapshotLocked takes the file's content now.
func (l *Limits) snapshotLocked(snap *limitsSnapshot) error {
	b, err := json.Marshal(limitsFile{Version: limitsVersion, Accounts: l.accounts})
	if err != nil {
		return err
	}
	l.taken++
	*snap = limitsSnapshot{gen: l.taken, b: b}
	return nil
}

// write writes snap, when one was taken and no newer one was written,
// setting *err to its error when *err is nil.
func (l *Limits) write(snap *limitsSnapshot, err *error) {
	if snap.b == nil {
		return
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if snap.gen <= l.written {
		return
	}
	if werr := fsutil.WriteFileAtomic(l.path, snap.b); werr != nil {
		*err = cmp.Or(*err, werr)
		return
	}
	l.written = snap.gen
}

// Report is the limit windows as usage.get returns them at now: accounts
// in the registry's harness order, then by configuration directory;
// windows five_hour, seven_day, the weekly windows of one model, the
// others by id, then extra_usage. A window whose reset time has passed
// comes without its share, marked Reset.
func (l *Limits) Report(now time.Time) []wire.UsageLimits {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]wire.UsageLimits, 0, len(l.accounts))
	for _, a := range l.accounts {
		ws := make([]wire.UsageLimitWindow, len(a.Windows))
		for i, w := range a.Windows {
			if w.UsedPercent != nil && w.ResetsAt > 0 && w.ResetsAt <= now.UnixMilli() {
				w.UsedPercent, w.Reset = nil, true
			}
			ws[i] = w
		}
		slices.SortFunc(ws, func(x, y wire.UsageLimitWindow) int {
			return cmp.Or(cmp.Compare(wire.UsageWindowRank(x.Window), wire.UsageWindowRank(y.Window)), cmp.Compare(x.Window, y.Window))
		})
		out = append(out, wire.UsageLimits{Harness: a.Harness, ConfigDir: a.ConfigDir, Plan: a.Plan, Windows: ws})
	}
	slices.SortFunc(out, func(x, y wire.UsageLimits) int {
		return cmp.Or(cmp.Compare(harnessRank(x.Harness), harnessRank(y.Harness)), cmp.Compare(x.ConfigDir, y.ConfigDir))
	})
	return out
}

// harnessRank is the place of harness id in the registry; an unknown one
// comes last.
func harnessRank(id string) int {
	if i := slices.IndexFunc(harness.All, func(h harness.Harness) bool { return h.ID == id }); i >= 0 {
		return i
	}
	return len(harness.All)
}
