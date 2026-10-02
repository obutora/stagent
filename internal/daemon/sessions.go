package daemon

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// session is one registered PTY session.
type session struct {
	s      wire.Session
	track  stateTrack
	holder *connState // current holder connection, nil while disconnected
	ended  bool

	gone   *time.Timer // holder connection lost, waiting for re-registration
	remove *time.Timer // ended, lingering in the list
	// pushedAt is when holderUpdate last broadcast this session; activity-only
	// changes are held back until activityPushInterval has passed.
	pushedAt int64

	// Debounced state_changed: logged is the state the last event recorded.
	logged       string
	debounce     *time.Timer
	debounceGen  int
	pendingSince time.Time

	lastTerminal time.Time // rate limit of holder.notify
}

func (s *session) stopTimers() {
	for _, t := range []*time.Timer{s.gone, s.remove, s.debounce} {
		if t != nil {
			t.Stop()
		}
	}
	s.gone, s.remove, s.debounce = nil, nil, nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

// holderRegister adds a session, or re-attaches a holder that reconnected
// (to this daemon or after a daemon restart).
func (d *Daemon) holderRegister(cs *connState, ws wire.Session) (wire.HolderRegisterResult, error) {
	if ws.ID == "" {
		return wire.HolderRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "session id required")
	}
	if ws.Harness == "" {
		ws.Harness = wire.DetectHarness(ws.Command)
	}
	if ws.Mode == "" {
		ws.Mode = wire.ModePassthrough
	}
	now := nowMs()
	d.mu.Lock()
	defer d.mu.Unlock()
	if cs.holderID != "" && cs.holderID != ws.ID {
		return wire.HolderRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "connection already registered %s", cs.holderID)
	}
	cs.holderID = ws.ID
	res := wire.HolderRegisterResult{IdleAfterMs: d.eff.IdleAfterMs}

	if s := d.sessions[ws.ID]; s != nil && !s.ended {
		if s.gone != nil {
			s.gone.Stop()
			s.gone = nil
		}
		s.holder = cs
		// Hook-derived facts survive a holder reconnect.
		if ws.ConversationID == "" {
			ws.ConversationID = s.s.ConversationID
		}
		if ws.TranscriptPath == "" {
			ws.TranscriptPath = s.s.TranscriptPath
		}
		if ws.LastMessage == "" {
			ws.LastMessage = s.s.LastMessage
		}
		if ws.Harness == wire.HarnessOther {
			ws.Harness = s.s.Harness
		}
		state, source := s.s.State, s.s.StateSource
		s.s = ws
		s.s.State, s.s.StateSource = state, source
		s.track = s.track.holderReport(ws.State, ws.StateSource, now)
		d.recomputeLocked(s, true)
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
		return res, nil
	}
	if old := d.sessions[ws.ID]; old != nil {
		old.stopTimers()
	}
	s := &session{s: ws, holder: cs}
	s.track = stateTrack{}.holderReport(ws.State, ws.StateSource, now)
	s.s.State, s.s.StateSource = s.track.merged()
	s.logged = s.s.State
	d.sessions[ws.ID] = s
	// A holder re-registering with a restarted daemon already has its
	// session_started in the log.
	known := d.log.Any(func(e *wire.Event) bool {
		return e.SessionID == ws.ID && e.Kind == wire.EventSessionStarted
	})
	if !known {
		d.emitLocked(ws.ID, wire.EventSessionStarted, wire.SessionStartedData{
			Harness: ws.Harness, Command: ws.Command, Cwd: ws.Cwd, Mode: ws.Mode,
		})
	}
	d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	return res, nil
}

// liveSessionLocked returns the registered, not ended session id (or the
// connection's own session when id is empty).
func (d *Daemon) liveSessionLocked(cs *connState, id string) *session {
	if id == "" {
		id = cs.holderID
	}
	s := d.sessions[id]
	if s == nil || s.ended {
		return nil
	}
	return s
}

