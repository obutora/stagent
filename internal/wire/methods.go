package wire

import "encoding/json"

// ---------------------------------------------------------------------------
// App ↔ bridge methods (see PROTOCOL.md). The bridge serves session.* by
// forwarding to the session's holder and everything else in the "daemon"
// group by forwarding to the daemon, verbatim.

const (
	MethodHello = "hello"
	MethodPing  = "ping"

	// Served by the holder (bridge forwards to Layout.HolderAddr(id)).
	MethodSessionAttach     = "session.attach"
	MethodSessionDetach     = "session.detach"
	MethodSessionInput      = "session.input"
	MethodSessionResize     = "session.resize"
	MethodSessionScrollback = "session.scrollback"
	MethodSessionSignal     = "session.signal"
	MethodSessionInfo       = "session.info"

	// Served by the bridge itself (spawns a detached holder).
	MethodSessionSpawn = "session.spawn"

	// Served by the daemon.
	MethodWatch                 = "watch"
	MethodSessionsList          = "sessions.list"
	MethodConversationsList     = "conversations.list"
	MethodTranscriptGet         = "transcript.get"
	MethodTranscriptSubscribe   = "transcript.subscribe"
	MethodTranscriptUnsubscribe = "transcript.unsubscribe"
	MethodApprovalsList         = "approvals.list"
	MethodApprovalRespond       = "approval.respond"
	MethodConfigGet             = "config.get"
	MethodConfigSet             = "config.set"
	MethodNotifyTest            = "notify.test"

	// Notifications (server → app).
	NotifyEvent          = "event"           // Event
	NotifySessionUpdated = "session.updated" // Session
	NotifySessionRemoved = "session.removed" // SessionRef
	NotifyOutput         = "output"          // OutputParams
	NotifyResize         = "resize"          // ResizeParams
	NotifyClosed         = "closed"          // ClosedParams
	NotifyTranscript     = "transcript"      // TranscriptParams
)

// DaemonMethods are forwarded by the bridge to the daemon.
var DaemonMethods = map[string]bool{
	MethodWatch: true, MethodSessionsList: true, MethodConversationsList: true,
	MethodTranscriptGet: true, MethodTranscriptSubscribe: true, MethodTranscriptUnsubscribe: true,
	MethodApprovalsList: true, MethodApprovalRespond: true,
	MethodConfigGet: true, MethodConfigSet: true, MethodNotifyTest: true,
}

// HolderMethods are forwarded by the bridge to the session's holder.
var HolderMethods = map[string]bool{
	MethodSessionAttach: true, MethodSessionDetach: true, MethodSessionInput: true,
	MethodSessionResize: true, MethodSessionScrollback: true, MethodSessionSignal: true,
	MethodSessionInfo: true,
}

// Capabilities advertised in HelloResult.
const (
	CapScreenMode = "screen_mode" // session.attach mode "screen"
	CapSpawn      = "spawn"
	CapHooks      = "hooks" // approvals / hook-derived state
	CapTranscript = "transcript"
	CapPush       = "push" // ntfy / webhook
)

type HelloParams struct {
	Protocol int    `json:"protocol"`
	Client   string `json:"client"` // e.g. "ssh-term/2.38.0"
}

type HelloResult struct {
	Protocol     int      `json:"protocol"`
	Version      string   `json:"version"`
	OS           string   `json:"os"`   // linux | darwin | windows
	Arch         string   `json:"arch"` // amd64 | arm64
	Home         string   `json:"home"`
	Capabilities []string `json:"capabilities"`
}

// SessionRef names a session.
type SessionRef struct {
	ID string `json:"id"`
}

type WatchParams struct {
	// Since is the last event seq the client has seen; events after it are
	// returned in Missed. 0 replays nothing.
	Since int64 `json:"since"`
}

type WatchResult struct {
	Sessions  []Session  `json:"sessions"`
	Approvals []Approval `json:"approvals"`
	Seq       int64      `json:"seq"` // latest seq at subscription time
	Missed    []Event    `json:"missed"`
	// Truncated: events between Since and Missed[0] were dropped by retention.
	Truncated bool `json:"truncated"`
}

