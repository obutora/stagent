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
	Attached       bool     `json:"attached"` // a session.attach client is attached (not passthrough's own terminal)
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
	// LastLocalInputAt is when a terminal on the host last typed anything
	// into the session, unix ms: the passthrough session's own terminal or
	// a `stagent attach` (session.input with local); 0 (omitted) until one
	// has. Terminal replies, focus and mouse reports do not count, nor does
	// the app's session.input.
	LastLocalInputAt int64 `json:"last_local_input_at,omitempty"`
	// Focused: one of those terminals last reported focus in (ESC [ I,
	// sent once the program enabled focus reporting) and not focus out
	// since. Terminals that report no focus leave it false.
	Focused bool `json:"focused,omitempty"`
	// PresenceFile is the holder's $CLAUDE_CLIENT_PRESENCE_FILE (omitted
	// when unset): while that file exists, someone is at the session.
	PresenceFile string `json:"presence_file,omitempty"`
	ExitCode     *int   `json:"exit_code,omitempty"`
	// TaskID is the task the session belongs to (omitted: none). The
	// daemon decides it: a worktree task's sessions are those whose cwd is
	// in its worktree; an in_place task's are its prepared session and the
	// ones session.spawn started with its task_id. A holder registers with
	// the task_id it was started with (`stagent run --task`).
	TaskID string `json:"task_id,omitempty"`
}

// NewSessionID returns a random 16-hex-char id (paths.sessionIDLen).
func NewSessionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// DetectHarness maps a command's executable to a harness id. For a task's
// prepared session it is the command `stagent task run` ends in
// (AgentArgv).
func DetectHarness(argv []string) string {
	argv = AgentArgv(argv)
	if len(argv) == 0 {
		return HarnessOther
	}
	switch base := programName(argv[0]); base {
	case HarnessClaude, HarnessCodex, HarnessOmp:
		return base
	}
	return HarnessOther
}

// AgentArgv is the program a command runs in the end: for `stagent task
// run [flags] <id> -- <command…>` (a task's prepared session) the command
// after "--", which that one becomes; argv itself otherwise.
func AgentArgv(argv []string) []string {
	if len(argv) < 4 || programName(argv[0]) != "stagent" || argv[1] != "task" || argv[2] != "run" {
		return argv
	}
	for i := 3; i < len(argv); i++ {
		if argv[i] == "--" {
			return argv[i+1:]
		}
	}
	return argv
}

