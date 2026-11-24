package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"slices"
	"sort"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Stopped agents (止まった agent). The daemon keeps a record of the agent of
// every live session — not ended, no schedule run, an agent harness with a
// conversation id — in <root>/state/agents.json, with the boot id of the
// host (bootid). An agent leaving its session (the session ended, or the
// probe returned it to its shell) keeps its record agentEndedKeep longer:
// while the host shuts down, holders may report their end just before the
// daemon dies. A session whose holder went away without holder.ended (lost
// after GoneGrace) makes its agent a stopped agent at once (reason lost).
//
// At its start the daemon reads the file back. After a reboot (another
// boot id) every record is a stopped agent (reason reboot), and so is one
// found lost within agentEndedKeep of the file's last write (its holder was
// killed as the host shut down). In the same boot an ended record is over,
// and a running one is still running while its holder answers (it
// registers again); otherwise it stopped (lost).
// Stopped agents stay in the file until they are resumed (stopped.resume
// on the bridge claims one; its session registering drops it), removed
// (stopped.remove), continued another way (a live session carries the
// same conversation) or stoppedKeep after they were found.

const (
	agentEndedKeep  = 30 * time.Second
	agentsSaveDelay = time.Second
	stoppedKeep     = 7 * 24 * time.Hour
	// claimFor: how long a claim of stopped.resume holds while its
	// session has not registered.
	claimFor = 60 * time.Second
)

// stoppedServed: stopped agents are kept on Linux and macOS only
// (wire.StoppedAgentsServedOn; the hosts the bridge announces them on).
var stoppedServed = wire.StoppedAgentsServedOn(runtime.GOOS)