func (d *Daemon) holderUpdate(cs *connState, p wire.SessionPatch) {
	now := nowMs()
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.liveSessionLocked(cs, p.ID)
	if s == nil {
		return
	}
	changed, activity := false, false
	if p.Title != nil && *p.Title != s.s.Title {
		s.s.Title, changed = *p.Title, true
	}
	if p.LastActivityAt != nil && *p.LastActivityAt != s.s.LastActivityAt {
		s.s.LastActivityAt, activity = *p.LastActivityAt, true
	}
	if p.Cols != nil && *p.Cols != s.s.Cols {
		s.s.Cols, changed = *p.Cols, true
	}
	if p.Rows != nil && *p.Rows != s.s.Rows {
		s.s.Rows, changed = *p.Rows, true
	}
	if p.Mode != nil && *p.Mode != s.s.Mode {
		s.s.Mode, changed = *p.Mode, true
	}
	if p.State != nil {
		source := ""
		if p.StateSource != nil {
			source = *p.StateSource
		}
		s.track = s.track.holderReport(*p.State, source, now)
		if d.recomputeLocked(s, true) {
			changed = true
		}
	}
	// Busy agents report activity every second. Pushing each one to every
	// watcher would make the app's list traffic grow with the number of
	// sessions even when nothing is attached, so a bare timestamp change
	// rides along with the next real change or goes out after the interval.
	if changed || (activity && now-s.pushedAt >= activityPushInterval.Milliseconds()) {
		s.pushedAt = now
		d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	}
}

// activityPushInterval bounds how often a session whose only change is
// last_activity_at is pushed to watchers.
const activityPushInterval = 15 * time.Second

// holderNotify turns a notification the program emitted (OSC 9/99/777,
// BEL) into a notification event, at most one per debounce interval.
func (d *Daemon) holderNotify(cs *connState, p wire.HolderNotifyParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.liveSessionLocked(cs, p.ID)
	if s == nil {
		return
	}
	now := time.Now()
	if now.Sub(s.lastTerminal) < time.Duration(d.eff.Notify.DebounceMs)*time.Millisecond {
		return
	}
	s.lastTerminal = now
	body := strings.TrimSpace(p.Body)
	if t := strings.TrimSpace(p.Title); t != "" {
		if body != "" {
			body = t + ": " + body
		} else {
			body = t
		}
	}
	if body == "" {
		body = "Bell"
	}
	n := wire.NotificationData{Title: sessionLabel(s.s.Harness, s.s.Cwd), Body: clipLine(body, 200), Level: wire.LevelInfo, Reason: reasonTerminal}
	// The program's text stays in the app; pushes carry a fixed phrase.
	push := n
	push.Body = "Terminal notification"
	d.notifyLocked(s.s.ID, n, &push)
}

func (d *Daemon) holderEnded(cs *connState, p wire.ClosedParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.liveSessionLocked(cs, p.ID); s != nil {
		d.endLocked(s, p.ExitCode, false)
	}
}

// holderGoneLocked handles a holder connection that closed. Without a
// holder.ended first, the session is declared lost after GoneGrace unless
// the holder re-registers (it reconnects on its own after transient drops).
func (d *Daemon) holderGoneLocked(cs *connState) {
	s := d.sessions[cs.holderID]
	if s == nil || s.holder != cs {
		return
	}
	s.holder = nil
	if s.ended || d.closed {
		return
	}
	s.gone = time.AfterFunc(d.opts.GoneGrace, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || d.sessions[s.s.ID] != s || s.holder != nil || s.ended {
			return
		}
		s.gone = nil
		d.endLocked(s, -1, true)
	})
}

// endLocked records the end of a session: final state, session_ended, an
// exited notification, and removal from the list after EndedLinger.
func (d *Daemon) endLocked(s *session, exitCode int, lost bool) {
	s.ended = true
	code := exitCode
	s.s.ExitCode = &code
	s.track = s.track.exited()
	d.recomputeLocked(s, true)
	d.settleLocked(s) // the final transition is recorded right away
	d.emitLocked(s.s.ID, wire.EventSessionEnded, wire.SessionEndedData{ExitCode: exitCode})
	n := wire.NotificationData{Title: sessionLabel(s.s.Harness, s.s.Cwd), Level: wire.LevelInfo, Reason: reasonExited, Body: "Exited"}
	switch {
	case lost:
		n.Level, n.Body = wire.LevelWarn, "Session lost (its holder stopped)"
	case exitCode != 0:
		n.Level, n.Body = wire.LevelWarn, fmt.Sprintf("Exited with code %d", exitCode)
	}
	d.notifyLocked(s.s.ID, n, nil)
	d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	s.remove = time.AfterFunc(d.opts.EndedLinger, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || d.sessions[s.s.ID] != s {
			return
		}
		delete(d.sessions, s.s.ID)
		d.broadcastLocked(wire.NotifySessionRemoved, wire.SessionRef{ID: s.s.ID})
		d.scheduleSweepLocked(sweepAfterRemoval)
	})
}

