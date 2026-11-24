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
	MethodPresenceSet: true, MethodTaskList: true, MethodTaskSetup: true,
	MethodScheduleList: true, MethodScheduleCreate: true, MethodScheduleRuns: true, MethodSchedulePreview: true,
	MethodScheduleUpdate: true, MethodScheduleRemove: true, MethodSchedulePause: true, MethodScheduleResume: true,
	MethodScheduleRunNow: true,
	MethodScheduleOutput: true,
	MethodUsageGet:       true,
	MethodStoppedRemove:  true,
	MethodRelayRecord:    true,
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
	// CapPushChat: notify.chat (Discord, Slack, Telegram) and notify.test
	// channels. Older stagent versions store notify.chat without sending.
	CapPushChat = "push_chat"
	// CapPersist: session.spawn shell, attach resume (since/offset/end),
	// signal hangup, runtime mode changes (handoff).
	CapPersist = "persist"
	// CapPersistShell (Windows only): session.spawn shell works there.
	// Windows bridges before it announced CapPersist but refused shells.
	CapPersistShell = "persist_shell"
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
	// Blocked is set when another user owns this host's stagent location
	// (the run directory, a socket or the daemon's named pipe), so no kept
	// shell can work until the host's administrator removes it. Older
	// stagent versions never send it.
	Blocked *Blocked `json:"blocked,omitempty"`
}

// Blocked names a stagent location another user owns.
type Blocked struct {
	Path  string `json:"path"`  // e.g. /tmp/stagent-1000, \\.\pipe\stagent-<SID>
	Owner string `json:"owner"` // user name when known, else uid / SID
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
	// Tasks: every task, in the order they were created; task.updated and
	// task.removed follow the changes.
	Tasks []Task `json:"tasks"`
	// Stopped: the stopped agents (Linux and macOS; empty elsewhere),
	// newest found_at first; stopped.updated replaces the list. Older
	// stagent versions omit it.
	Stopped []StoppedAgent `json:"stopped"`
	Seq     int64          `json:"seq"` // latest seq at subscription time
	Missed  []Event        `json:"missed"`
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
	// session.signal hangup or (Windows) its passthrough console closed.
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
	// TaskID ties the session to that task: the way to add a session to
	// an in_place task (a worktree task's sessions are those in its
	// worktree anyway). Unknown ids tie nothing.
	TaskID string `json:"task_id,omitempty"`
}

type SpawnResult struct {
	Session Session `json:"session"`
}

// Signals for session.signal.
const (
	SignalInterrupt = "interrupt" // Ctrl-C to the PTY
	SignalTerminate = "terminate" // SIGTERM; Windows: ^C, TerminateProcess after 3 s
	SignalKill      = "kill"      // SIGKILL / TerminateProcess
	SignalHangup    = "hangup"    // SIGHUP, as when a terminal closes; Windows: like terminate
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
	// LastError holds, per channel ("ntfy", "webhook", "chat"), the last
	// failed push; a successful push to the channel removes it. Never null.
	LastError map[string]NotifyFailure `json:"last_error"`
}

// ConfigSetParams carries a JSON Merge Patch (RFC 7396) for config.json:
// only the keys it names change, null deletes a key.
type ConfigSetParams struct {
	Config json.RawMessage `json:"config"`
}

// NotifyTestParams picks the channels notify.test pushes to (Channel*);
// none means every enabled channel.
type NotifyTestParams struct {
	Channels []string `json:"channels,omitempty"`
}

// ---------------------------------------------------------------------------
// Local IPC only

