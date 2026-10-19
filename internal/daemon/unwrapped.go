package daemon

import (
	"runtime"
	"sort"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Agents started interactively without a stagent session (GLOSSARY: この
// ホストの agent に出ない起動) are recorded from their hooks, one per
// conversation, in memory only: the list says why an agent is not shown
// now, it is not a history. A record goes away at the conversation's
// SessionEnd (Claude Code; omp's session_shutdown), UnwrappedTTL after its
// last hook, or with the daemon. Recording changes nothing else a
// sessionless hook does.

const (
	defaultUnwrappedTTL = 12 * time.Hour
	// unwrappedPushEvery bounds how often a record's last_activity_at alone
	// is pushed to watchers: the app shows it in minutes.
	unwrappedPushEvery = time.Minute
)

type unwrappedRec struct {
	u        wire.UnwrappedLaunch
	pushedAt int64
	expire   *time.Timer
}

// unwrappedReason says why a sessionless hook's agent has no session, ""
// when it is not recorded: a non-interactive run (Claude Code's sdk-*
// entry points; a Codex conversation of `codex exec`, the SDK or a
// subagent; a codex / omp whose terminal is known to be missing or whose
// command line asks for a batch run) or a program that is not an agent
// harness. Claude Code tells its entry point and Codex its originator
// (daemonHarness.launch); without them (older versions, unreadable
// rollout) they are judged like the others.
//
// On a Windows host (windows) the terminal is never known: every other
// launch counts. One from an SSH session has its own reason (the wrapper
// does not wrap there), and a launch without the wrapper's marker is told
// apart by a shell above the harness: without one, a program started it
// without a terminal (e.g. over WMI) rather than a terminal opened before
// the setup. An app that runs the agent in a PowerShell reading its
// profile (Orca) gets the wrapper; the Codex desktop app counts as ide.
func unwrappedReason(p wire.HookEventParams, windows bool) string {
	if !harness.Known(p.Harness) {
		return ""
	}
	kind := launchUnknown
	if launch := daemonHarnesses[p.Harness].launch; launch != nil {
		kind = launch(p)
	}
	switch kind {
	case launchBatch:
		return ""
	case launchIDE:
		return wire.UnwrappedIDE
	case launchUnknown:
		if (p.ParentTTY != nil && !*p.ParentTTY) || p.ParentBatch {
			return ""
		}
	}
	if windows && p.SSH {
		return wire.UnwrappedSSH
	}
	if p.ShellWrapper {
		return wire.UnwrappedBypassed
	}
	if windows && p.TerminalAncestor != nil && !*p.TerminalAncestor {
		return wire.UnwrappedNoTerminal
	}
	return wire.UnwrappedOldTerminal
}

// launchKind is what a harness reports about how it was started
// (daemonHarness.launch).
type launchKind int

const (
	// launchUnknown: judged by the terminal and the command line.
	launchUnknown launchKind = iota
	// launchTerminal: an interactive start, whatever the terminal facts.
	launchTerminal
	// launchBatch: a non-interactive run, not recorded.
	launchBatch
	// launchIDE: an app or editor extension that runs the harness.
	launchIDE
)

// noteUnwrappedLocked records the agent of a hook that found no session;
// it forgets the conversation at its SessionEnd and once a stagent session
// carries it (found: a resumed conversation).
func (d *Daemon) noteUnwrappedLocked(p wire.HookEventParams, pl hookPayload, eff hookEffect, found bool, now int64) {
	conv := pl.SessionID
	if conv == "" {
		return // nothing to tell conversations apart
	}
	key := p.Harness + "\x00" + conv
	r := d.unwrapped[key]
	if found || eff.event == hookSessionEnd {
		if r != nil {
			d.dropUnwrappedLocked(key, r)
		}
		return
	}
	reason := ""
	if p.SessionID == "" { // a $STAGENT_SESSION_ID means stagent started it
		reason = unwrappedReason(p, runtime.GOOS == "windows")
	}
	if reason == "" {
		return
	}
	ttl := d.opts.UnwrappedTTL
	if r == nil {
		r = &unwrappedRec{u: wire.UnwrappedLaunch{ConversationID: conv, Harness: p.Harness, FirstSeenAt: now}}
		d.unwrapped[key] = r
		r.expire = time.AfterFunc(ttl, func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.unwrapped[key] == r {
				d.dropUnwrappedLocked(key, r)
			}
		})
	} else {
		r.expire.Reset(ttl)
	}
	changed := r.pushedAt == 0 || r.u.Reason != reason || (pl.Cwd != "" && pl.Cwd != r.u.Cwd) ||
		now-r.pushedAt >= unwrappedPushEvery.Milliseconds()
	r.u.Reason, r.u.LastActivityAt = reason, now
	if pl.Cwd != "" {
		r.u.Cwd = pl.Cwd
	}
	if changed {
		r.pushedAt = now
		d.broadcastLocked(wire.NotifyUnwrappedUpdated, wire.UnwrappedUpdatedParams{Unwrapped: d.unwrappedListLocked()})
	}
}

func (d *Daemon) dropUnwrappedLocked(key string, r *unwrappedRec) {
	r.expire.Stop()
	delete(d.unwrapped, key)
	d.broadcastLocked(wire.NotifyUnwrappedUpdated, wire.UnwrappedUpdatedParams{Unwrapped: d.unwrappedListLocked()})
}

// unwrappedListLocked lists the records, newest last_activity_at first.
func (d *Daemon) unwrappedListLocked() []wire.UnwrappedLaunch {
	out := make([]wire.UnwrappedLaunch, 0, len(d.unwrapped))
	for _, r := range d.unwrapped {
		out = append(out, r.u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActivityAt != out[j].LastActivityAt {
			return out[i].LastActivityAt > out[j].LastActivityAt
		}
		return out[i].ConversationID < out[j].ConversationID
	})
	return out
}
