package wire

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Sessions

// Session states.
const (
	StateWorking       = "working"
	StateIdle          = "idle"
	StateWaitingInput  = "waiting_input"
	StateNeedsApproval = "needs_approval"
	StateExited        = "exited"
)

// Where a state came from, highest priority first.
const (
	SourceHook     = "hook"     // harness hook (structured)
	SourceTerminal = "terminal" // OSC 9/99/777 notification, BEL
	SourceActivity = "activity" // output activity / quiescence
	SourceProcess  = "process"  // child process exit
)

// Harness ids.
const (
	HarnessClaude = "claude"
	HarnessCodex  = "codex"
	HarnessOmp    = "omp"
	HarnessOther  = "other"
)

// Session modes.
const (
	ModePassthrough = "passthrough" // runs in a local terminal; that terminal owns the size
	ModeDetached    = "detached"    // no local terminal; the app owns the size
)

// Session is one PTY session held by a `stagent run` process.
type Session struct {
	ID             string   `json:"id"`
	Harness        string   `json:"harness"`
	Command        []string `json:"command"`
	Cwd            string   `json:"cwd"`
	PID            int      `json:"pid"` // child (agent) pid
	HolderPID      int      `json:"holder_pid"`
	Mode           string   `json:"mode"`
	State          string   `json:"state"`
	StateSource    string   `json:"state_source"`
	Title          string   `json:"title,omitempty"` // OSC 0/2 window title
	ConversationID string   `json:"conversation_id,omitempty"`
	TranscriptPath string   `json:"transcript_path,omitempty"`
	LastMessage    string   `json:"last_message,omitempty"` // short, single line
	Cols           int      `json:"cols"`
	Rows           int      `json:"rows"`
	StartedAt      int64    `json:"started_at"`       // unix ms
	LastActivityAt int64    `json:"last_activity_at"` // unix ms, last PTY output
	ExitCode       *int     `json:"exit_code,omitempty"`
}

// NewSessionID returns a random 16-hex-char id (paths.sessionIDLen).
func NewSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// DetectHarness maps a command's executable to a harness id.
func DetectHarness(argv []string) string {
	if len(argv) == 0 {
		return HarnessOther
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		base = strings.TrimSuffix(base, ext)
	}
	switch base {
	case HarnessClaude, HarnessCodex, HarnessOmp:
		return base
	}
	return HarnessOther
}

// EnvSessionID is exported to the agent process so hooks can name the PTY
// session they belong to.
const EnvSessionID = "STAGENT_SESSION_ID"

// ---------------------------------------------------------------------------
// Events (persisted in the event log, replayed with `since`)

// Event kinds.
const (
	EventSessionStarted    = "session_started"
	EventSessionEnded      = "session_ended"
	EventStateChanged      = "state_changed"
	EventNotification      = "notification"
	EventApprovalRequested = "approval_requested"
	EventApprovalResolved  = "approval_resolved"
)

// Event is one entry of the daemon's event log.
type Event struct {
	Seq       int64           `json:"seq"`
	Time      int64           `json:"time"` // unix ms
	SessionID string          `json:"session_id,omitempty"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// SessionStartedData is Event.Data for session_started.
type SessionStartedData struct {
	Harness string   `json:"harness"`
	Command []string `json:"command"`
	Cwd     string   `json:"cwd"`
	Mode    string   `json:"mode"`
}

// SessionEndedData is Event.Data for session_ended.
type SessionEndedData struct {
	ExitCode int `json:"exit_code"`
}

// StateChangedData is Event.Data for state_changed.
type StateChangedData struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Source string `json:"source"`
}

// Notification levels.
const (
	LevelInfo = "info"
	LevelWarn = "warn"
)

// NotificationData is Event.Data for notification — what the app shows and
// what offline push channels send. Body never contains terminal output.
type NotificationData struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Level string `json:"level"`
	// Reason: "waiting_input" | "needs_approval" | "turn_complete" |
	// "exited" | "terminal" (OSC/BEL from the program) | "test" | "digest".
	Reason string `json:"reason"`
	// Count > 1 for a digest folding several notifications together.
	Count int `json:"count,omitempty"`
}

// Approval is a pending permission request raised by a blocking hook.
type Approval struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id,omitempty"`
	Harness   string `json:"harness"`
	ToolName  string `json:"tool_name,omitempty"`
	Summary   string `json:"summary"` // human readable, e.g. the command line
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at"`
}