// runningAgent is the record of a live session's agent as agents.json
// keeps it. EndedAt (unix ms) is set once the agent left its session.
type runningAgent struct {
	ID             string `json:"id"`
	Harness        string `json:"harness"`
	ConversationID string `json:"conversation_id"`
	Cwd            string `json:"cwd"`
	Title          string `json:"title,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	StartedAt      int64  `json:"started_at"`
	EndedAt        int64  `json:"ended_at,omitempty"`
}

type agentRec struct {
	a     runningAgent
	prune *time.Timer // drops an ended record after agentEndedKeep
}

// stoppedClaim is a stopped.resume about to start session for a stopped
// agent.
type stoppedClaim struct {
	session string
	at      time.Time
}

// agentsFile is the document stored in agents.json. SavedAt: when it was
// written (unix ms).
type agentsFile struct {
	BootID  string              `json:"boot_id"`
	SavedAt int64               `json:"saved_at"`
	Running []runningAgent      `json:"running"`
	Stopped []wire.StoppedAgent `json:"stopped"`
}

func loadAgents(path string) (agentsFile, error) {
	var f agentsFile
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return agentsFile{}, err
	}
	return f, nil
}

// saveAgentsLocked writes agents.json (0600, atomically).
func (d *Daemon) saveAgentsLocked() {
	f := agentsFile{
		BootID: d.bootID, SavedAt: d.opts.Now().UnixMilli(),
		Running: make([]runningAgent, 0, len(d.agents)), Stopped: d.stopped,
	}
	if f.Stopped == nil {
		f.Stopped = []wire.StoppedAgent{}
	}
	for _, r := range d.agents {
		f.Running = append(f.Running, r.a)
	}
	sort.Slice(f.Running, func(i, j int) bool { return f.Running[i].ID < f.Running[j].ID })
	b, err := json.MarshalIndent(f, "", "  ")
	if err == nil {
		err = fsutil.WriteFileAtomic(d.layout.StoppedAgents, b)
	}
	if err != nil {
		d.logf("daemon: agents: %v", err)
	}
}

// saveAgentsSoonLocked writes agents.json agentsSaveDelay from now (one
// write for the changes meanwhile); Shutdown writes what is left.
func (d *Daemon) saveAgentsSoonLocked() {
	if d.agentsSave != nil || d.closed {
		return
	}
	d.agentsSave = time.AfterFunc(agentsSaveDelay, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.agentsSave = nil
		if !d.closed {
			d.saveAgentsLocked()
		}
	})
}

// restoreAgents reads agents.json at the daemon's start and turns the
// agents whose sessions went away into stopped agents.
func (d *Daemon) restoreAgents() {
	if !stoppedServed {
		return
	}
	d.bootID = d.opts.BootID()
	f, err := loadAgents(d.layout.StoppedAgents)
	if err != nil {
		d.logf("daemon: agents: %v (starting without stopped agents)", err)
	}
	now := d.opts.Now().UnixMilli()
	rebooted := f.BootID != "" && d.bootID != "" && f.BootID != d.bootID
	for _, st := range f.Stopped {
		if st.ID != "" && harness.Known(st.Harness) && st.ConversationID != "" {
			if rebooted && st.Reason == wire.StoppedLost && st.FoundAt >= f.SavedAt-agentEndedKeep.Milliseconds() {
				st.Reason = wire.StoppedReboot
			}
			d.addStoppedLocked(st)
		}
	}
	for _, a := range f.Running {
		if a.ID == "" || !harness.Known(a.Harness) || a.ConversationID == "" {
			continue
		}
		switch {
		case rebooted:
			d.addStoppedLocked(stoppedOf(a, wire.StoppedReboot, now))
		case a.EndedAt != 0:
			// It ended normally.
		case d.holderAnswers(a.ID):
			a.EndedAt = 0
			d.agents[a.ID] = &agentRec{a: a}
		default:
			d.addStoppedLocked(stoppedOf(a, wire.StoppedLost, now))
		}
	}
	d.pruneStoppedLocked(now)
	// Nothing recorded, nothing to write: the boot id goes with the first
	// record.
	if len(f.Running) > 0 || len(f.Stopped) > 0 {
		d.saveAgentsLocked()
	}
}

func stoppedOf(a runningAgent, reason string, now int64) wire.StoppedAgent {
	return wire.StoppedAgent{
		ID: a.ID, Harness: a.Harness, ConversationID: a.ConversationID, Cwd: a.Cwd,
		Title: a.Title, TaskID: a.TaskID, StartedAt: a.StartedAt, FoundAt: now, Reason: reason,
	}
}

// agentLostLocked makes the agent of s, whose holder went away without the
// agent ending (the session is lost), a stopped agent (reason lost).
func (d *Daemon) agentLostLocked(s *session) {
	if !stoppedServed || d.closed {
		return
	}
	r := d.agents[s.s.ID]
	if r == nil || r.a.EndedAt != 0 {
		return
	}
	if r.prune != nil {
		r.prune.Stop()
	}
	delete(d.agents, s.s.ID)
	d.addStoppedLocked(stoppedOf(r.a, wire.StoppedLost, d.opts.Now().UnixMilli()))
	d.stoppedChangedLocked()
}

// addStoppedLocked adds st, replacing a stopped agent of the same id or
// conversation.
func (d *Daemon) addStoppedLocked(st wire.StoppedAgent) {
	d.stopped = slices.DeleteFunc(d.stopped, func(o wire.StoppedAgent) bool {
		return o.ID == st.ID || o.Harness == st.Harness && o.ConversationID == st.ConversationID
	})
	d.stopped = append(d.stopped, st)
}

// pruneStoppedLocked drops the stopped agents found stoppedKeep ago or
// earlier, reporting whether there were any.
func (d *Daemon) pruneStoppedLocked(now int64) bool {
	n := len(d.stopped)
	d.stopped = slices.DeleteFunc(d.stopped, func(st wire.StoppedAgent) bool { return stoppedExpired(st, now) })
	return len(d.stopped) != n
}

func stoppedExpired(st wire.StoppedAgent, now int64) bool {
	return now-st.FoundAt >= stoppedKeep.Milliseconds()
}

// stoppedListLocked lists the stopped agents, newest found_at first, then
// newest started_at.
func (d *Daemon) stoppedListLocked() []wire.StoppedAgent {
	now := d.opts.Now().UnixMilli()
	out := make([]wire.StoppedAgent, 0, len(d.stopped))
	for _, st := range d.stopped {
		if !stoppedExpired(st, now) {
			out = append(out, st)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FoundAt != out[j].FoundAt {
			return out[i].FoundAt > out[j].FoundAt
		}
		return out[i].StartedAt > out[j].StartedAt
	})
	return out
}

// stoppedChangedLocked saves and pushes the list after a change.
func (d *Daemon) stoppedChangedLocked() {
	d.pruneStoppedLocked(d.opts.Now().UnixMilli())
	d.saveAgentsSoonLocked()
	d.broadcastLocked(wire.NotifyStoppedUpdated, wire.StoppedUpdatedParams{Stopped: d.stoppedListLocked()})
}

// dropStoppedLocked removes stopped agent id, which session continues
// ("" when removed), and releases its claim. It reports whether it was
// listed; the caller calls stoppedChangedLocked.
func (d *Daemon) dropStoppedLocked(id, session string) bool {
	delete(d.claims, id)
	if session != "" {
		d.resumed[id] = session
	}
	n := len(d.stopped)
	d.stopped = slices.DeleteFunc(d.stopped, func(st wire.StoppedAgent) bool { return st.ID == id })
	return len(d.stopped) != n
}

// sessionChangedLocked pushes session.updated for s after a change, keeping
// the record of its agent and its relay trace up to date first.
func (d *Daemon) sessionChangedLocked(s *session) {
	d.noteAgentLocked(s)
	others := d.fillRelaysLocked(s)
	d.relayOfSessionLocked(s)
	d.broadcastLocked(wire.NotifySessionUpdated, s.s)
	d.relaysTouchedLocked(others)
}

// noteAgentLocked records the agent of s, or marks its record ended when
// the agent left the session. A session carrying a stopped agent's
// conversation continues it: that stopped agent goes.
func (d *Daemon) noteAgentLocked(s *session) {
	if !stoppedServed || d.closed {
		return
	}
	id := s.s.ID
	r := d.agents[id]
	if !s.ended && s.s.RunID == "" && harness.Known(s.s.Harness) && s.s.ConversationID != "" {
		a := runningAgent{
			ID: id, Harness: s.s.Harness, ConversationID: s.s.ConversationID, Cwd: s.s.Cwd,
			Title: s.s.Title, TaskID: s.s.TaskID, StartedAt: s.s.StartedAt,
		}
		if r == nil {
			r = &agentRec{}
			d.agents[id] = r
		}
		if r.prune != nil {
			r.prune.Stop()
			r.prune = nil
		}
		if r.a != a {
			r.a = a
			d.saveAgentsSoonLocked()
		}
		changed := false
		for _, st := range slices.Clone(d.stopped) {
			if st.Harness == a.Harness && st.ConversationID == a.ConversationID {
				changed = d.dropStoppedLocked(st.ID, id) || changed
			}
		}
		if changed {
			d.stoppedChangedLocked()
		}
		return
	}
	if r == nil || r.a.EndedAt != 0 {
		return
	}
	r.a.EndedAt = nowMs()
	d.saveAgentsSoonLocked()
	r.prune = time.AfterFunc(agentEndedKeep, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed || d.agents[id] != r || r.a.EndedAt == 0 {
			return
		}
		delete(d.agents, id)
		d.saveAgentsSoonLocked()
	})
}

// adoptAgentLocked is called for a session that registers anew (not a
// holder reconnecting to this daemon). A holder that registers again after
// a daemon restart reports no conversation (hooks tell it): its record's
// comes back, as the agent's harness for a shell session (which the probe
// returns to the shell once the agent is gone). A stopped agent of its id
// (its holder was taken for gone) is running after all, and the stopped
// agent a stopped.resume claimed for it is continued.
func (d *Daemon) adoptAgentLocked(s *session) {
	if !stoppedServed {
		return
	}
	id := s.s.ID
	conv, agent := "", ""
	if r := d.agents[id]; r != nil && r.a.EndedAt == 0 {
		conv, agent = r.a.ConversationID, r.a.Harness
	}
	changed := false
	for _, st := range slices.Clone(d.stopped) {
		if st.ID == id {
			conv, agent = st.ConversationID, st.Harness
			changed = d.dropStoppedLocked(st.ID, id) || changed
		}
	}
	for sid, c := range d.claims {
		if c.session == id {
			changed = d.dropStoppedLocked(sid, id) || changed
		}
	}
	if changed {
		d.stoppedChangedLocked()
	}
	if conv == "" || s.s.ConversationID != "" || s.s.RunID != "" ||
		(s.s.Harness != agent && s.s.Harness != wire.HarnessOther) {
		return
	}
	s.s.ConversationID = conv
	if s.s.Harness != agent {
		s.s.Harness = agent
		s.agentSeen, s.missed = false, 0
		d.startProbeLocked()
	}
}

// liveConversationLocked is the live session carrying conversation conv of
// harness h, or nil.
func (d *Daemon) liveConversationLocked(h, conv string) *session {
	for _, s := range d.sessions {
		if !s.ended && s.s.Harness == h && s.s.ConversationID == conv {
			return s
		}
	}
	return nil
}

// stoppedClaim reserves stopped agent p.ID for the session p.SessionID a
// stopped.resume is about to start. A live session that carries its
// conversation — or the one an earlier resume of it started — is answered
// instead, and nothing is to be started; busy while another resume's
// session has not registered yet.
func (d *Daemon) stoppedClaim(p wire.StoppedClaimParams) (wire.StoppedClaimResult, error) {
	if p.ID == "" || p.SessionID == "" {
		return wire.StoppedClaimResult{}, wire.Errorf(wire.ErrBadRequest, "stopped.claim: id and session_id required")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	i := slices.IndexFunc(d.stoppedListLocked(), func(st wire.StoppedAgent) bool { return st.ID == p.ID })
	if i < 0 {
		if s := d.sessions[d.resumed[p.ID]]; s != nil && !s.ended {
			return wire.StoppedClaimResult{Session: &s.s}, nil
		}
		return wire.StoppedClaimResult{}, wire.Errorf(wire.ErrNotFound, "no stopped agent %s", p.ID)
	}
	st := d.stoppedListLocked()[i]
	if s := d.liveConversationLocked(st.Harness, st.ConversationID); s != nil {
		if d.dropStoppedLocked(st.ID, s.s.ID) {
			d.stoppedChangedLocked()
		}
		return wire.StoppedClaimResult{Session: &s.s}, nil
	}
	now := time.Now()
	if c, ok := d.claims[p.ID]; ok && c.session != p.SessionID && now.Sub(c.at) < claimFor {
		return wire.StoppedClaimResult{}, wire.Errorf(wire.ErrBusy, "stopped agent %s is being resumed", p.ID)
	}
	d.claims[p.ID] = stoppedClaim{session: p.SessionID, at: now}
	return wire.StoppedClaimResult{Agent: &st}, nil
}

// stoppedUnclaim releases p.SessionID's claim of stopped agent p.ID.
func (d *Daemon) stoppedUnclaim(p wire.StoppedClaimParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.claims[p.ID]; ok && c.session == p.SessionID {
		delete(d.claims, p.ID)
	}
}

// stoppedRemove takes stopped agent id off the list (nothing when it is
// not listed).
func (d *Daemon) stoppedRemove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dropStoppedLocked(id, "") {
		d.stoppedChangedLocked()
	}
}
