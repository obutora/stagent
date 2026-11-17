package daemon

import (
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// qwenDaemon: Qwen Code sends Claude Code's hooks, with three differences.
//
//   - It shows its permission menu only once its PermissionRequest hook
//     returned, and runs that hook only for a call it is about to ask
//     about (one its rules allow runs no hook): the hook registers the
//     approval and returns at once (harness.Harness.HookHeldForPrompt is
//     false), and the holder watches the screen for the menu, as for
//     Codex.
//   - Its question tool, ask_user_question, goes through its permission
//     flow like claude's AskUserQuestion: the question shows once the
//     PermissionRequest hook naming it returned, and the permission_prompt
//     notification that follows is about the question.
//   - It sends the idle_prompt notification whenever a reply of the model
//     ends: before the tool calls the reply asks for (in the same
//     millisecond as their PreToolUse, either first) and some 50 ms after
//     Stop. The one that counts comes alone: after the user turned a
//     permission menu down (Esc), with no Stop (replyIdle).
var qwenDaemon = daemonHarness{
	watchPrompt:    true,
	questionShown:  hookPermissionRequest,
	idleEveryReply: true,
}

// replyIdleGrace: an idle_prompt of a harness with idleEveryReply counts
// only when no other hook of the session comes this close to it, before
// or after.
const replyIdleGrace = 500 * time.Millisecond

// replyIdleLocked handles an idle_prompt notification of a harness that
// sends one with every reply (daemonHarness.idleEveryReply): it waits
// for input only when no other hook of the session came within the grace
// before it (a tool call's PreToolUse, Stop) nor comes within the grace
// after it (a PreToolUse that came second: hookEvent stops the timer, and
// hookGen voids one already running); then the session waits for input
// as on claude's. Outside stagent sessions it is not told: nothing there
// can wait for the hook that would follow. Times are Options.Now's and the
// timer Options.AfterFunc's.
func (d *Daemon) replyIdleLocked(s *session, now int64) {
	if s == nil || now-s.hookAt < replyIdleGrace.Milliseconds() {
		return
	}
	gen := s.hookGen
	s.replyIdle = d.opts.AfterFunc(replyIdleGrace, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || s.ended || s.hookGen != gen {
			return
		}
		s.replyIdle = nil
		s.track = s.track.hookState(wire.StateWaitingInput, d.opts.Now().UnixMilli())
		if d.recomputeLocked(s, false) {
			d.sessionChangedLocked(s)
		}
	})
}
