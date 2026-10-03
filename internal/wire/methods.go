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
	MethodConfigGet             = "config.get"
	MethodConfigSet             = "config.set"
	MethodNotifyTest            = "notify.test"
	MethodPresenceSet           = "presence.set"

	// Notifications (server → app).
	NotifyEvent          = "event"           // Event
	NotifySessionUpdated = "session.updated" // Session
	NotifySessionRemoved = "session.removed" // SessionRef
	NotifyOutput         = "output"          // OutputParams
	NotifyResize         = "resize"          // ResizeParams
	NotifyClosed         = "closed"          // ClosedParams
	NotifyTranscript     = "transcript"      // TranscriptParams
	// NotifyUnwrappedUpdated carries the whole current list.
	NotifyUnwrappedUpdated = "unwrapped.updated" // UnwrappedUpdatedParams
)

// DaemonMethods are forwarded by the bridge to the daemon.
var DaemonMethods = map[string]bool{
	MethodWatch: true, MethodSessionsList: true, MethodConversationsList: true,
	MethodTranscriptGet: true, MethodTranscriptSubscribe: true, MethodTranscriptUnsubscribe: true,
	MethodApprovalsList: true,
	MethodConfigGet:     true, MethodConfigSet: true, MethodNotifyTest: true,
	MethodPresenceSet: true,
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
	// CapPersist: session.spawn shell, attach resume (since/offset/end),
	// signal hangup, runtime mode changes (handoff).
	CapPersist = "persist"
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
	// HostID is the host's random id (hostid); absent when it could not be
	// read or created. Older stagent versions never send it.
	HostID string `json:"host_id,omitempty"`
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
	// Unwrapped: agents started without a stagent session, newest
	// last_activity_at first; unwrapped.updated replaces the list.
	Unwrapped []UnwrappedLaunch `json:"unwrapped"`
	Seq       int64             `json:"seq"` // latest seq at subscription time
	Missed    []Event           `json:"missed"`
	// Truncated: events between Since and Missed[0] were dropped by retention.
	Truncated bool `json:"truncated"`
}

// UnwrappedUpdatedParams is the unwrapped.updated notification.
type UnwrappedUpdatedParams struct {
	Unwrapped []UnwrappedLaunch `json:"unwrapped"`
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
	// Since is the stream position the client consumed up to (the last
	// `end` it saw); raw mode resumes from there when possible. nil = no
	// resume.
	Since *int64 `json:"since,omitempty"`
}

type AttachResult struct {
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Mode string `json:"mode"`
	// Offset is the stream position right after the first output item,
	// where live output continues.
	Offset int64 `json:"offset"`
	// Resumed: the first output item is the missed range [since, offset)
	// (reset false) instead of a snapshot.
	Resumed bool `json:"resumed"`
}

// OutputParams carries terminal bytes to an attached client. When Reset is
// true the client must clear its terminal (screen + scrollback) and then
// write Data, which is a full ANSI snapshot of the current screen.
type OutputParams struct {
	ID    string `json:"id"`
	Data  []byte `json:"data"` // base64 in JSON
	Reset bool   `json:"reset,omitempty"`
	// End is the stream position after this chunk (raw mode; for a reset
	// snapshot the position the snapshot was taken at).
	End int64 `json:"end,omitempty"`
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
	// HungUp (holder.ended only): the program ended after a client's
	// session.signal hangup.
	HungUp bool `json:"hung_up,omitempty"`
}

// InputParams is applied in order: Text (raw), Paste (bracketed when the
// program enabled bracketed paste), Keys, then a CR if Submit.
type InputParams struct {
	ID     string   `json:"id"`
	Text   string   `json:"text,omitempty"`
	Paste  string   `json:"paste,omitempty"`
	Keys   []string `json:"keys,omitempty"` // names from KeySequences
	Submit bool     `json:"submit,omitempty"`
	// Local marks keyboard input of a terminal on the host (`stagent
	// attach`): it counts as Session.LastLocalInputAt and its focus
	// reports set Session.Focused. The app never sets it.
	Local bool `json:"local,omitempty"`
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
	Command []string `json:"command"`
	// Shell runs the user's login shell instead of Command (which must then
	// be empty).
	Shell bool              `json:"shell,omitempty"`
	Cwd   string            `json:"cwd,omitempty"` // default home
	Cols  int               `json:"cols"`
	Rows  int               `json:"rows"`
	Env   map[string]string `json:"env,omitempty"`
}

type SpawnResult struct {
	Session Session `json:"session"`
}