// recomputeLocked derives the session state from its track. On a change it
// updates the session, cancels approvals the terminal already answered
// (when cancelApprovals) and (re)starts the debounce of state_changed. It
// reports whether the state changed; the caller broadcasts.
func (d *Daemon) recomputeLocked(s *session, cancelApprovals bool) bool {
	state, source := s.track.merged()
	if state == s.s.State && source == s.s.StateSource {
		return false
	}
	s.s.State, s.s.StateSource = state, source
	if cancelApprovals && (state == wire.StateWorking || state == wire.StateExited) {
		d.cancelApprovalsLocked(s.s.ID)
	}
	d.debounceLocked(s)
	return true
}

// debounceLocked schedules recording the current state once it has been
// stable for DebounceMs. Continuous flapping is still recorded after
// 4×DebounceMs.
func (d *Daemon) debounceLocked(s *session) {
	delay := time.Duration(d.eff.Notify.DebounceMs) * time.Millisecond
	now := time.Now()
	if s.debounce != nil {
		if now.Sub(s.pendingSince) >= 4*delay {
			return // let the pending timer fire
		}
		s.debounce.Stop()
	} else {
		s.pendingSince = now
	}
	s.debounceGen++
	gen := s.debounceGen
	s.debounce = time.AfterFunc(delay, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || s.debounceGen != gen {
			return
		}
		d.settleLocked(s)
	})
}

// settleLocked records a state_changed event (and the notification the
// transition raises) if the state differs from the last recorded one.
func (d *Daemon) settleLocked(s *session) {
	if s.debounce != nil {
		s.debounce.Stop()
		s.debounce = nil
		s.debounceGen++
	}
	from, to := s.logged, s.s.State
	if from == to {
		return
	}
	s.logged = to
	d.emitLocked(s.s.ID, wire.EventStateChanged, wire.StateChangedData{From: from, To: to, Source: s.s.StateSource})
	switch notifyReasonForTransition(from, to) {
	case reasonWaitingInput:
		d.notifyLocked(s.s.ID, wire.NotificationData{
			Title: sessionLabel(s.s.Harness, s.s.Cwd), Body: "Waiting for input",
			Level: wire.LevelInfo, Reason: reasonWaitingInput,
		}, nil)
	case reasonNeedsApproval:
		body := "Needs approval"
		if ap := d.firstApprovalLocked(s.s.ID); ap != nil && ap.a.ToolName != "" {
			body += ": " + ap.a.ToolName
		}
		d.notifyLocked(s.s.ID, wire.NotificationData{
			Title: sessionLabel(s.s.Harness, s.s.Cwd), Body: body,
			Level: wire.LevelInfo, Reason: reasonNeedsApproval,
		}, nil)
	}
}

func (d *Daemon) sessionList() []wire.Session {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sessionListLocked()
}

// sessionListLocked lists sessions, newest first.
func (d *Daemon) sessionListLocked() []wire.Session {
	out := make([]wire.Session, 0, len(d.sessions))
	for _, s := range d.sessions {
		out = append(out, s.s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt > out[j].StartedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// sessionLabel names a session in notifications: "<harness> · <cwd base>".
func sessionLabel(harness, cwd string) string {
	if harness == "" {
		harness = wire.HarnessOther
	}
	cwd = strings.TrimRight(strings.ReplaceAll(cwd, `\`, "/"), "/")
	base := cwd[strings.LastIndexByte(cwd, '/')+1:]
	if base == "" || strings.HasSuffix(base, ":") {
		return harness
	}
	return harness + " · " + base
}

// clipLine collapses whitespace and clips to max bytes on a rune boundary.
func clipLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
