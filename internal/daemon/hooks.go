package daemon

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// approval is a pending permission request of a stagent session. Nothing
// in stagent answers it: the program's own screen does (the PC, Claude's
// Remote Control, or SSH Term typing the option's key into that screen).
type approval struct {
	a wire.Approval
	// hold is non-nil while a claude hook waits on the approval, and is
	// closed when the approval closes so the hook returns (no decision).
	hold chan struct{}
	// gen is the prompt watch started with a held approval: a
	// holder.prompt_gone of this watch or a later one closes it.
	gen  int64
	done bool
}

// hookEvent applies one harness hook event. A PermissionRequest of a claude
// running in a stagent session keeps its hook open, with no time limit,
// until the approval closes: the session's holder saw claude's permission
// menu leave the screen (it was answered somewhere), claude stopped the
// hook (the approved tool finished, or claude was interrupted) or the turn
// moved on. claude shows the same prompt on its screen meanwhile, so nobody
// waits on the hook. Every other event returns at once.
func (d *Daemon) hookEvent(cs *connState, id *int64, p wire.HookEventParams) (any, error) {
	pl := parseHookPayload(p.Payload)
	eff := classifyHook(p.Event, pl)
	now := nowMs()

	d.mu.Lock()
	s := d.hookSessionLocked(p, pl)
	changed := false
	if s != nil {
		s.hookGen++
		s.remoteControl = p.RemoteControl
		changed = d.hookMetaLocked(s, p.Harness, pl)
	}
	d.noteUnwrappedLocked(p, pl, eff, s != nil, now)
	label := sessionLabel(p.Harness, pl.Cwd)
	if s != nil {
		label = sessionLabel(s.s.Harness, s.s.Cwd)
	}
	lang := d.eff.Notify.Lang

	var ap *approval
	switch {
	case eff.approval:
		// A request outside stagent sessions (an IDE, `claude -p`, the SDK)
		// is neither registered nor pushed: nothing here can answer it.
		if s != nil {
			ap = d.newApprovalLocked(s, p.Harness, pl, now)
			s.track = s.track.hookState(wire.StateNeedsApproval, now)
			changed = d.recomputeLocked(s, false) || changed
		}
	case eff.state != "":
		if s != nil {
			// omp's extension sends the prompt as message.
			if prompt := firstNonBlank(pl.Prompt, pl.Message); eff.state == wire.StateWorking && prompt != "" {
				s.s.LastMessage = clipLine(prompt, 120)
				changed = true
			}
			s.track = s.track.hookState(eff.state, now)
			changed = d.recomputeLocked(s, eff.state == wire.StateWorking) || changed
		} else if eff.state == wire.StateWaitingInput {
			// Outside stagent sessions: in-app only (notifyLocked).
			d.notifyLocked(nil, wire.NotificationData{Title: label, Body: notify.Text(lang, notify.PhraseWaitingInput), Level: wire.LevelInfo, Reason: reasonWaitingInput}, nil)
		}
	case eff.clear:
		if s != nil {
			s.track = s.track.hookClear()
			changed = d.recomputeLocked(s, false) || changed
		}
	}
	if eff.turnComplete {
		if s != nil {
			d.cancelApprovalsLocked(s.s.ID) // the turn ended without them
			if m := firstNonBlank(pl.LastAssistantMessage, pl.Message); m != "" {
				s.s.LastMessage = clipLine(m, 120)
				changed = true
			}
		}
		d.notifyLocked(s, wire.NotificationData{Title: label, Body: notify.Text(lang, notify.PhraseTurnComplete), Level: wire.LevelInfo, Reason: reasonTurnComplete}, nil)
	}
	if changed && s != nil {
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
	d.mu.Unlock()

	if ap == nil || ap.hold == nil {
		return struct{}{}, nil
	}
	go func() {
		select {
		case <-ap.hold:
			cs.c.ReplyResult(id, struct{}{}, nil)
		case <-cs.c.Context().Done():
			// claude stopped the hook: the approved tool finished, or
			// claude was interrupted. (Answering the prompt does not stop
			// it; the holder's prompt watch reports that.)
			d.mu.Lock()
			d.resolveLocked(ap)
			d.mu.Unlock()
		}
	}()
	return rpc.Async, nil
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// hookSessionLocked finds the PTY session a hook belongs to: by
// $STAGENT_SESSION_ID, else by the harness conversation id.
func (d *Daemon) hookSessionLocked(p wire.HookEventParams, pl hookPayload) *session {
	if p.SessionID != "" {
		if s := d.sessions[p.SessionID]; s != nil && !s.ended {
			return s
		}
	}
	if pl.SessionID == "" {
		return nil
	}
	for _, s := range d.sessions {
		if !s.ended && s.s.ConversationID == pl.SessionID && (p.Harness == "" || s.s.Harness == p.Harness) {
			return s
		}
	}
	return nil
}

// hookMetaLocked records what a hook tells about the session: harness,
// conversation id and transcript path. It reports a change. A session that
// registered as another program (a shell) is promoted to the hook's agent;
// the probe returns it to its base harness once that agent is gone.
func (d *Daemon) hookMetaLocked(s *session, harness string, pl hookPayload) bool {
	changed := false
	switch harness {
	case wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp:
		if s.s.Harness != harness {
			s.s.Harness, changed = harness, true
			if s.base == wire.HarnessOther {
				s.agentSeen, s.missed = false, 0
				d.startProbeLocked()
			}
		}
	}
	if pl.SessionID != "" && pl.SessionID != s.s.ConversationID {
		s.s.ConversationID, changed = pl.SessionID, true
	}
	if pl.TranscriptPath != "" && pl.TranscriptPath != s.s.TranscriptPath {
		s.s.TranscriptPath, changed = pl.TranscriptPath, true
	}
	return changed
}

// newApprovalLocked registers a pending approval of s. A claude hook waits
// on it, and s's holder watches for claude's permission menu meanwhile
// (prompt.go); Codex shows its prompt only once the hook returned, so its
// hook is answered at once and the approval closes on output activity or
// Stop.
func (d *Daemon) newApprovalLocked(s *session, harness string, pl hookPayload, now int64) *approval {
	ap := &approval{
		a: wire.Approval{
			RequestID: wire.NewSessionID(),
			SessionID: s.s.ID,
			Harness:   harness,
			ToolName:  pl.ToolName,
			Summary:   approvalSummary(pl.ToolName, pl.ToolInput),
			CreatedAt: now,
		},
	}
	d.approvals[ap.a.RequestID] = ap
	d.emitLocked(s.s.ID, wire.EventApprovalRequested, ap.a)
	if harness == wire.HarnessClaude {
		ap.hold = make(chan struct{})
		d.startPromptWatchLocked(s, ap)
	}
	return ap
}

// approvalSummary describes a permission request in one line, e.g.
// "Bash: rm -rf build".
func approvalSummary(tool string, input json.RawMessage) string {
	s := transcript.Summarize(input)
	switch {
	case tool == "":
		return s
	case s == "":
		return tool
	}
	return tool + ": " + s
}

// resolveLocked closes an approval once: it releases a waiting hook and
// records the outcome. When no other approval of the session is pending,
// the session's pushes settle (the request was answered or the turn moved
// on) and the hook-derived needs_approval goes too, so the session shows
// what its terminal shows again.
func (d *Daemon) resolveLocked(ap *approval) {
	if ap.done {
		return
	}
	ap.done = true
	delete(d.approvals, ap.a.RequestID)
	if ap.hold != nil {
		close(ap.hold)
	}
	d.emitLocked(ap.a.SessionID, wire.EventApprovalResolved, wire.ApprovalResolvedData{
		RequestID: ap.a.RequestID, By: "cancelled",
	})
	if d.closed {
		return
	}
	s := d.sessions[ap.a.SessionID]
	if s == nil || s.ended {
		return
	}
	if ap.hold != nil {
		d.endPromptWatchLocked(s)
	}
	if d.firstApprovalLocked(s.s.ID) != nil {
		return
	}
	d.settlePushLocked(s)
	if s.track.Hook != wire.StateNeedsApproval {
		return
	}
	s.track = s.track.hookClear()
	if d.recomputeLocked(s, false) {
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
}

// cancelApprovalsLocked closes a session's pending approvals: the terminal
// (or the end of the turn) already settled them.
func (d *Daemon) cancelApprovalsLocked(sessionID string) {
	for _, ap := range d.approvals {
		if ap.a.SessionID == sessionID && sessionID != "" {
			d.resolveLocked(ap)
		}
	}
}

func (d *Daemon) firstApprovalLocked(sessionID string) *approval {
	var first *approval
	for _, ap := range d.approvals {
		if ap.a.SessionID == sessionID && (first == nil || ap.a.CreatedAt < first.a.CreatedAt) {
			first = ap
		}
	}
	return first
}

func (d *Daemon) approvalList() []wire.Approval {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.approvalListLocked()
}

func (d *Daemon) approvalListLocked() []wire.Approval {
	out := make([]wire.Approval, 0, len(d.approvals))
	for _, ap := range d.approvals {
		out = append(out, ap.a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}
