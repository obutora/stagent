package daemon

import "github.com/obutora/stagent/internal/wire"

// Prompt watch. claude does not stop a PermissionRequest hook when its
// prompt is answered (on the PC, in Remote Control or from SSH Term): the
// hook lives until the approved tool finished, and the dialog keeps the
// output going, so no idle→working transition marks the answer either.
// Codex's hook returns before its prompt shows, and nothing tells when the
// prompt is answered: an Esc ends the turn without Stop, and Codex blinks
// its terminal title while the prompt is up, so the output never goes quiet
// before the answer either. Copilot CLI's and Qwen Code's hooks return
// before their menus show too. The session's holder keeps the screen: while
// a claude, Codex, Copilot CLI or Qwen Code approval of the session is
// pending the daemon asks it to
// watch for the permission menu of that harness (holder.prompt_watch), and
// the holder reports when the menu it saw left the screen
// (holder.prompt_gone). Holders that cannot watch (before 0.4.0, which
// ignore the notification; 0.5.0 and earlier know only claude's menu;
// 0.15.0 and earlier know no Copilot CLI menu, 0.21.1 and earlier no Qwen
// Code menu) or never saw the menu send
// nothing; Stop remains the fallback for those.
//
// Antigravity CLI sends no hook at all when its menu shows
// (daemonHarness.menuOnScreen): only its PreToolUse, before it checks the
// call's permission, which its rules may allow without asking. That check
// starts a watch with shown (startCheckWatchLocked): the holder reports
// when the menu comes on the screen (holder.prompt_shown), which registers
// the approval on that same watch, and then when it leaves, as for the
// others. Holders that know no Antigravity CLI menu (0.22.1 and earlier)
// report neither: its sessions show no approval there.

// startPromptWatchLocked starts a new watch for ap, a pending approval of
// s. Every approval starts its own watch, so the report of an older watch
// never closes an approval registered after it (the next prompt).
func (d *Daemon) startPromptWatchLocked(s *session, ap *approval) {
	d.promptGens++
	ap.gen, s.promptGen, s.promptHarness = d.promptGens, d.promptGens, ap.a.Harness
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

// sendPromptWatchLocked tells s's holder, if connected, to watch (on) for
// the menu of s.promptHarness or stop watching for the prompt of watch
// s.promptGen. It goes through the connection's outbox: a stuck holder
// never blocks the daemon.
func (d *Daemon) sendPromptWatchLocked(s *session, on bool) {
	if s.holder == nil || s.holder.c == nil {
		return
	}
	p := wire.HolderPromptWatchParams{ID: s.s.ID, Gen: s.promptGen, On: on}
	if on {
		p.Harness = s.promptHarness
		p.Shown = s.checkGen != 0 && s.checkGen == s.promptGen
	}
	s.holder.outLocked().push(notification(wire.MethodHolderPromptWatch, p))
}

// startCheckWatchLocked starts a watch for the menu the permission check
// harnessID just made of a call may bring (holder.prompt_watch with
// shown): no approval is registered until the holder reports the menu on
// the screen (holderPromptShown). The next check's watch replaces it: the
// holder drops a watch that saw no menu when a newer one starts.
func (d *Daemon) startCheckWatchLocked(s *session, harnessID string) {
	d.promptGens++
	s.checkGen, s.promptGen, s.promptHarness = d.promptGens, d.promptGens, harnessID
	d.sendPromptWatchLocked(s, true)
}

// endCheckWatchLocked ends s's check watch: the check went by without its
// menu (the call ran, or the turn moved on or ended).
func (d *Daemon) endCheckWatchLocked(s *session) {
	if s.checkGen == 0 {
		return
	}
	s.checkGen = 0
	d.endPromptWatchLocked(s)
}

// holderPromptShown registers the approval of the menu the session's
// latest permission check brought, which came on the screen (watch p.Gen):
// named by the tool of that check, watched for its leaving by the same
// watch. The session needs approval, as when a hook registers one. A
// report of an older check's watch is about a check gone by.
func (d *Daemon) holderPromptShown(cs *connState, p wire.HolderPromptGoneParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.liveSessionLocked(cs, p.ID)
	if s == nil || s.holder != cs || p.Gen <= 0 || p.Gen != s.checkGen {
		return
	}
	s.checkGen = 0
	now := d.opts.Now().UnixMilli()
	d.newApprovalLocked(s, s.promptHarness, hookPayload{}, now, p.Gen)
	s.track = s.track.hookState(wire.StateNeedsApproval, now)
	d.recomputeLocked(s, false)
	d.sessionChangedLocked(s)
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
