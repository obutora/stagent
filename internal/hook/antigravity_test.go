package hook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/pathstest"
)

// Payloads Antigravity CLI 1.5.14 sent (home shortened): a turn's first
// model call, its second, a tool call's check, the end of the turn.
const (
	agyPreInvocation0 = `{"artifactDirectoryPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4","conversationId":"215bbd83-2b01-4c76-a48b-35a1781e64b4","cwd":"/tmp/agy-lab","hookEventName":"PreInvocation","initialNumSteps":1,"invocationNum":0,"lastUserInput":"Run the shell command ` + "`curl -sI https://example.com`" + ` and tell me the status line.","modelName":"gemini-4-neon-high","transcriptPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4/.system_generated/logs/transcript_full.jsonl","workspacePaths":["/tmp/agy-lab"]}`
	agyPreInvocation1 = `{"conversationId":"215bbd83-2b01-4c76-a48b-35a1781e64b4","cwd":"/tmp/agy-lab","hookEventName":"PreInvocation","initialNumSteps":1,"invocationNum":1,"lastUserInput":"Run the shell command","modelName":"gemini-4-neon-high","transcriptPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4/.system_generated/logs/transcript_full.jsonl","workspacePaths":["/tmp/agy-lab"]}`
	agyPreToolUse     = `{"artifactDirectoryPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4","conversationId":"215bbd83-2b01-4c76-a48b-35a1781e64b4","cwd":"/tmp/agy-lab","hookEventName":"PreToolUse","lastUserInput":"Run the shell command","modelName":"gemini-4-neon-high","stepIdx":2,"toolCall":{"args":{"CommandLine":"curl -sI https://example.com","WaitMsBeforeAsync":10000,"toolSummary":"Fetch HTTP headers from example.com"},"id":"call_7309094732877221702","name":"run_command"},"transcriptPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4/.system_generated/logs/transcript_full.jsonl","workspacePaths":["/tmp/agy-lab"]}`
	agyStop           = `{"continuationActive":false,"conversationId":"215bbd83-2b01-4c76-a48b-35a1781e64b4","cwd":"/tmp/agy-lab","error":"","executionNum":0,"fullyIdle":true,"hookEventName":"Stop","lastUserInput":"Run the shell command","modelName":"gemini-4-neon-high","terminationReason":"NO_TOOL_CALL","transcriptPath":"/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4/.system_generated/logs/transcript_full.jsonl","workspacePaths":["/tmp/agy-lab"]}`
)

func runAgy(event, stdin string) string {
	var out strings.Builder
	run([]string{"antigravity", event}, procIO{stdin: strings.NewReader(stdin), stdout: &out})
	return out.String()
}

// The daemon gets Antigravity CLI's payloads with Claude Code's fields
// added: the conversation as session_id, its transcript, the tool call's
// name and arguments. Antigravity CLI gets {"decision":"ask"} back for a
// tool call — its own rules decide; {} would deny the call.
func TestAntigravityHookForwardsPayload(t *testing.T) {
	got := fakeDaemon(t, 0)
	if out := runAgy("PreToolUse", agyPreToolUse); out != `{"decision":"ask"}`+"\n" {
		t.Fatalf("stdout %q", out)
	}
	p := <-got
	var pl struct {
		Event      string `json:"hook_event_name"`
		Session    string `json:"session_id"`
		Transcript string `json:"transcript_path"`
		Cwd        string `json:"cwd"`
		Tool       string `json:"tool_name"`
		Input      struct {
			CommandLine string
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(p.Payload, &pl); err != nil {
		t.Fatal(err)
	}
	if p.Harness != "antigravity" || p.Event != "PreToolUse" || pl.Event != "PreToolUse" ||
		pl.Session != "215bbd83-2b01-4c76-a48b-35a1781e64b4" ||
		pl.Transcript != "/home/u/.gemini/antigravity-cli/brain/215bbd83-2b01-4c76-a48b-35a1781e64b4/.system_generated/logs/transcript_full.jsonl" ||
		pl.Cwd != "/tmp/agy-lab" || pl.Tool != "run_command" || pl.Input.CommandLine != "curl -sI https://example.com" {
		t.Fatalf("forwarded %s %s", p.Event, p.Payload)
	}
	if !strings.Contains(string(p.Payload), `"toolCall"`) {
		t.Errorf("Antigravity CLI's own fields dropped: %s", p.Payload)
	}
}

// A turn's first model call (invocationNum 0) is the prompt submitted:
// UserPromptSubmit with lastUserInput as its prompt. The later model calls
// and PostInvocation never reach the daemon; each still gets {}.
func TestAntigravityHookPromptIsFirstInvocation(t *testing.T) {
	got := fakeDaemon(t, 0)
	runAgy("PreInvocation", agyPreInvocation0)
	p := <-got
	var pl struct {
		Prompt string `json:"prompt"`
	}
	json.Unmarshal(p.Payload, &pl)
	if p.Event != "UserPromptSubmit" || !strings.HasPrefix(pl.Prompt, "Run the shell command `curl") {
		t.Fatalf("forwarded %s %s", p.Event, p.Payload)
	}
	post := strings.Replace(agyPreInvocation1, `"PreInvocation"`, `"PostInvocation"`, 1)
	for _, c := range [][2]string{{"PreInvocation", agyPreInvocation1}, {"PostInvocation", post}} {
		if out := runAgy(c[0], c[1]); out != "{}\n" {
			t.Errorf("%s: stdout %q", c[0], out)
		}
	}
	runAgy("Stop", agyStop)
	if p := <-got; p.Event != "Stop" {
		t.Fatalf("forwarded %+v, want the Stop after the dropped model calls", p)
	}
	select {
	case p := <-got:
		t.Fatalf("also forwarded %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// A turn the user cancelled ends with StopCancelled, no turn that
// completed.
func TestAntigravityHookCancelledStop(t *testing.T) {
	got := fakeDaemon(t, 0)
	runAgy("Stop", strings.Replace(agyStop, "NO_TOOL_CALL", "USER_CANCELED", 1))
	if p := <-got; p.Event != "StopCancelled" {
		t.Fatalf("forwarded %+v", p)
	}
}

// Antigravity CLI waits for its hooks: with no daemon running the hook
// answers at once, still leaving a tool call to Antigravity CLI's rules.
func TestAntigravityHookAnswersWithoutDaemon(t *testing.T) {
	pathstest.SetHome(t, t.TempDir())
	start := time.Now()
	if out := runAgy("PreToolUse", agyPreToolUse); out != `{"decision":"ask"}`+"\n" {
		t.Fatalf("stdout %q", out)
	}
	if out := runAgy("Stop", "not json"); out != "{}\n" {
		t.Fatalf("stdout for a broken payload %q", out)
	}
	if d := time.Since(start); d > dialTimeout {
		t.Fatalf("took %v", d)
	}
}
