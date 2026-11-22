package hook

import (
	"encoding/json"
)

// antigravityHook: stagent's group in Antigravity CLI's hooks file
// (install/antigravity.go) passes the event as an argument, with Antigravity
// CLI's own event names, and the payloads have camelCase fields of its own
// (conversationId, transcriptPath, cwd, lastUserInput, toolCall: {id, name,
// args}, invocationNum, terminationReason). The hook rewrites them into
// Claude Code's events and snake_case fields, which the daemon reads:
//
//   - PreInvocation runs before every model call of a turn; the first
//     (invocationNum 0) comes right after a prompt was submitted and is
//     UserPromptSubmit, with lastUserInput as its prompt. The later ones,
//     like PostInvocation, tell nothing the daemon uses and are dropped.
//   - Stop ends every turn; one the user cancelled (terminationReason
//     USER_CANCELED) is StopCancelled.
//   - SessionStart (a new conversation, at its first prompt), PreToolUse
//     and PostToolUse keep their names.
//
// How Antigravity CLI was started is read from its process
// (harness.BatchArgs): its hooks are its children (through `sh -c`).
// Antigravity CLI reads a JSON object on the hook's stdout (output). For
// PreToolUse the object must say {"decision": "ask"}: that leaves the call
// to its own permission rules (Auto mode still runs what it allows without
// asking), while an object without a decision — {} — denies the call
// ("tool call denied by pre-tool hook"). Every other event gets {}, which
// asks nothing of it (a Stop without "decision": "continue" stops).
var antigravityHook = hookHarness{
	payload: antigravityPayload,
	event:   antigravityEvent,
	local:   antigravityLocal,
	output:  antigravityOutput,
}

func antigravityOutput(event string) string {
	if event == "PreToolUse" {
		return `{"decision":"ask"}`
	}
	return "{}"
}

// antigravityFields maps the camelCase fields of Antigravity CLI's
// payloads to the snake_case ones the daemon reads.
var antigravityFields = [][2]string{
	{"hookEventName", "hook_event_name"},
	{"conversationId", "session_id"},
	{"transcriptPath", "transcript_path"},
}

// antigravityPayload adds the snake_case fields to an Antigravity CLI
// payload: those of antigravityFields, tool_name and tool_input from
// toolCall, and prompt from lastUserInput on the first PreInvocation of a
// turn (the prompt that started it).
func antigravityPayload(payload []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil || m == nil {
		return payload
	}
	added := false
	set := func(k string, v json.RawMessage) {
		if _, has := m[k]; !has && len(v) > 0 {
			m[k], added = v, true
		}
	}
	for _, f := range antigravityFields {
		set(f[1], m[f[0]])
	}
	var tc struct {
		Name json.RawMessage `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if json.Unmarshal(m["toolCall"], &tc) == nil {
		set("tool_name", tc.Name)
		set("tool_input", tc.Args)
	}
	if firstInvocation(payload) {
		set("prompt", m["lastUserInput"])
	}
	if !added {
		return payload
	}
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return b
}

const (
	antigravityPreInvocation  = "PreInvocation"
	antigravityPostInvocation = "PostInvocation"
	antigravityStop           = "Stop"
)

// antigravityEvent names Antigravity CLI's events as the daemon reads them.
func antigravityEvent(event string, payload []byte) string {
	switch event {
	case antigravityPreInvocation:
		if firstInvocation(payload) {
			return "UserPromptSubmit"
		}
	case antigravityStop:
		var p struct {
			Reason string `json:"terminationReason"`
		}
		json.Unmarshal(payload, &p)
		if p.Reason == "USER_CANCELED" {
			return "StopCancelled"
		}
	}
	return event
}

// antigravityLocal drops the model calls after a turn's first.
func antigravityLocal(event string, _ []byte) bool {
	return event == antigravityPreInvocation || event == antigravityPostInvocation
}

// firstInvocation: payload is of the first model call of a turn
// (invocationNum 0, which protojson leaves out as the default).
func firstInvocation(payload []byte) bool {
	var p struct {
		Event string `json:"hookEventName"`
		N     *int   `json:"invocationNum"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Event != antigravityPreInvocation {
		return false
	}
	return p.N == nil || *p.N == 0
}
