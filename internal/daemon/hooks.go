package daemon

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// approval is a pending permission request; its hook waits on reply.
type approval struct {
	a     wire.Approval
	reply chan wire.HookEventResult // buffered 1
	timer *time.Timer
	done  bool
}

// defaultDenyMessage is what the agent is told when the app denies without
// a message.
const defaultDenyMessage = "Denied from SSH Term"

// hookEvent applies one harness hook event. PermissionRequest answers only
// once the app responds, the approval times out or the hook goes away.
func (d *Daemon) hookEvent(cs *connState, id *int64, p wire.HookEventParams) (any, error) {
	pl := parseHookPayload(p.Payload)
	eff := classifyHook(p.Event, pl)
	now := nowMs()

	d.mu.Lock()
	s := d.hookSessionLocked(p, pl)
	changed := false
	if s != nil {
		changed = d.hookMetaLocked(s, p.Harness, pl)
	}
	label := sessionLabel(p.Harness, pl.Cwd)
	sid := ""
	if s != nil {
		label = sessionLabel(s.s.Harness, s.s.Cwd)
		sid = s.s.ID
	}

	var ap *approval
	switch {
	case eff.approval:
		ap = d.newApprovalLocked(sid, p.Harness, pl, now)
		if s != nil {
			s.track = s.track.hookState(wire.StateNeedsApproval, now)
			changed = d.recomputeLocked(s, false) || changed
		} else {
			d.notifyLocked("", wire.NotificationData{
				Title: label, Body: approvalBody(ap.a.ToolName), Level: wire.LevelInfo, Reason: reasonNeedsApproval,
			}, nil)
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
		} else if reason := sessionlessReason(eff.state); reason != "" {
			body := "Waiting for input"
			if reason == reasonNeedsApproval {
				body = "Needs approval"
			}
			d.notifyLocked("", wire.NotificationData{Title: label, Body: body, Level: wire.LevelInfo, Reason: reason}, nil)
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
		d.notifyLocked(sid, wire.NotificationData{Title: label, Body: "Turn complete", Level: wire.LevelInfo, Reason: reasonTurnComplete}, nil)
	}
	if changed && s != nil {
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
	d.mu.Unlock()

	if ap == nil {
		return wire.HookEventResult{}, nil
	}
	go func() {
		select {
		case res := <-ap.reply:
			cs.c.ReplyResult(id, res, nil)
		case <-cs.c.Context().Done():
			// The hook was killed (harness timeout, user interrupt).
			d.mu.Lock()
			d.resolveLocked(ap, wire.DecisionNone, "cancelled", "")
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

func sessionlessReason(state string) string {
	switch state {
	case wire.StateWaitingInput:
		return reasonWaitingInput
	case wire.StateNeedsApproval:
		return reasonNeedsApproval
	}
	return ""
}

func approvalBody(tool string) string {
	if tool == "" {
		return "Needs approval"
	}
	return "Needs approval: " + tool
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
// conversation id and transcript path. It reports a change.
func (d *Daemon) hookMetaLocked(s *session, harness string, pl hookPayload) bool {
	changed := false
	switch harness {
	case wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp:
		if s.s.Harness != harness {
			s.s.Harness, changed = harness, true
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

// newApprovalLocked registers a pending approval with its timeout.
func (d *Daemon) newApprovalLocked(sessionID, harness string, pl hookPayload, now int64) *approval {
	timeout := time.Duration(d.eff.ApprovalTimeoutSec) * time.Second
	ap := &approval{
		a: wire.Approval{
			RequestID: wire.NewSessionID(),
			SessionID: sessionID,
			Harness:   harness,
			ToolName:  pl.ToolName,
			Summary:   approvalSummary(pl.ToolName, pl.ToolInput),
			CreatedAt: now,
			ExpiresAt: now + timeout.Milliseconds(),
		},
		reply: make(chan wire.HookEventResult, 1),
	}
	d.approvals[ap.a.RequestID] = ap
	ap.timer = time.AfterFunc(timeout, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.resolveLocked(ap, wire.DecisionNone, "timeout", "")
	})
	d.emitLocked(sessionID, wire.EventApprovalRequested, ap.a)
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

// resolveLocked answers an approval's hook once and records the outcome.
// An app decision puts the session back to working (the tool runs or the
// agent continues with the denial) unless other approvals are pending.
func (d *Daemon) resolveLocked(ap *approval, decision, by, message string) {
	if ap.done {
		return
	}
	ap.done = true
	delete(d.approvals, ap.a.RequestID)
	ap.timer.Stop()
	ap.reply <- wire.HookEventResult{Decision: decision, Message: message}
	d.emitLocked(ap.a.SessionID, wire.EventApprovalResolved, wire.ApprovalResolvedData{
		RequestID: ap.a.RequestID, Decision: decision, By: by,
	})
	if by != "app" || d.closed {
		return
	}
	s := d.sessions[ap.a.SessionID]
	if s == nil || s.ended || d.firstApprovalLocked(s.s.ID) != nil {
		return
	}
	s.track = s.track.hookState(wire.StateWorking, nowMs())
	if d.recomputeLocked(s, false) {
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
}

// cancelApprovalsLocked resolves a session's pending approvals without a
// decision: the terminal (or the end of the turn) already settled them.
func (d *Daemon) cancelApprovalsLocked(sessionID string) {
	for _, ap := range d.approvals {
		if ap.a.SessionID == sessionID && sessionID != "" {
			d.resolveLocked(ap, wire.DecisionNone, "cancelled", "")
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

func (d *Daemon) approvalRespond(p wire.ApprovalRespondParams) error {
	if p.Decision != wire.DecisionAllow && p.Decision != wire.DecisionDeny {
		return wire.Errorf(wire.ErrBadRequest, "decision must be allow or deny")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	ap := d.approvals[p.RequestID]
	if ap == nil {
		return wire.Errorf(wire.ErrApprovalClosed, "approval %s is no longer pending", p.RequestID)
	}
	msg := strings.TrimSpace(p.Message)
	if p.Decision == wire.DecisionDeny && msg == "" {
		msg = defaultDenyMessage
	}
	d.resolveLocked(ap, p.Decision, "app", msg)
	return nil
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
