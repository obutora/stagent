package usage

import (
	"encoding/json"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/wire"
)

// The notifications of the limit windows (上限の枠): a window passing 80%
// and 100% is notified once each per window (its reset time), and a
// window that reached 100% once more when it resets. What was notified is
// kept in <root>/state/usage-alerts.json, so a restart notifies nothing
// again.

// alertsVersion is the version of usage-alerts.json; a file of another
// version is read as empty.
const alertsVersion = 1

// Alert thresholds: the share (percent) a window passes.
const (
	AlertNearly = 80
	AlertFull   = 100
)

// AlertFresh is how old a measured value may be to be compared: a value
// measured before (a statusLine that stopped, a rollout written earlier)
// notifies nothing.
const AlertFresh = time.Hour

// Alert is one notification to send about a limit window.
type Alert struct {
	Harness, ConfigDir, Window string
	// ResetsAt is the window's reset time (unix ms) as first seen: the
	// notifications of one window share it (their replace key).
	ResetsAt int64
	// Threshold is AlertNearly or AlertFull; 0 for the window having reset.
	Threshold int
}

// Alerts remembers what was notified of each window. Safe for concurrent
// use.
type Alerts struct {
	path string

	mu      sync.Mutex
	windows []alertWindow
}

// alertWindow is what was notified of the current window of one window
// id of one account.
type alertWindow struct {
	Harness   string `json:"harness"`
	ConfigDir string `json:"config_dir"`
	Window    string `json:"window"`
	ResetsAt  int64  `json:"resets_at"` // as first seen (Alert.ResetsAt)
	Seen      int64  `json:"seen"`      // the latest reset time reported
	Sent      int    `json:"sent,omitempty"`
	ResetSent bool   `json:"reset_sent,omitempty"`
}

type alertsFile struct {
	Version int           `json:"version"`
	Windows []alertWindow `json:"windows"`
}

// OpenAlerts reads what was notified, kept at path; a missing, unreadable
// or other-version file gives nothing notified.
func OpenAlerts(path string) *Alerts {
	a := &Alerts{path: path}
	var f alertsFile
	if b, err := os.ReadFile(path); err == nil && json.Unmarshal(b, &f) == nil && f.Version == alertsVersion {
		a.windows = f.Windows
	}
	return a
}

// Check compares the limit windows (Limits.Report at now) with what was
// notified and returns what to notify now, oldest event first, recording
// it. A window whose share was measured more than AlertFresh before now
// is not compared; a window reporting a reset time later than the one
// seen (by more than sameWindow) is a new window. A window that reached
// AlertFull notifies its reset once, when its reset time passed or a new
// window came. The file is written whenever something changed; err is
// about writing it (what to notify is returned all the same).
func (a *Alerts) Check(accounts []wire.UsageLimits, now time.Time) (out []Alert, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	dirty := false
	for _, acc := range accounts {
		for _, w := range acc.Windows {
			if w.UsedPercent == nil && !w.Reset || w.ResetsAt <= 0 {
				continue // nothing measured (an error only)
			}
			i := slices.IndexFunc(a.windows, func(x alertWindow) bool {
				return x.Harness == acc.Harness && x.ConfigDir == acc.ConfigDir && x.Window == w.Window
			})
			if i < 0 {
				if w.Reset {
					continue
				}
				a.windows = append(a.windows, alertWindow{Harness: acc.Harness, ConfigDir: acc.ConfigDir, Window: w.Window, ResetsAt: w.ResetsAt, Seen: w.ResetsAt})
				i, dirty = len(a.windows)-1, true
			}
			s := &a.windows[i]
			later := time.Duration(w.ResetsAt-s.Seen) * time.Millisecond
			if later < -sameWindow {
				continue // an older window than the one notified
			}
			newWindow := later > sameWindow
			if (newWindow || w.Reset) && s.Sent >= AlertFull && !s.ResetSent {
				out = append(out, Alert{Harness: s.Harness, ConfigDir: s.ConfigDir, Window: s.Window, ResetsAt: s.ResetsAt})
				s.ResetSent, dirty = true, true
			}
			switch {
			case newWindow:
				*s = alertWindow{Harness: s.Harness, ConfigDir: s.ConfigDir, Window: s.Window, ResetsAt: w.ResetsAt, Seen: w.ResetsAt}
				dirty = true
			case later > 0:
				s.Seen, dirty = w.ResetsAt, true
			}
			if w.Reset || w.UsedPercent == nil || now.Sub(time.UnixMilli(w.MeasuredAt)) > AlertFresh {
				continue
			}
			th := 0
			switch p := *w.UsedPercent; {
			case p >= AlertFull:
				th = AlertFull
			case p >= AlertNearly:
				th = AlertNearly
			}
			if th > s.Sent {
				out = append(out, Alert{Harness: s.Harness, ConfigDir: s.ConfigDir, Window: s.Window, ResetsAt: s.ResetsAt, Threshold: th})
				s.Sent, dirty = th, true
			}
		}
	}
	if !dirty {
		return out, nil
	}
	b, err := json.Marshal(alertsFile{Version: alertsVersion, Windows: a.windows})
	if err == nil {
		err = fsutil.WriteFileAtomic(a.path, b)
	}
	return out, err
}