// programName is the lower-case base name of argv0 without a Windows
// program extension.
func programName(argv0 string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(argv0, `\`, "/")))
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// EnvSessionID is exported to the agent process so hooks can name the PTY
// session they belong to.
const EnvSessionID = "STAGENT_SESSION_ID"

// Environment variables claude sets (stagent only reads them): the
// session's Remote Control connection, seen by its hooks, and a file whose
// existence says that a client is at the session.
const (
	EnvClaudeBridgeSessionID = "CLAUDE_CODE_BRIDGE_SESSION_ID"
	EnvClaudePresenceFile    = "CLAUDE_CLIENT_PRESENCE_FILE"
)

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
	// HungUp: the program ended after a client's session.signal hangup
	// (the app ending a kept shell) or, on Windows, after the console of a
	// passthrough session closed; not an abnormal exit.
	HungUp bool `json:"hung_up,omitempty"`
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
	// SessionID is the session the notification is about; absent for a
	// digest, notify.test and approvals of no session. ntfy's sequence ID
	// and the s= of the Click link are both made from it.
	SessionID string `json:"session_id,omitempty"`
	// TaskNumber is the number of the session's task (0: none). Not part
	// of the event: pushes title the notification "#N <title>" (see
	// notify's labelled), a digest of several never.
	TaskNumber int `json:"-"`
}

// Approval is a pending permission request reported by a harness hook. It
// is answered on the program's own screen; stagent only tracks it.
type Approval struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id,omitempty"`
	Harness   string `json:"harness"`
	ToolName  string `json:"tool_name,omitempty"`
	Summary   string `json:"summary"` // human readable, e.g. the command line
	CreatedAt int64  `json:"created_at"`
}

// ApprovalResolvedData is Event.Data for approval_resolved.
type ApprovalResolvedData struct {
	RequestID string `json:"request_id"`
	By        string `json:"by"` // always "cancelled": answered on screen or the turn moved on
}

// UnwrappedLaunch is an agent started interactively on this host without
// a stagent session (GLOSSARY: このホストの agent に出ない起動), seen
// through its hooks. The daemon keeps these in memory only, one per
// conversation.
type UnwrappedLaunch struct {
	ConversationID string `json:"conversation_id"`
	Harness        string `json:"harness"`
	Cwd            string `json:"cwd"`
	Reason         string `json:"reason"`           // Unwrapped* below
	FirstSeenAt    int64  `json:"first_seen_at"`    // unix ms, the first hook
	LastActivityAt int64  `json:"last_activity_at"` // unix ms, the latest hook
}

// Why a launch has no stagent session (UnwrappedLaunch.Reason).
const (
	// UnwrappedOldTerminal: the shell lacks the wrapper's marker, i.e. a
	// terminal opened before the wrapper was installed (or one that does
	// not read the shell's rc files).
	UnwrappedOldTerminal = "old_terminal"
	// UnwrappedBypassed: the shell has the wrapper but it was bypassed
	// (`command claude`, a full path).
	UnwrappedBypassed = "bypassed"
	// UnwrappedIDE: an IDE extension or a desktop app (Claude Code's
	// CLAUDE_CODE_ENTRYPOINT is neither cli nor sdk-*; a Codex originator
	// other than its terminal UI and its batch runs).
	UnwrappedIDE = "ide"
	// UnwrappedSSH (Windows): started in an SSH session, where the
	// PowerShell wrapper does not wrap (not UserInteractive) and the agent
	// ends with the connection.
	UnwrappedSSH = "ssh"
	// UnwrappedNoTerminal (Windows): no wrapper's marker and no shell above
	// the harness, i.e. a program started it without a terminal.
	UnwrappedNoTerminal = "no_terminal"
)

// EnvShellWrapper is the marker the shell wrapper's rc block exports.
const EnvShellWrapper = "STAGENT_SHELL_WRAPPER"

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
	// IdleAfterMs: quiet time after which output activity counts as idle.
	IdleAfterMs int `json:"idle_after_ms"`
	// DisableHandoff makes `stagent run --handoff=auto` (the shell
	// wrappers) end the program with its terminal unless STAGENT_HANDOFF
	// says otherwise.
	DisableHandoff bool `json:"disable_handoff"`
	// Repos holds settings per source checkout (元のチェックアウト), keyed
	// by its canonical path (task.Canonical).
	Repos map[string]RepoConfig `json:"repos,omitempty"`
}

// RepoConfig is one source checkout's settings.
type RepoConfig struct {
	// WorktreeInclude: git pathspec globs (`:(glob)`, from the checkout's
	// root) of gitignored files a new task worktree gets a copy of, on top
	// of the checkout's .worktreeinclude (コピーするファイル).
	WorktreeInclude []string `json:"worktree_include,omitempty"`
}

// NotifyConfig configures offline push channels.
type NotifyConfig struct {
	Ntfy    NtfyConfig    `json:"ntfy"`
	Webhook WebhookConfig `json:"webhook"`
	Chat    ChatConfig    `json:"chat"`
	// DebounceMs: minimum stable time of a state before it notifies.
	DebounceMs int `json:"debounce_ms"`
	// DigestWindowMs: notifications within this window fold into one digest.
	DigestWindowMs int `json:"digest_window_ms"`
	// HostLabel prefixes the title of every push so the user can tell the
	// hosts apart; empty means the host name (no default is filled in).
	HostLabel string `json:"host_label"`
	// ClickBase is the link an ntfy push opens when tapped (ntfy's Click),
	// with h=<host_id> and, for one session, s=<session id> added to its
	// query; empty means no link. The app sets it; stagent knows no app
	// scheme.
	ClickBase string `json:"click_base"`
	// ClickPage is the open page (開くページ) the chat destination and the
	// generic webhook link to: an https URL, h=<host_id> and, for one
	// session, s=<session id> go into its fragment; empty means no link.
	ClickPage string `json:"click_page"`
	// Reasons selects what is pushed; in-app notification events are not
	// affected. Missing keys take the defaults (WithDefaults).
	Reasons NotifyReasons `json:"reasons"`
	// Lang is the language of the fixed phrases and digest titles that
	// stagent writes: en | ja | ko | zh (default en). Reasons stay as they
	// are.
	Lang string `json:"lang"`
	// SkipWhenClaudeAppNotifies: no pushes for a session whose latest hook
	// ran with CLAUDE_CODE_BRIDGE_SESSION_ID (the claude is connected to
	// Remote Control, so the Claude app notifies too).
	SkipWhenClaudeAppNotifies bool `json:"skip_when_claude_app_notifies"`
}

