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
	// Question is the question a hook-derived waiting_input is about
	// (nil: none); it goes with that state.
	Question *wire.Question

	Holder       string // latest state reported by the holder
	HolderSource string // terminal | activity | process
	HolderAt     int64
	// InputAt is when input was last written to the program (unix ms,
	// Session.LastInputAt), as the holder reported it.
	InputAt int64

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
	// state is stale and lower sources take over again. Not while a
	// question is in force: output then is the user moving through its
	// UI (the highlight, the tabs, a multi-select's checks), and the
	// question goes with a hook or when its UI leaves the screen
	// (questionOffScreen). A session that waits for the user
	// (needs_approval, waiting_input) keeps waiting until input reached
	// the program after the hook: output without it is the harness
	// redrawing (a resize when the session is opened again), not an answer.
	if t.Hook != "" && t.Question == nil && state == wire.StateWorking && source == wire.SourceActivity && at >= t.HookAt+overrideGraceMs &&
		(!waitsForUser(t.Hook) || t.InputAt > t.HookAt) {
		t.Hook, t.HookAt = "", 0
	}
	return t
}

// quietEnded applies the holder's report of a state (holderReport, just
// before) to a session of a harness whose turn may end with no hook
// (daemonHarness.turnEndsUnheard): when its output went quiet (idle from
// output activity) while a hook-derived working is in force, the turn
// ended unheard (an Esc), and that working goes; the holder's idle takes
// over. A waiting state stays: the harness waits quietly for the answer.
// The harness draws a spinner for as long as it works, so its output
// never goes quiet amid a turn.
func (t stateTrack) quietEnded() stateTrack {
	if t.Hook == wire.StateWorking && t.Holder == wire.StateIdle && t.HolderSource == wire.SourceActivity {
		t.Hook, t.HookAt = "", 0
	}
	return t
}

// holderInput applies the holder's report of when input was last written
// to the program.
func (t stateTrack) holderInput(at int64) stateTrack {
	t.InputAt = max(t.InputAt, at)
	return t
}

// waitsForUser reports whether a hook-derived state waits for the user's
// answer.
func waitsForUser(state string) bool {
	return state == wire.StateNeedsApproval || state == wire.StateWaitingInput
}

// hookState applies a state derived from a harness hook. A question in
// force stays with a later waiting_input (a reminder about it) and goes
// with any other state.
func (t stateTrack) hookState(state string, at int64) stateTrack {
	t.Hook, t.HookAt = state, at
	if state != wire.StateWaitingInput {
		t.Question = nil
	}
	return t
}

// hookQuestion applies a question hook: the session waits for the answer
// to q.
func (t stateTrack) hookQuestion(q *wire.Question, at int64) stateTrack {
	t.Hook, t.HookAt, t.Question = wire.StateWaitingInput, at, q
	return t
}

// hookClear drops the hook-derived state (SessionEnd: the harness left, the
// PTY-derived state is all that remains).
func (t stateTrack) hookClear() stateTrack {
	t.Hook, t.HookAt, t.Question = "", 0, nil
	return t
}

// questionOffScreen applies the holder's report that the question's UI
// left the screen (holder.question_gone): it was answered or cancelled
// there — an Esc sends no hook. The question and its waiting_input go, the
// PTY-derived state takes over.
func (t stateTrack) questionOffScreen() stateTrack {
	if t.Question == nil {
		return t
	}
	return t.hookClear()
}