// Signals for session.signal.
const (
	SignalInterrupt = "interrupt" // Ctrl-C to the PTY
	SignalTerminate = "terminate" // SIGTERM / CTRL_BREAK / TerminateProcess after grace
	SignalKill      = "kill"      // SIGKILL / TerminateProcess
	SignalHangup    = "hangup"    // SIGHUP, as when a terminal closes (Windows: like terminate)
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

// PresenceSetParams tells the daemon whether the app is in the
// foreground. While a connection with a live watch says so, the host
// pushes nothing (the app shows the events itself); a closed connection
// is not in the foreground.
type PresenceSetParams struct {
	Foreground bool `json:"foreground"`
}

type ConfigResult struct {
	Config Config       `json:"config"`
	Notify NotifyStatus `json:"notify"`
}

// NotifyStatus reports the push channels' health (config.get / config.set,
// `stagent doctor`).
type NotifyStatus struct {
	// LastError holds, per channel ("ntfy", "webhook"), the last failed
	// push; a successful push to the channel removes it. Never null.
	LastError map[string]NotifyFailure `json:"last_error"`
}

// ConfigSetParams carries a JSON Merge Patch (RFC 7396) for config.json:
// only the keys it names change, null deletes a key.
type ConfigSetParams struct {
	Config json.RawMessage `json:"config"`
}

// ---------------------------------------------------------------------------
// Local IPC only

const (
	// holder → daemon (persistent connection opened by the holder)
	MethodHolderRegister = "holder.register" // Session → HolderRegisterResult
	MethodHolderUpdate   = "holder.update"   // notification, SessionPatch
	MethodHolderNotify   = "holder.notify"   // notification, HolderNotifyParams
	MethodHolderEnded    = "holder.ended"    // notification, ClosedParams
	// notification, HolderPromptGoneParams: the claude permission menu the
	// daemon asked to watch for (holder.prompt_watch) left the screen.
	MethodHolderPromptGone = "holder.prompt_gone"

	// hook → daemon (one request per connection)
	// Answers {} once the daemon applied the event; a PermissionRequest of a
	// claude running in a stagent session is answered only when its approval
	// closes (the hook stays open until then, with no decision).
	MethodHookEvent = "hook.event" // HookEventParams → {}

	// install / doctor → daemon
	MethodDaemonStatus   = "daemon.status"   // → DaemonStatus
	MethodDaemonShutdown = "daemon.shutdown" // → {}
	// daemon → holder notification sent before a deliberate shutdown
	// (daemon.shutdown, SIGTERM): holders keep retrying but stop
	// auto-starting the daemon, so `uninstall --level stop` sticks.
	MethodDaemonStopping = "daemon.stopping"
	// daemon → holder notification, HolderPromptWatchParams: watch the
	// screen for claude's permission menu while a claude approval of the
	// session holds its hook (on), or stop (off). Holders before 0.4.0
	// ignore it, like any notification they do not know.
	MethodHolderPromptWatch = "holder.prompt_watch"
)

type HolderRegisterResult struct {
	IdleAfterMs int `json:"idle_after_ms"`
}

// HolderPromptWatchParams starts (on) or ends a prompt watch. Gen names
// the watch; a new one starts with every claude approval registered.
type HolderPromptWatchParams struct {
	ID  string `json:"id"`
	Gen int64  `json:"gen"`
	On  bool   `json:"on"`
}

// HolderPromptGoneParams reports that the permission menu watch Gen saw
// is no longer on the screen: the prompt was answered somewhere.
type HolderPromptGoneParams struct {
	ID  string `json:"id"`
	Gen int64  `json:"gen"`
}

// SessionPatch updates a registered session; nil fields are unchanged.
type SessionPatch struct {
	ID             string  `json:"id"`
	State          *string `json:"state,omitempty"`
	StateSource    *string `json:"state_source,omitempty"`
	Title          *string `json:"title,omitempty"`
	LastActivityAt *int64  `json:"last_activity_at,omitempty"`
	// LastLocalInputAt is pushed to watchers at once (the holder sends it at
	// most once per second), unlike last_activity_at.
	LastLocalInputAt *int64  `json:"last_local_input_at,omitempty"`
	Cols             *int    `json:"cols,omitempty"`
	Rows             *int    `json:"rows,omitempty"`
	Mode             *string `json:"mode,omitempty"` // passthrough → detached on handoff
	Attached         *bool   `json:"attached,omitempty"`
	Focused          *bool   `json:"focused,omitempty"`
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
	// RemoteControl: the hook ran with $CLAUDE_CODE_BRIDGE_SESSION_ID, set
	// while claude is connected to Remote Control.
	RemoteControl bool `json:"remote_control,omitempty"`

	// How the agent was started, sent when SessionID is empty (see
	// UnwrappedLaunch). ShellWrapper: $STAGENT_SHELL_WRAPPER is set.
	// Entrypoint: $CLAUDE_CODE_ENTRYPOINT. ParentTTY: the harness (the
	// hook's nearest ancestor that is not a shell) reads a terminal — its
	// stdin on Linux, its controlling terminal on macOS; nil when unknown
	// (Windows). ParentBatch:
	// the harness's command line asks for a non-interactive run, by the
	// shell wrapper's rule (claude -p / --print; codex exec / e; omp -p /
	// --print or --mode other than text).
	ShellWrapper bool   `json:"shell_wrapper,omitempty"`
	Entrypoint   string `json:"entrypoint,omitempty"`
	ParentTTY    *bool  `json:"parent_tty,omitempty"`
	ParentBatch  bool   `json:"parent_batch,omitempty"`
}

type DaemonStatus struct {
	PID       int               `json:"pid"`
	Version   string            `json:"version"`
	StartedAt int64             `json:"started_at"`
	Sessions  int               `json:"sessions"`
	Bridges   int               `json:"bridges"`
	Unwrapped []UnwrappedLaunch `json:"unwrapped"`
	// BootstrapSwapped (macOS only; absent elsewhere): the daemon moved
	// itself to the user's per-user bootstrap port at start, so it and what
	// it starts reach the network after the user logs out of the GUI.
	BootstrapSwapped *bool `json:"bootstrap_swapped,omitempty"`
	// BootstrapError is why that failed.
	BootstrapError string `json:"bootstrap_error,omitempty"`
}