type SessionsListResult struct {
	Sessions []Session `json:"sessions"`
}

// Attach modes.
const (
	AttachRaw    = "raw"    // PTY bytes as produced, resynced by snapshot on overflow
	AttachScreen = "screen" // ANSI diffs of changed rows, at most FPS per second
)

type AttachParams struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`          // default raw
	FPS  int    `json:"fps,omitempty"` // screen mode cap, default 15, max 30
}

type AttachResult struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Mode string `json:"mode"`
}

// OutputParams carries terminal bytes to an attached client. When Reset is
// true the client must clear its terminal (screen + scrollback) and then
// write Data, which is a full ANSI snapshot of the current screen.
type OutputParams struct {
	ID    string `json:"id"`
	Data  []byte `json:"data"` // base64 in JSON
	Reset bool   `json:"reset,omitempty"`
}

type ResizeParams struct {
	ID   string `json:"id"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	// Force lets the app take the size of a passthrough session.
	Force bool `json:"force,omitempty"`
}

type ClosedParams struct {
	ID       string `json:"id"`
	ExitCode int    `json:"exit_code"`
}

// InputParams is applied in order: Text (raw), Paste (bracketed when the
// program enabled bracketed paste), Keys, then a CR if Submit.
type InputParams struct {
	ID     string   `json:"id"`
	Text   string   `json:"text,omitempty"`
	Paste  string   `json:"paste,omitempty"`
	Keys   []string `json:"keys,omitempty"` // names from KeySequences
	Submit bool     `json:"submit,omitempty"`
}

// KeySequences maps session.input key names to the bytes sent to the PTY.
var KeySequences = map[string]string{
	"enter": "\r", "esc": "\x1b", "tab": "\t", "shift-tab": "\x1b[Z",
	"backspace": "\x7f", "delete": "\x1b[3~", "space": " ",
	"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
	"home": "\x1b[H", "end": "\x1b[F", "pageup": "\x1b[5~", "pagedown": "\x1b[6~",
	"ctrl-c": "\x03", "ctrl-d": "\x04", "ctrl-z": "\x1a", "ctrl-l": "\x0c",
	"ctrl-r": "\x12", "ctrl-u": "\x15", "ctrl-o": "\x0f", "ctrl-t": "\x14",
}

type ScrollbackParams struct {
	ID string `json:"id"`
	// Before is an exclusive byte offset into the session's output stream;
	// 0 means "up to the latest byte".
	Before   int64 `json:"before,omitempty"`
	MaxBytes int   `json:"max_bytes,omitempty"` // default 256 KiB, max 1 MiB
}

type ScrollbackResult struct {
	Data  []byte `json:"data"`  // raw PTY bytes [Start, End)
	Start int64  `json:"start"` // stream offset of Data[0]
	End   int64  `json:"end"`
	// First is the oldest offset still retained; Start == First means the
	// client has reached the beginning.
	First int64 `json:"first"`
}

type SpawnParams struct {
	Command []string          `json:"command"`
	Cwd     string            `json:"cwd,omitempty"` // default home
	Cols    int               `json:"cols"`
	Rows    int               `json:"rows"`
	Env     map[string]string `json:"env,omitempty"`
}

type SpawnResult struct {
	Session Session `json:"session"`
}

// Signals for session.signal.
const (
	SignalInterrupt = "interrupt" // Ctrl-C to the PTY
	SignalTerminate = "terminate" // SIGTERM / CTRL_BREAK / TerminateProcess after grace
	SignalKill      = "kill"      // SIGKILL / TerminateProcess
)

type SignalParams struct {
	ID     string `json:"id"`
	Signal string `json:"signal"`
}

type ConversationsListParams struct {
	Harness []string `json:"harness,omitempty"` // empty = all
	Limit   int      `json:"limit,omitempty"`   // default 50 per harness
}

type ConversationsListResult struct {
	Conversations []Conversation `json:"conversations"`
}