// Approval decisions.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
	DecisionNone  = "none" // no decision: the harness asks in the terminal
)

// ApprovalResolvedData is Event.Data for approval_resolved.
type ApprovalResolvedData struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"`
	By        string `json:"by"` // "app" | "timeout" | "cancelled"
}

// ---------------------------------------------------------------------------
// Transcripts

// Message is one normalized conversation entry.
type Message struct {
	Role     string `json:"role"` // user | assistant | tool | system
	Text     string `json:"text"`
	ToolName string `json:"tool_name,omitempty"`
	Time     int64  `json:"time,omitempty"` // unix ms
}

// Conversation is one stored conversation of a harness (conversations.list).
type Conversation struct {
	Harness       string `json:"harness"`
	ID            string `json:"id"`
	Cwd           string `json:"cwd"`
	Title         string `json:"title"`
	UpdatedAt     int64  `json:"updated_at"` // unix ms (file mtime)
	Path          string `json:"path"`
	LiveSessionID string `json:"live_session_id,omitempty"`
}

// ---------------------------------------------------------------------------
// Configuration (config.json, config.get / config.set)

// Config is the daemon configuration. Zero values mean "default".
type Config struct {
	Notify    NotifyConfig    `json:"notify"`
	Retention RetentionConfig `json:"retention"`
	// ApprovalTimeoutSec bounds how long a blocking hook waits for the app.
	ApprovalTimeoutSec int `json:"approval_timeout_sec"`
	// IdleAfterMs: quiet time after which output activity counts as idle.
	IdleAfterMs int `json:"idle_after_ms"`
}

// NotifyConfig configures offline push channels.
type NotifyConfig struct {
	Ntfy    NtfyConfig    `json:"ntfy"`
	Webhook WebhookConfig `json:"webhook"`
	// DebounceMs: minimum stable time of a state before it notifies.
	DebounceMs int `json:"debounce_ms"`
	// DigestWindowMs: notifications within this window fold into one digest.
	DigestWindowMs int `json:"digest_window_ms"`
}

// NtfyConfig is an ntfy.sh (or self-hosted ntfy) topic.
type NtfyConfig struct {
	Enabled bool   `json:"enabled"`
	Server  string `json:"server"` // default https://ntfy.sh
	Topic   string `json:"topic"`
	Token   string `json:"token,omitempty"`
}

// WebhookConfig posts NotificationData JSON to a URL.
type WebhookConfig struct {
	Enabled bool              `json:"enabled"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// RetentionConfig bounds on-disk data.
type RetentionConfig struct {
	EventsDays         int `json:"events_days"`
	EventsMax          int `json:"events_max"`
	ScrollbackDays     int `json:"scrollback_days"`
	ScrollbackTotalMiB int `json:"scrollback_total_mib"`
	// ScrollbackSessionMiB caps one session (segments of 1 MiB).
	ScrollbackSessionMiB int `json:"scrollback_session_mib"`
}

// WithDefaults fills zero fields with the documented defaults.
func (c Config) WithDefaults() Config {
	if c.ApprovalTimeoutSec <= 0 {
		c.ApprovalTimeoutSec = 60
	}
	if c.IdleAfterMs <= 0 {
		c.IdleAfterMs = 3000
	}
	if c.Notify.Ntfy.Server == "" {
		c.Notify.Ntfy.Server = "https://ntfy.sh"
	}
	if c.Notify.DebounceMs <= 0 {
		c.Notify.DebounceMs = 2000
	}
	if c.Notify.DigestWindowMs <= 0 {
		c.Notify.DigestWindowMs = 5000
	}
	r := &c.Retention
	if r.EventsDays <= 0 {
		r.EventsDays = 7
	}
	if r.EventsMax <= 0 {
		r.EventsMax = 10000
	}
	if r.ScrollbackDays <= 0 {
		r.ScrollbackDays = 3
	}
	if r.ScrollbackTotalMiB <= 0 {
		r.ScrollbackTotalMiB = 512
	}
	if r.ScrollbackSessionMiB <= 0 {
		r.ScrollbackSessionMiB = 8
	}
	return c
}
