package wire

// ---------------------------------------------------------------------------
// Stopped agents (止まった agent): agents that ran in a stagent session
// whose holder went away without the agent ending — the host rebooted, or
// the holder vanished otherwise. Only their conversation is left, which
// stopped.resume continues in a new kept shell. The daemon keeps them in
// <root>/state/agents.json and is the only writer. Linux and macOS only
// (CapStoppedAgents).

const (
	// Served by the bridge (spawn queue): resume one in a new session
	// (StoppedResumeParams → StoppedResumeResult); refused to coding
	// agents' process trees (ADR 0004).
	MethodStoppedResume = "stopped.resume"
	// Served by the daemon (the bridge forwards it), refused to coding
	// agents' process trees: take one off the list (StoppedRef → {}).
	MethodStoppedRemove = "stopped.remove"

	// Local IPC only (daemon), refused to coding agents' process trees.
	//
	// stopped.claim is the bridge's stopped.resume reserving a stopped
	// agent for the session it is about to start (StoppedClaimParams →
	// StoppedClaimResult): the live session that already carries its
	// conversation, or the agent to resume.
	MethodStoppedClaim = "stopped.claim"
	// stopped.unclaim releases a claim whose session did not start
	// (StoppedClaimParams → {}).
	MethodStoppedUnclaim = "stopped.unclaim"

	// NotifyStoppedUpdated carries the whole current list
	// (StoppedUpdatedParams), like unwrapped.updated.
	NotifyStoppedUpdated = "stopped.updated"
)

// CapStoppedAgents: the daemon keeps stopped agents (watch's stopped,
// stopped.updated, stopped.remove) and the bridge serves stopped.resume.
const CapStoppedAgents = "stopped_agents"

// StoppedAgentsServedOn reports whether a host of goos (runtime.GOOS)
// keeps stopped agents: Linux and macOS, as the daemon records them and
// the bridge announces CapStoppedAgents.
func StoppedAgentsServedOn(goos string) bool { return goos == "linux" || goos == "darwin" }

// Why an agent stopped (StoppedAgent.Reason).
const (
	StoppedReboot = "reboot" // the host restarted
	StoppedLost   = "lost"   // its holder went away otherwise
)

// StoppedAgent is an agent whose session went away under it. The list is
// ordered newest found_at first, then newest started_at.
type StoppedAgent struct {
	ID             string `json:"id"` // the old session's id
	Harness        string `json:"harness"`
	ConversationID string `json:"conversation_id"`
	Cwd            string `json:"cwd"`
	Title          string `json:"title,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	StartedAt      int64  `json:"started_at"` // unix ms, the old session's
	FoundAt        int64  `json:"found_at"`   // unix ms, when the daemon found it stopped
	Reason         string `json:"reason"`     // Stopped* above
}

// StoppedUpdatedParams is the stopped.updated notification.
type StoppedUpdatedParams struct {
	Stopped []StoppedAgent `json:"stopped"`
}

// StoppedRef names a stopped agent.
type StoppedRef struct {
	ID string `json:"id"`
}

type StoppedResumeParams struct {
	ID   string            `json:"id"`
	Cols int               `json:"cols"`
	Rows int               `json:"rows"`
	Env  map[string]string `json:"env,omitempty"`
}

// StoppedResumeResult: Existed is set when a live session already carried
// the conversation (or an earlier resume of it started one) and nothing
// was started.
type StoppedResumeResult struct {
	Session Session `json:"session"`
	Existed bool    `json:"existed"`
}

// StoppedClaimParams: SessionID is the id of the session the bridge is
// about to start.
type StoppedClaimParams struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
}

// StoppedClaimResult has Session when a live session already carries the
// conversation (start nothing), else Agent, now claimed for SessionID.
type StoppedClaimResult struct {
	Agent   *StoppedAgent `json:"agent,omitempty"`
	Session *Session      `json:"session,omitempty"`
}
