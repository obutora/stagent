package hook

import (
	"encoding/json"
)

// copilotHook: stagent's hooks file for Copilot CLI (install/copilot.go)
// passes the event as an argument and registers the PascalCase event names,
// whose payloads have Claude Code's snake_case fields. The camelCase form
// (sessionId, transcriptPath, toolName, toolArgs), which the hooks
// reference shows for the notification payload, is read too: the
// snake_case fields the daemon reads are added from it.
var copilotHook = hookHarness{payload: copilotPayload}

// copilotCamel maps the camelCase fields of Copilot CLI's payloads to the
// snake_case ones of its VS Code compatible form.
var copilotCamel = [][2]string{
	{"sessionId", "session_id"},
	{"transcriptPath", "transcript_path"},
	{"toolName", "tool_name"},
	{"toolArgs", "tool_input"},
}

func copilotPayload(payload []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil || m == nil {
		return payload
	}
	added := false
	for _, f := range copilotCamel {
		if v, ok := m[f[0]]; ok {
			if _, has := m[f[1]]; !has {
				m[f[1]], added = v, true
			}
		}
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
