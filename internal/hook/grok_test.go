package hook

import (
	"strings"
	"testing"
	"time"
)

// Grok Build's payloads carry Claude Code's snake_case fields beside its
// own camelCase ones (here a permission_prompt notification of Grok Build
// 1.0.77): the hook forwards them as they are, the event named by the
// argument the installed command passes.
func TestGrokHookForwardsPayload(t *testing.T) {
	got := fakeDaemon(t, 0)
	payload := `{"hookEventName":"notification","sessionId":"01a1e529-d6eb-7ca1-a972-905c1d336da5","cwd":"/home/u/proj","transcriptPath":"/home/u/.grok/sessions/%2Fhome%2Fu%2Fproj/01a1e529-d6eb-7ca1-a972-905c1d336da5/updates.jsonl","notificationType":"permission_prompt","message":"Tool permission requested","hook_event_name":"Notification","session_id":"01a1e529-d6eb-7ca1-a972-905c1d336da5","transcript_path":"/home/u/.grok/sessions/%2Fhome%2Fu%2Fproj/01a1e529-d6eb-7ca1-a972-905c1d336da5/updates.jsonl","notification_type":"permission_prompt"}`
	runHook([]string{"grok", "Notification"}, payload, "0123456789abcdef")
	if p := <-got; p.Harness != "grok" || p.Event != "Notification" || string(p.Payload) != payload {
		t.Fatalf("forwarded %+v", p)
	}
}

// Grok Build sends Stop when a turn ends (reason end_turn) and again, with
// reason shutdown, when it exits, beside SessionEnd: only the first is a
// turn that ended. A Stop without a reason counts as one.
func TestGrokHookDropsTheStopOfItsExit(t *testing.T) {
	got := fakeDaemon(t, 0)
	runHook([]string{"grok", "Stop"}, `{"hook_event_name":"Stop","session_id":"s","reason":"shutdown","stopHookActive":false}`, "0123456789abcdef")
	runHook([]string{"grok", "Stop"}, `{"hook_event_name":"Stop","session_id":"s","reason":"end_turn","lastAssistantMessage":"Done.","last_assistant_message":"Done."}`, "0123456789abcdef")
	if p := <-got; p.Event != "Stop" || !strings.Contains(string(p.Payload), "end_turn") {
		t.Fatalf("forwarded %+v", p)
	}
	runHook([]string{"grok", "Stop"}, `{"hook_event_name":"Stop","session_id":"s"}`, "0123456789abcdef")
	if p := <-got; p.Event != "Stop" {
		t.Fatalf("forwarded %+v", p)
	}
	select {
	case p := <-got:
		t.Fatalf("also forwarded %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// Grok Build also runs the hooks of Claude Code's settings
// (~/.claude/settings.json), stagent's claude hook among them, with
// GROK_HOOK_EVENT set: that call is no claude's, and Grok Build's own hooks
// report the same event.
func TestClaudeHookRunByGrokIsDropped(t *testing.T) {
	got := fakeDaemon(t, 0)
	stdin := `{"hook_event_name":"UserPromptSubmit","session_id":"01a1e532","prompt":"go"}`
	run([]string{"claude", "UserPromptSubmit"}, procIO{stdin: strings.NewReader(stdin), ranBy: "grok"})
	run([]string{"grok", "UserPromptSubmit"}, procIO{stdin: strings.NewReader(stdin), ranBy: "grok"})
	if p := <-got; p.Harness != "grok" {
		t.Fatalf("forwarded %+v", p)
	}
	select {
	case p := <-got:
		t.Fatalf("also forwarded %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
	if r := hookRunner(func(k string) string {
		if k == "GROK_HOOK_EVENT" {
			return "user_prompt_submit"
		}
		return ""
	}); r != "grok" {
		t.Errorf("runner %q, want grok", r)
	}
	if r := hookRunner(func(string) string { return "" }); r != "" {
		t.Errorf("runner %q without Grok Build's variables", r)
	}
}

// Grok Build shows its permission menu while the permission_prompt
// notification's hook runs and stops a hook after 5 s: the hook returns at
// once.
func TestGrokNotificationDoesNotWait(t *testing.T) {
	hold := callTimeout + 500*time.Millisecond
	fakeDaemon(t, hold)
	start := time.Now()
	runHook([]string{"grok", "Notification"}, `{"hook_event_name":"Notification","session_id":"s","notification_type":"permission_prompt"}`, "s")
	if d := time.Since(start); d >= hold {
		t.Fatalf("grok Notification waited %v", d)
	}
}