// NotifyReasons selects the notification reasons that are pushed. A nil
// bool or an empty Exited means the default.
type NotifyReasons struct {
	NeedsApproval *bool `json:"needs_approval,omitempty"` // default true
	WaitingInput  *bool `json:"waiting_input,omitempty"`  // default true
	TurnComplete  *bool `json:"turn_complete,omitempty"`  // default true
	// Exited: off | error (only abnormal exits: a code other than 0 or a
	// lost session; default) | all.
	Exited   string `json:"exited,omitempty"`
	Terminal *bool  `json:"terminal,omitempty"` // default false
}

// NotifyReasons.Exited values.
const (
	ExitedOff   = "off"
	ExitedError = "error"
	ExitedAll   = "all"
)

// Languages of notify.lang.
const (
	LangEn = "en"
	LangJa = "ja"
	LangKo = "ko"
	LangZh = "zh"
)

// ValidLang reports whether lang is a notify.lang value ("" = default).
func ValidLang(lang string) bool {
	switch lang {
	case "", LangEn, LangJa, LangKo, LangZh:
		return true
	}
	return false
}

// ValidExited reports whether v is a notify.reasons.exited value ("" =
// default).
func ValidExited(v string) bool {
	switch v {
	case "", ExitedOff, ExitedError, ExitedAll:
		return true
	}
	return false
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

// ChatConfig sends to a chat service in its own message format (chat
// destination). URL names the service and is a secret: Discord
// (https://discord.com/api/webhooks/<id>/<token>, also discordapp.com, or
// discord://<id>/<token>), Slack Incoming Webhooks
// (https://hooks.slack.com/services/<a>/<b>/<c> or slack://<a>/<b>/<c>) or
// Telegram (tgram://<bot token>/<chat id>).
type ChatConfig struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

// Push channel names (keys of NotifyStatus.LastError, notify.test's
// channels).
const (
	ChannelNtfy    = "ntfy"
	ChannelWebhook = "webhook"
	ChannelChat    = "chat"
)

// NotifyFailure is the last failed push to one channel. Error never
// contains the ntfy topic, the webhook URL or the chat URL; for the chat
// destination it is the status line followed by the service's short
// reason (`404 Not Found: Unknown Webhook (10015)`).
type NotifyFailure struct {
	At     int64  `json:"at"`               // unix ms
	Status int    `json:"status,omitempty"` // HTTP status; 0 for a transport error
	Error  string `json:"error"`
	Kind   string `json:"kind,omitempty"` // Failure*, the chat destination only
}

// Kinds of a failed push to the chat destination (NotifyFailure.Kind). A
// client shows Error for a kind it does not know.
const (
	FailureRevoked     = "revoked"      // the service disabled the destination (webhook deleted, bot token revoked)
	FailureUnreachable = "unreachable"  // the destination cannot be reached (bot blocked, channel gone or archived)
	FailureRateLimited = "rate_limited" // 429
	FailureRejected    = "rejected"     // any other 4xx: the message stagent built was refused
	FailureServer      = "server"       // 5xx
	FailureNetwork     = "network"      // no status line: the request did not get an answer
)

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
	if c.Notify.Lang == "" {
		c.Notify.Lang = LangEn
	}
	nr := &c.Notify.Reasons
	for _, f := range []struct {
		p   **bool
		def bool
	}{{&nr.NeedsApproval, true}, {&nr.WaitingInput, true}, {&nr.TurnComplete, true}, {&nr.Terminal, false}} {
		if *f.p == nil {
			v := f.def
			*f.p = &v
		}
	}
	if nr.Exited == "" {
		nr.Exited = ExitedError
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
