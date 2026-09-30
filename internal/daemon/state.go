package daemon

import (
	"encoding/json"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// overrideGraceMs: an activity "working" report must come this long after a
// hook-derived state to override it. Harnesses redraw (spinners, the
// approval dialog itself) right after firing a hook; that output must not
// read as "the user answered in the terminal".
const overrideGraceMs = 1000

// stateTrack holds what each source last said about a session. The merged
// state is derived from it by merged(); inputs are applied by the pure
// methods below so the priority rules can be table-tested.
type stateTrack struct {
	Hook   string // hook-derived state, "" when none is in force
	HookAt int64  // unix ms the hook state was set

	Holder       string // latest state reported by the holder
	HolderSource string // terminal | activity | process
	HolderAt     int64

	Exited bool // the child process ended
}

// holderReport applies a state the holder derived from the PTY (terminal
// escapes, output activity, process exit).
func (t stateTrack) holderReport(state, source string, at int64) stateTrack {
	if state == "" {
		return t
	}
	if source == "" {
		source = wire.SourceActivity
	}
	if state == wire.StateExited {
		t.Exited = true
	}
	t.Holder, t.HolderSource, t.HolderAt = state, source, at
	// Output activity after a hook state means the harness moved on without
	// telling us (e.g. the user answered a prompt in the terminal): the hook
	// state is stale and lower sources take over again.
	if t.Hook != "" && state == wire.StateWorking && source == wire.SourceActivity && at >= t.HookAt+overrideGraceMs {
		t.Hook, t.HookAt = "", 0
	}
	return t
}

// hookState applies a state derived from a harness hook.
func (t stateTrack) hookState(state string, at int64) stateTrack {
	t.Hook, t.HookAt = state, at
	return t
}

// hookClear drops the hook-derived state (SessionEnd: the harness left, the
// PTY-derived state is all that remains).
func (t stateTrack) hookClear() stateTrack {
	t.Hook, t.HookAt = "", 0
	return t
}

// exited marks the child process as ended.
func (t stateTrack) exited() stateTrack {
	t.Exited = true
	return t
}

// merged is the session state and the source it came from: process exit
// beats everything, then a hook state in force, then the holder's report.
func (t stateTrack) merged() (state, source string) {
	switch {
	case t.Exited:
		return wire.StateExited, wire.SourceProcess
	case t.Hook != "":
		return t.Hook, wire.SourceHook
	case t.Holder != "":
		return t.Holder, t.HolderSource
	}
	return wire.StateIdle, wire.SourceActivity
}

// ---------------------------------------------------------------------------
// Hook events

// Normalized hook event names (Claude's; Codex and the omp extension use
// the same names, Codex's legacy notify program maps to Stop).
const (
	hookSessionStart      = "SessionStart"
	hookSessionEnd        = "SessionEnd"
	hookUserPromptSubmit  = "UserPromptSubmit"
	hookStop              = "Stop"
	hookNotification      = "Notification"
	hookPermissionRequest = "PermissionRequest"
	codexNotify           = "notify"
)

// hookPayload is the union of the fields stagent reads from hook payloads
// of every harness.
type hookPayload struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	Prompt               string          `json:"prompt"`
	Message              string          `json:"message"`
	NotificationType     string          `json:"notification_type"`
	ToolName             string          `json:"tool_name"`
	ToolInput            json.RawMessage `json:"tool_input"`
	LastAssistantMessage string          `json:"last_assistant_message"`

	// Codex notify program (agent-turn-complete).
	Type                     string `json:"type"`
	ThreadID                 string `json:"thread-id"`
	LastAssistantMessageDash string `json:"last-assistant-message"`
}

func parseHookPayload(raw json.RawMessage) hookPayload {
	var p hookPayload
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p) // a malformed payload still carries the event name
	}
	if p.SessionID == "" {
		p.SessionID = p.ThreadID
	}
	if p.LastAssistantMessage == "" {
		p.LastAssistantMessage = p.LastAssistantMessageDash
	}
	return p
}

// hookEffect is what one hook event means.
type hookEffect struct {
	event string // normalized event name
	// state is the hook-derived session state to set ("" = unchanged).
	state string
	// clear drops the hook-derived state (SessionEnd).
	clear bool
	// turnComplete emits a turn_complete notification.
	turnComplete bool
	// approval: a blocking permission request.
	approval bool
}

// classifyHook maps a harness hook event to its effect. event is the name
// given on the command line or in the payload.
func classifyHook(event string, p hookPayload) hookEffect {
	if event == "" {
		event = p.HookEventName
	}
	if event == codexNotify || p.Type == "agent-turn-complete" {
		event = hookStop
	}
	e := hookEffect{event: event}
	switch event {
	case hookUserPromptSubmit:
		e.state = wire.StateWorking
	case hookStop:
		e.state = wire.StateIdle
		e.turnComplete = true
	case hookPermissionRequest:
		e.state = wire.StateNeedsApproval
		e.approval = true
	case hookNotification:
		e.state = notificationState(p)
	case hookSessionEnd:
		e.clear = true
	}
	return e
}

// notificationState maps Claude's Notification hook to a state:
// permission_prompt → needs_approval; idle_prompt and elicitation dialogs →
// waiting_input; auth_success and unknown types without a message → none.
// Payloads without notification_type (older Claude Code) are classified by
// their message.
func notificationState(p hookPayload) string {
	switch p.NotificationType {
	case "permission_prompt":
		return wire.StateNeedsApproval
	case "idle_prompt", "elicitation_dialog":
		return wire.StateWaitingInput
	case "auth_success":
		return ""
	}
	msg := strings.ToLower(p.Message)
	switch {
	case msg == "":
		return ""
	case strings.Contains(msg, "permission") || strings.Contains(msg, "approv"):
		return wire.StateNeedsApproval
	}
	return wire.StateWaitingInput
}

// notifyReasonForTransition is the notification a debounced state change
// raises ("" = none): entering needs_approval from any other state,
// entering waiting_input from working.
func notifyReasonForTransition(from, to string) string {
	switch {
	case to == wire.StateNeedsApproval && from != wire.StateNeedsApproval:
		return reasonNeedsApproval
	case to == wire.StateWaitingInput && from == wire.StateWorking:
		return reasonWaitingInput
	}
	return ""
}

// Notification reasons (wire.NotificationData.Reason).
const (
	reasonWaitingInput  = "waiting_input"
	reasonNeedsApproval = "needs_approval"
	reasonTurnComplete  = "turn_complete"
	reasonExited        = "exited"
	reasonTerminal      = "terminal"
	reasonTest          = "test"
)
