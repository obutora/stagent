package hook

import (
	"encoding/json"

	"github.com/obutora/stagent/internal/wire"
)

// grokHook: stagent's hooks file for Grok Build (install/grok.go) passes
// the event as an argument; the payloads carry Claude Code's snake_case
// fields (session_id, transcript_path, notification_type, tool_name, …)
// beside Grok Build's camelCase ones. How Grok Build was started is read
// from its process (harness.BatchArgs): its hooks are its children. It
// sends Stop twice: when a turn ends (reason end_turn) and when it exits
// (reason shutdown, beside SessionEnd), which is no turn and is dropped.
var grokHook = hookHarness{local: grokLocal}

func grokLocal(event string, payload []byte) bool {
	if event != "Stop" {
		return false
	}
	var p struct {
		Reason string `json:"reason"`
	}
	json.Unmarshal(payload, &p)
	return p.Reason != "" && p.Reason != "end_turn"
}

// hookRunner is the harness that runs the hook, told by its environment
// when it runs other harnesses' hooks too ("" otherwise): Grok Build runs
// those of Claude Code's settings (~/.claude/settings.json) as well as its
// own, with GROK_HOOK_EVENT set.
func hookRunner(getenv func(string) string) string {
	if getenv("GROK_HOOK_EVENT") != "" {
		return wire.HarnessGrok
	}
	return ""
}
