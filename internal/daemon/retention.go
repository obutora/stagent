package daemon

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/obutora/stagent/internal/ipc"
)

const (
	// sweepInterval is the longest time between two retention sweeps.
	sweepInterval = time.Hour
	// sweepAfterRemoval batches sweeps triggered by sessions leaving the list.
	sweepAfterRemoval = 30 * time.Second
)

// runSweep runs the start-up sweep; later sweeps are scheduled by it and by
// session removals.
func (d *Daemon) runSweep() {
	d.sweepNow()
	d.mu.Lock()
	d.scheduleSweepLocked(sweepInterval)
	d.mu.Unlock()
}

// scheduleSweepLocked (re)arms the sweep timer to fire after delay.
func (d *Daemon) scheduleSweepLocked(delay time.Duration) {
	if d.closed {
		return
	}
	if d.sweep != nil {
		d.sweep.Stop()
	}
	d.sweep = time.AfterFunc(delay, func() {
		d.sweepNow()
		d.mu.Lock()
		d.scheduleSweepLocked(sweepInterval)
		d.mu.Unlock()
	})
}

type dataDir struct {
	id     string
	path   string
	newest time.Time
	size   int64
}

// sweepNow deletes the scrollback of sessions that are no longer live:
// older than ScrollbackDays, then oldest first while the total exceeds
// ScrollbackTotalMiB. Registered sessions and holders that still answer on
// their address (e.g. not yet re-registered after a daemon restart) are
// never touched. A chat message from before the restart whose holder
// stopped answering without registering is marked resolved.
func (d *Daemon) sweepNow() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	r := d.eff.Retention
	live := make(map[string]bool, len(d.sessions))
	for id := range d.sessions {
		live[id] = true
	}
	notifyCfg := d.eff.Notify
	unregistered := slices.Collect(maps.Keys(d.chatSent))
	d.mu.Unlock()

	for _, id := range unregistered {
		if d.holderAnswers(id) {
			continue
		}
		d.mu.Lock()
		pending := d.chatSent[id]
		delete(d.chatSent, id)
		d.mu.Unlock()
		if pending {
			d.sender.ResolveChat(notifyCfg, id)
		}
	}

	entries, err := os.ReadDir(d.layout.DataDir)
	if err != nil {
		return
	}
	var dirs []dataDir
	var total int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dd := dataDir{id: e.Name(), path: filepath.Join(d.layout.DataDir, e.Name())}
		filepath.WalkDir(dd.path, func(p string, de fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := de.Info(); err == nil {
				if !de.IsDir() {
					dd.size += info.Size()
				}
				if info.ModTime().After(dd.newest) {
					dd.newest = info.ModTime()
				}
			}
			return nil
		})
		total += dd.size
		if !live[dd.id] {
			dirs = append(dirs, dd)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].newest.Before(dirs[j].newest) })
	cutoff := time.Now().Add(-time.Duration(r.ScrollbackDays) * 24 * time.Hour)
	limit := int64(r.ScrollbackTotalMiB) << 20
	for _, dd := range dirs {
		if !dd.newest.Before(cutoff) && total <= limit {
			continue
		}
		if d.holderAnswers(dd.id) {
			continue
		}
		if err := os.RemoveAll(dd.path); err != nil {
			d.logf("daemon: retention: %v", err)
			continue
		}
		total -= dd.size
	}
}

// holderAnswers reports whether a holder serves session id right now.
func (d *Daemon) holderAnswers(id string) bool {
	c, err := ipc.Dial(d.layout.HolderAddr(id), 200*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
