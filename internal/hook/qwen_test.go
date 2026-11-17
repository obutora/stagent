package hook

import (
	"testing"
	"time"
)

// Qwen Code's payloads are Claude Code's: the hook forwards them as they
// are, the event named by the argument the installed command passes.
func TestQwenHookForwardsPayload(t *testing.T) {
	got := fakeDaemon(t, 0)
	payload := `{"session_id":"a2730fef-0f6d-48da-9317-c9dc99d18525","transcript_path":"/home/u/.qwen/projects/-home-u-proj/chats/a2730fef-0f6d-48da-9317-c9dc99d18525.jsonl","cwd":"/home/u/proj","hook_event_name":"Notification","timestamp":"2026-11-16T16:24:52.681Z","permission_mode":"default","prompt_id":"a2730fef-0f6d-48da-9317-c9dc99d18525########0","message":"Qwen Code needs your permission to use run_shell_command","notification_type":"permission_prompt","title":"Permission needed"}`
	runHook([]string{"qwen", "Notification"}, payload, "0123456789abcdef")
	if p := <-got; p.Harness != "qwen" || p.Event != "Notification" || string(p.Payload) != payload {
		t.Fatalf("forwarded %+v", p)
	}
}

// Qwen Code shows its permission menu only once the PermissionRequest hook
// returned (a hook sleeping 8 s held the menu back 8 s), so that hook never
// waits on the daemon.
func TestQwenPermissionRequestDoesNotWait(t *testing.T) {
	hold := callTimeout + 500*time.Millisecond
	fakeDaemon(t, hold)
	start := time.Now()
	runHook([]string{"qwen", "PermissionRequest"}, `{"hook_event_name":"PermissionRequest","session_id":"s","tool_name":"run_shell_command","tool_input":{"command":"ls"}}`, "s")
	if d := time.Since(start); d >= hold {
		t.Fatalf("qwen PermissionRequest waited %v", d)
	}
}