// question is the question the session waits on: the hook's, while its
// waiting_input is the merged state.
func (t stateTrack) question() *wire.Question {
	if t.Exited || t.Hook != wire.StateWaitingInput {
		return nil
	}
	return t.Question
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

// Normalized hook event names (Claude's; Codex and the omp and Pi
// extensions use the same names, Codex's legacy notify program maps to
// Stop).
const (
	hookSessionStart      = "SessionStart"
	hookSessionEnd        = "SessionEnd"
	hookUserPromptSubmit  = "UserPromptSubmit"
	hookStop              = "Stop"
	hookStopFailure       = "StopFailure" // the turn ended on an API error (Qwen Code)
	hookNotification      = "Notification"
	hookPermissionRequest = "PermissionRequest"
	hookPreToolUse        = "PreToolUse"
	hookPostToolUse       = "PostToolUse"
	// hookPermissionReplied: the harness's permission menu was answered,
	// wherever that happened (OpenCode's permission.replied).
	hookPermissionReplied = "PermissionReplied"
	// hookStopCancelled: the user cut the turn short, turning a permission
	// menu down or dismissing it (Grok Build), or stopping the turn
	// (OpenCode's session.execution.interrupted).
	hookStopCancelled = "StopCancelled"
	codexNotify       = "notify"
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
	// ask: the agent asks the user a question through its question tool
	// (harness.Harness.QuestionTool); the hook's tool input holds the
	// questions.
	ask bool
	// permissionCheck: the harness checks a tool call's permission and
	// may show its menu next (daemonHarness.checkHook); the tool is kept
	// for that menu's approval.
	permissionCheck bool
	// answered: the session's pending approvals were answered.
	answered bool
}

// classifyHook maps a hook event of harness harnessID to its effect. event
// is the name given on the command line or in the payload.
func classifyHook(harnessID, event string, p hookPayload) hookEffect {
	if event == "" {
		event = p.HookEventName
	}
	if event == codexNotify || p.Type == "agent-turn-complete" {
		event = hookStop
	}
	dh := daemonHarnesses[harnessID]
	e := hookEffect{event: event}
	switch event {
	case hookUserPromptSubmit:
		e.state = wire.StateWorking
	case hookStop, hookStopFailure:
		e.state = wire.StateIdle
		e.turnComplete = true
	case hookPermissionRequest:
		if isQuestionTool(harnessID, p.ToolName) {
			e.state, e.ask = wire.StateWaitingInput, true
			break
		}
		if dh.checkHook == hookPermissionRequest {
			e.permissionCheck = true
			break
		}
		e.state = wire.StateNeedsApproval
		e.approval = true
	case hookPreToolUse:
		if isQuestionTool(harnessID, p.ToolName) {
			e.state, e.ask = wire.StateWaitingInput, true
		} else if dh.checkHook == hookPreToolUse {
			e.permissionCheck = true
		}
	case hookPostToolUse:
		if isQuestionTool(harnessID, p.ToolName) {
			e.state = wire.StateWorking
		}
	case hookNotification:
		e.state = notificationState(p)
		// A harness that checks first shows its menu with this one.
		e.approval = dh.checkHook != "" && p.NotificationType == "permission_prompt"
	case hookStopCancelled:
		// The turn ended without its approvals, by the user's own hand:
		// nothing to tell them.
		e.state, e.answered = wire.StateIdle, true
	case hookPermissionReplied:
		// Allowed, the tool runs; turned down, the turn ends (Stop).
		e.state, e.answered = wire.StateWorking, true
	case hookSessionEnd:
		e.clear = true
	}
	return e
}

// notificationState maps Claude's (and Copilot CLI's) Notification hook to
// a state: permission_prompt → needs_approval; idle_prompt and elicitation
// dialogs → waiting_input; auth_success, Copilot CLI's notices of
// background work and unknown types without a message → none. Payloads
// without notification_type (older Claude Code) are classified by their
// message.
func notificationState(p hookPayload) string {
	switch p.NotificationType {
	case "permission_prompt":
		return wire.StateNeedsApproval
	case "idle_prompt", "elicitation_dialog":
		return wire.StateWaitingInput
	case "auth_success", "shell_completed", "shell_detached_completed", "agent_completed", "agent_idle":
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
// entering waiting_input from working, and entering it with a question
// (question) from any other state. A question is always a fresh ask, never
// a reminder, and the agent may ask it within the debounce of the prompt
// (claude calls AskUserQuestion some 1.6 s after UserPromptSubmit), so the
// working in between is never recorded and the change is from idle (#1846).
func notifyReasonForTransition(from, to string, question bool) string {
	switch {
	case to == wire.StateNeedsApproval && from != wire.StateNeedsApproval:
		return reasonNeedsApproval
	case to == wire.StateWaitingInput && from == wire.StateWorking:
		return reasonWaitingInput
	case to == wire.StateWaitingInput && from != wire.StateWaitingInput && question:
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