const (
	// holder → daemon (persistent connection opened by the holder)
	MethodHolderRegister = "holder.register" // Session → HolderRegisterResult
	MethodHolderUpdate   = "holder.update"   // notification, SessionPatch
	MethodHolderNotify   = "holder.notify"   // notification, HolderNotifyParams
	MethodHolderEnded    = "holder.ended"    // notification, ClosedParams
	// notification, HolderPromptGoneParams: the claude or Codex permission
	// menu the daemon asked to watch for (holder.prompt_watch) left the
	// screen.
	MethodHolderPromptGone = "holder.prompt_gone"
	// notification, HolderPromptGoneParams (since 0.20.0): the question UI
	// the daemon asked to watch for (holder.question_watch) left the
	// screen: the question was answered or cancelled there.
	MethodHolderQuestionGone = "holder.question_gone"
	// notification, HolderPromptGoneParams: the permission menu a watch
	// started with shown (HolderPromptWatchParams.Shown) looks for came on
	// the screen. Its watch goes on, to report when it leaves.
	MethodHolderPromptShown = "holder.prompt_shown"

	// hook → daemon (one request per connection)
	// Answers {} once the daemon applied the event; a PermissionRequest of a
	// claude running in a stagent session is answered only when its approval
	// closes (the hook stays open until then, with no decision), except one
	// naming its question tool, which is answered at once.
	MethodHookEvent = "hook.event" // HookEventParams → {}

	// bridge → daemon (since 0.21.1), SessionEndParams → {}: end a session
	// its holder does not answer for — its socket is gone, nothing accepts
	// on it, or the holder went away or stayed silent on session.signal.
	// The daemon signals the session's program (and kills it and the
	// holder when they outlast the grace) and records the end; a session it
	// does not list as live is already over. Refused to coding agents'
	// process trees.
	MethodSessionEnd = "session.end"

	// install / doctor → daemon
	MethodDaemonStatus   = "daemon.status"   // → DaemonStatus
	MethodDaemonShutdown = "daemon.shutdown" // → {}
	// daemon → holder notification sent before a deliberate shutdown
	// (daemon.shutdown, SIGTERM): holders keep retrying but stop
	// auto-starting the daemon, so `uninstall --level stop` sticks.
	MethodDaemonStopping = "daemon.stopping"
	// daemon → holder notification, HolderPromptWatchParams: watch the
	// screen for the permission menu while an approval of the session from
	// a harness whose menu is watched is pending (on), or while such a
	// harness checks a tool call's permission and may show its menu with
	// no hook (Shown), or stop (off). Holders before 0.4.0 ignore it, like
	// any notification they do not know; 0.5.0 and earlier know only
	// claude's menu, 0.15.0 and earlier no Copilot CLI menu, 0.21.1 and
	// earlier no Qwen Code menu, 0.22.1 and earlier neither Antigravity
	// CLI's menu nor Shown.
	MethodHolderPromptWatch = "holder.prompt_watch"
	// daemon → holder notification, HolderPromptWatchParams (since
	// 0.20.0): watch the screen for the question UI of the harness while a
	// question of the session is in force (on), or stop (off); Gen names
	// the watch (one per question in force). Holders before 0.20.0 ignore
	// it: their sessions' questions go with the next hook only.
	MethodHolderQuestionWatch = "holder.question_watch"

	// `stagent explain --session` → holder, ScreenExplainParams →
	// ScreenExplain: the holder reads its screen with the screen rules it
	// started with and says which of them matched. Holders that predate it
	// answer unknown_method.
	MethodSessionExplain = "session.explain"
)

// ScreenExplainParams asks a holder to explain its screen, read as
// Harness ("": as the harness the session runs).
type ScreenExplainParams struct {
	ID      string `json:"id"`
	Harness string `json:"harness,omitempty"`
}

// ScreenExplain is what `stagent explain` reports: the checks the holder
// makes on a screen, read as Harness ("" for no harness in particular),
// with the rules that matched, and the override files of RulesDir.
type ScreenExplain struct {
	Harness  string            `json:"harness"`
	RulesDir string            `json:"rules_dir"`
	Files    []ScreenRulesFile `json:"files"`
	Checks   []ScreenCheck     `json:"checks"`
}

// ScreenRulesFile is an override file of the screen rules (`stagent
// explain`, `stagent doctor`): Name is the rules it overrides (common or a
// harness id); a file that is not Used keeps the built-in rules, Error
// says why.
type ScreenRulesFile struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Used  bool   `json:"used"`
	Error string `json:"error,omitempty"`
}

// Checks of ScreenCheck.
const (
	// CheckMenu: a menu of any harness, on which a chat message is
	// refused (menu_open).
	CheckMenu = "menu"
	// CheckPermission: the permission menu a prompt watch looks for.
	CheckPermission = "permission"
	// CheckQuestion: the question UI a question watch looks for.
	CheckQuestion = "question"
)

