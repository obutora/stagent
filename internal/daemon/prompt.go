package daemon

import "github.com/obutora/stagent/internal/wire"

// Prompt watch. claude does not stop a PermissionRequest hook when its
// prompt is answered (on the PC, in Remote Control or from SSH Term): the
// hook lives until the approved tool finished, and the dialog keeps the
// output going, so no idle→working transition marks the answer either.
// Codex's hook returns before its prompt shows, and nothing tells when the
// prompt is answered: an Esc ends the turn without Stop, and Codex blinks
// its terminal title while the prompt is up, so the output never goes quiet
// before the answer either. The session's holder keeps the screen: while a
// claude or Codex approval of the session is pending the daemon asks it to
// watch for the permission menu (holder.prompt_watch), and the holder
// reports when the menu it saw left the screen (holder.prompt_gone).
// Holders that cannot watch (before 0.4.0, which ignore the notification;
// 0.5.0 and earlier know only claude's menu) or never saw the menu send
// nothing; Stop remains the fallback for those.

// startPromptWatchLocked starts a new watch for ap, a pending approval of
// s. Every approval starts its own watch, so the report of an older watch
// never closes an approval registered after it (the next prompt).
func (d *Daemon) startPromptWatchLocked(s *session, ap *approval) {
	d.promptGens++
	ap.gen, s.promptGen = d.promptGens, d.promptGens
	d.sendPromptWatchLocked(s, true)
}

// endPromptWatchLocked ends s's watch once no watched approval of s is
// pending.
func (d *Daemon) endPromptWatchLocked(s *session) {
	if s.promptGen == 0 {
		return
	}
	for _, ap := range d.approvals {
		if ap.a.SessionID == s.s.ID && ap.gen > 0 {
			return
		}
	}
	d.sendPromptWatchLocked(s, false)
	s.promptGen = 0
}

// sendPromptWatchLocked tells s's holder, if connected, to watch (on) or
// stop watching for the prompt of watch s.promptGen. It goes through the
// connection's outbox: a stuck holder never blocks the daemon.
func (d *Daemon) sendPromptWatchLocked(s *session, on bool) {
	if s.holder == nil || s.holder.c == nil {
		return
	}
	s.holder.outLocked().push(notification(wire.MethodHolderPromptWatch, wire.HolderPromptWatchParams{
		ID: s.s.ID, Gen: s.promptGen, On: on,
	}))
}

// holderPromptGone closes the watched approvals of the session that existed
// when watch p.Gen started; one registered later stays pending.
// resolveLocked clears needs_approval, emits approval_resolved, releases
// the hooks and settles the pushes.
func (d *Daemon) holderPromptGone(cs *connState, p wire.HolderPromptGoneParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.liveSessionLocked(cs, p.ID)
	if s == nil || s.holder != cs || p.Gen <= 0 {
		return
	}
	for _, ap := range d.approvals {
		if ap.a.SessionID == s.s.ID && ap.gen > 0 && ap.gen <= p.Gen {
			d.resolveLocked(ap)
		}
	}
}
