package hook

import (
	"encoding/json"
	"testing"
	"time"
)

// Copilot CLI's hooks file passes the event as an argument; with the
// PascalCase registration the payload already has Claude Code's snake_case
// fields and is forwarded as it is.
func TestCopilotHookForwardsSnakeCasePayload(t *testing.T) {
	got := fakeDaemon(t, 0)
	payload := `{"hook_event_name":"Stop","session_id":"0193","timestamp":"2026-10-20T01:00:00Z","cwd":"/p","transcript_path":"/h/.copilot/session-state/0193/events.jsonl","stop_reason":"end_turn","stop_hook_active":false}`
	runHook([]string{"copilot", "Stop"}, payload, "")
	p := <-got
	if p.Harness != "copilot" || p.Event != "Stop" || string(p.Payload) != payload {
		t.Fatalf("forwarded %+v", p)
	}
}

// The reference shows the notification payload with camelCase sessionId
// (and the camelCase registration sends every payload so): the hook adds
// the snake_case fields the daemon reads, keeping the rest.
func TestCopilotHookAddsSnakeCaseFields(t *testing.T) {
	got := fakeDaemon(t, 0)
	runHook([]string{"copilot", "Notification"}, `{"sessionId":"0193","timestamp":1760000000000,"cwd":"/p","hook_event_name":"Notification","message":"Copilot needs your permission","title":"Permission needed","notification_type":"permission_prompt"}`, "")
	p := <-got
	var pl map[string]any
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		t.Fatal(err)
	}
	if p.Event != "Notification" || pl["session_id"] != "0193" || pl["sessionId"] != "0193" || pl["notification_type"] != "permission_prompt" || pl["message"] != "Copilot needs your permission" {
		t.Fatalf("forwarded %s %s", p.Event, p.Payload)
	}

	runHook([]string{"copilot", "PermissionRequest"}, `{"sessionId":"0193","toolName":"bash","toolArgs":{"command":"rm -rf build"},"transcriptPath":"/t.jsonl"}`, "")
	p = <-got
	var pr struct {
		SessionID      string `json:"session_id"`
		ToolName       string `json:"tool_name"`
		TranscriptPath string `json:"transcript_path"`
		ToolInput      struct {
			Command string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(p.Payload, &pr); err != nil || pr.SessionID != "0193" || pr.ToolName != "bash" || pr.ToolInput.Command != "rm -rf build" || pr.TranscriptPath != "/t.jsonl" {
		t.Fatalf("forwarded %s (%v)", p.Payload, err)
	}
}

// Copilot CLI shows its permission menu only once the PermissionRequest
// hook returned, so that hook never waits on the daemon.
func TestCopilotPermissionRequestDoesNotWait(t *testing.T) {
	hold := callTimeout + 500*time.Millisecond
	fakeDaemon(t, hold)
	start := time.Now()
	runHook([]string{"copilot", "PermissionRequest"}, `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"Bash"}`, "s")
	if d := time.Since(start); d >= hold {
		t.Fatalf("copilot PermissionRequest waited %v", d)
	}
}