// ScreenCheck is one check of a screen as Harness draws it ("" any
// harness). Rules are the rules that matched, or for no match the rules of
// Harness that were tried; Lines the lines that matched (the options of a
// menu, a question UI's header and hint or its box's edges). Signature is
// a permission menu's (holder.prompt_watch).
type ScreenCheck struct {
	Check     string          `json:"check"`
	Harness   string          `json:"harness"`
	Matched   bool            `json:"matched"`
	Rules     []ScreenRuleUse `json:"rules"`
	Lines     []ScreenLine    `json:"lines"`
	Signature string          `json:"signature,omitempty"`
}

// ScreenRuleUse is one rule, "<name>.<rule>" (codex.question,
// common.menu), as Rule has it: built-in, or from the override File.
type ScreenRuleUse struct {
	ID     string          `json:"id"`
	Origin string          `json:"origin"` // "builtin" or "override"
	File   string          `json:"file,omitempty"`
	Rule   json.RawMessage `json:"rule"`
}

// ScreenLine is a line of the screen; Line 1 is the top row.
type ScreenLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type HolderRegisterResult struct {
	IdleAfterMs int `json:"idle_after_ms"`
}

// SessionEndParams names the session session.end ends and the signal
// (SignalHangup, SignalTerminate or SignalKill) its program gets first.
type SessionEndParams struct {
	ID     string `json:"id"`
	Signal string `json:"signal"`
}

// HolderPromptWatchParams starts (on) or ends a prompt watch. Gen names
// the watch; a new one starts with every approval registered of a harness
// whose menu is watched, and with every permission check of one whose
// menu comes with no hook (Antigravity CLI). Harness (since 0.16.0, with
// on) is the harness whose menu the approval brings: Copilot CLI's menu
// has claude's cursor (❯), so the holder reads the screen as that harness
// draws it. Empty from older daemons: every harness's menus are looked
// for. Shown (with on): no approval is registered yet; the holder also
// reports when the menu comes on the screen (holder.prompt_shown), which
// registers it.
type HolderPromptWatchParams struct {
	ID      string `json:"id"`
	Gen     int64  `json:"gen"`
	On      bool   `json:"on"`
	Harness string `json:"harness,omitempty"`
	Shown   bool   `json:"shown,omitempty"`
}

// HolderPromptGoneParams reports that the permission menu watch Gen saw
// is no longer on the screen: the prompt was answered somewhere
// (holder.prompt_gone); or that it came on the screen
// (holder.prompt_shown).
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
	LastLocalInputAt *int64 `json:"last_local_input_at,omitempty"`
	// LastInputAt rides along with the next update; the holder sets it
	// before the input reaches the program, so it never comes after the
	// state that input's output brings.
	LastInputAt *int64  `json:"last_input_at,omitempty"`
	Cols        *int    `json:"cols,omitempty"`
	Rows        *int    `json:"rows,omitempty"`
	Mode        *string `json:"mode,omitempty"` // passthrough → detached on handoff
	Attached    *bool   `json:"attached,omitempty"`
	Focused     *bool   `json:"focused,omitempty"`
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
	// shell wrapper's rule (the harness's Batch in the registry, e.g.
	// claude -p / --print, codex exec). SSH: $SSH_CONNECTION is set.
	// TerminalAncestor (Windows; nil elsewhere or unknown): a shell
	// (PowerShell, pwsh, cmd, bash, sh) runs above the harness. Originator
	// (codex): session_meta.originator of the conversation's rollout file
	// (codex-tui, codex_exec, "Codex Desktop", …); Subagent: that
	// conversation is a subagent thread of another one.
	ShellWrapper     bool   `json:"shell_wrapper,omitempty"`
	Entrypoint       string `json:"entrypoint,omitempty"`
	ParentTTY        *bool  `json:"parent_tty,omitempty"`
	ParentBatch      bool   `json:"parent_batch,omitempty"`
	SSH              bool   `json:"ssh,omitempty"`
	TerminalAncestor *bool  `json:"terminal_ancestor,omitempty"`
	Originator       string `json:"originator,omitempty"`
	Subagent         bool   `json:"subagent,omitempty"`
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
