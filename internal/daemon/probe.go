package daemon

import (
	"time"

	"github.com/obutora/stagent/internal/follow"
	"github.com/obutora/stagent/internal/wire"
)

// A session that registered as another program — a shell kept on the host
// — is promoted to an agent by that agent's hooks (hookMetaLocked). Hooks
// do not reliably say when the agent quits, so while such a session is
// promoted the daemon probes its process tree, recognizing agents as
// `stagent follow --session` does (follow.AgentProbe), and returns the
// session to its base harness once the agent no longer runs in front of the
// user.

const (
	defaultProbeInterval = 2 * time.Second
	// unseenProbes: a promoted agent the probe never recognized (an
	// unusual launcher, or one that quit before the first probe) is given
	// up after this many probes.
	unseenProbes = 5
)

// promoted reports whether the session runs as an agent it did not
// register as, and so is probed.
func (s *session) promoted() bool {
	return !s.ended && s.base == wire.HarnessOther && s.s.Harness != wire.HarnessOther
}

// startProbeLocked starts the probe loop unless it runs or there is no
// probe.
func (d *Daemon) startProbeLocked() {
	if d.probing || d.closed || d.opts.AgentProbe == nil {
		return
	}
	d.probing = true
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.probeLoop()
	}()
}

// probeLoop probes the promoted sessions every ProbeInterval and exits
// when none is left.
func (d *Daemon) probeLoop() {
	t := time.NewTicker(d.opts.ProbeInterval)
	defer t.Stop()
	type job struct {
		s       *session
		hookGen int
	}
	lastErr := ""
	for {
		select {
		case <-t.C:
		case <-d.shutdown:
			d.mu.Lock()
			d.probing = false
			d.mu.Unlock()
			return
		}
		d.mu.Lock()
		var jobs []job
		var checks []follow.SessionAgent
		for _, s := range d.sessions {
			if s.promoted() {
				jobs = append(jobs, job{s, s.hookGen})
				checks = append(checks, follow.SessionAgent{PID: s.s.PID, Harness: s.s.Harness})
			}
		}
		if len(jobs) == 0 {
			d.probing = false
			d.mu.Unlock()
			return
		}
		d.mu.Unlock()

		running, err := d.opts.AgentProbe(checks)
		if err != nil || len(running) != len(checks) {
			if err != nil && err.Error() != lastErr {
				lastErr = err.Error()
				d.logf("daemon: agent probe: %v", err)
			}
			continue
		}
		lastErr = ""
		d.mu.Lock()
		for i, j := range jobs {
			s := j.s
			// A hook since the snapshot, a re-registration with another
			// program or the end of the session makes the result stale.
			if d.sessions[s.s.ID] != s || !s.promoted() || s.hookGen != j.hookGen ||
				s.s.Harness != checks[i].Harness || s.s.PID != checks[i].PID {
				continue
			}
			if running[i] {
				s.agentSeen = true
				continue
			}
			if s.missed++; s.agentSeen || s.missed >= unseenProbes {
				d.revertHarnessLocked(s)
			}
		}
		d.mu.Unlock()
	}
}

// revertHarnessLocked returns a promoted session to its base harness: the
// agent's conversation, transcript, last message, hook-derived state and
// pending approvals go with it.
func (d *Daemon) revertHarnessLocked(s *session) {
	s.s.Harness = s.base
	s.s.ConversationID, s.s.TranscriptPath, s.s.LastMessage = "", "", ""
	s.agentSeen, s.missed = false, 0
	d.cancelApprovalsLocked(s.s.ID)
	s.track = s.track.hookClear()
	d.recomputeLocked(s, false)
	d.broadcastLocked(wire.NotifySessionUpdated, s.s)
}