// TranscriptParams selects a transcript by live session, by conversation, or
// by path (in that order of precedence).
type TranscriptGetParams struct {
	SessionID      string `json:"session_id,omitempty"`
	Harness        string `json:"harness,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Path           string `json:"path,omitempty"`
	Limit          int    `json:"limit,omitempty"` // default 50
	// Before is the cursor returned by a previous call (byte offset); 0 =
	// newest messages.
	Before int64 `json:"before,omitempty"`
}

type TranscriptGetResult struct {
	Path     string    `json:"path"`
	Harness  string    `json:"harness"`
	Messages []Message `json:"messages"` // oldest first
	// Cursor to pass as Before for older messages; 0 = beginning reached.
	Cursor int64 `json:"cursor"`
}

type TranscriptSubscribeParams struct {
	SessionID string `json:"session_id,omitempty"`
	Path      string `json:"path,omitempty"`
}

// TranscriptParams is the "transcript" notification: new messages appended.
type TranscriptParams struct {
	SessionID string    `json:"session_id,omitempty"`
	Path      string    `json:"path"`
	Messages  []Message `json:"messages"`
}

type ApprovalsListResult struct {
	Approvals []Approval `json:"approvals"`
}

type ApprovalRespondParams struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"` // allow | deny
	Message   string `json:"message,omitempty"`
}

type ConfigResult struct {
	Config Config `json:"config"`
}

type ConfigSetParams struct {
	Config Config `json:"config"`
}

// ---------------------------------------------------------------------------
// Local IPC only

const (
	// holder → daemon (persistent connection opened by the holder)
	MethodHolderRegister = "holder.register" // Session → HolderRegisterResult
	MethodHolderUpdate   = "holder.update"   // notification, SessionPatch
	MethodHolderNotify   = "holder.notify"   // notification, HolderNotifyParams
	MethodHolderEnded    = "holder.ended"    // notification, ClosedParams

	// hook → daemon (one request per connection)
	MethodHookEvent = "hook.event" // HookEventParams → HookEventResult

	// install / doctor → daemon
	MethodDaemonStatus   = "daemon.status"   // → DaemonStatus
	MethodDaemonShutdown = "daemon.shutdown" // → {}
	// daemon → holder notification sent before a deliberate shutdown
	// (daemon.shutdown, SIGTERM): holders keep retrying but stop
	// auto-starting the daemon, so `uninstall --level stop` sticks.
	MethodDaemonStopping = "daemon.stopping"
)

type HolderRegisterResult struct {
	IdleAfterMs int `json:"idle_after_ms"`
}

// SessionPatch updates a registered session; nil fields are unchanged.
type SessionPatch struct {
	ID             string  `json:"id"`
	State          *string `json:"state,omitempty"`
	StateSource    *string `json:"state_source,omitempty"`
	Title          *string `json:"title,omitempty"`
	LastActivityAt *int64  `json:"last_activity_at,omitempty"`
	Cols           *int    `json:"cols,omitempty"`
	Rows           *int    `json:"rows,omitempty"`
}

// HolderNotifyParams is a notification the program itself emitted (OSC 9 /
// 99 / 777, or BEL).
type HolderNotifyParams struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	Bell  bool   `json:"bell,omitempty"`
}

type HookEventParams struct {
	Harness   string          `json:"harness"`
	Event     string          `json:"event"`                // harness-native event name
	SessionID string          `json:"session_id,omitempty"` // $STAGENT_SESSION_ID
	Payload   json.RawMessage `json:"payload,omitempty"`    // hook stdin (or notify argv JSON)
}

type HookEventResult struct {
	Decision string `json:"decision,omitempty"` // allow | deny | none (permission events)
	Message  string `json:"message,omitempty"`
}

type DaemonStatus struct {
	PID       int    `json:"pid"`
	Version   string `json:"version"`
	StartedAt int64  `json:"started_at"`
	Sessions  int    `json:"sessions"`
	Bridges   int    `json:"bridges"`
}
