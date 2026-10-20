package schedagent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// judge feeds out to the reader of harness h line by line, as the wrapper
// does, and returns the outcome and what the terminal would show.
func judge(h, out string, exitCode int) (Outcome, string) {
	var screen bytes.Buffer
	r := newReader(h, &screen)
	for line := range strings.Lines(out) {
		r.line([]byte(line))
	}
	return r.outcome(exitCode), screen.String()
}

// Shapes as claude 2.1.296 -p --output-format json printed them on this
// host (trimmed).
const (
	claudeDenied = `{"type":"result","subtype":"success","is_error":false,"result":"hello.txt の作成は断られました。","num_turns":2,"session_id":"b0d56a6d-34a3-46a5-86c8-64c503c98cff","permission_denials":[{"tool_name":"Write","tool_use_id":"toolu_01","tool_input":{"file_path":"/tmp/sx/claude/hello.txt","content":"hi"}},{"tool_name":"Bash","tool_use_id":"toolu_02","tool_input":{"command":"python3 x.py"}}],"terminal_reason":"completed"}` + "\n"
	claudeDone   = `{"type":"result","subtype":"success","is_error":false,"result":"DONE","session_id":"3843baa7-89dc-4364-80c7-f12f2f881cad","permission_denials":[]}` + "\n"
	claudeError  = "[claude-code:unrecognized_model] {\"model\":\"x\"}\n" +
		`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"model x not found","session_id":"02de5a8a-544d-478d-a2b9-7359097391f1","permission_denials":[],"terminal_reason":"api_error"}` + "\n"
)

func TestClaudeOutcome(t *testing.T) {
	o, screen := judge(wire.HarnessClaude, claudeDenied, 0)
	if o.Result != wire.RunDenied || o.Denials != 2 || o.ConversationID != "b0d56a6d-34a3-46a5-86c8-64c503c98cff" ||
		o.Summary != "hello.txt の作成は断られました。" || o.Unjudged {
		t.Errorf("denied: %+v", o)
	}
	for _, want := range []string{"hello.txt の作成は断られました。", "Write", "/tmp/sx/claude/hello.txt", "python3 x.py"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, `"permission_denials"`) {
		t.Errorf("screen shows the raw JSON:\n%s", screen)
	}

	if o, _ := judge(wire.HarnessClaude, claudeDone, 0); o.Result != wire.RunSucceeded || o.Denials != 0 || o.Summary != "DONE" {
		t.Errorf("done: %+v", o)
	}
	if o, _ := judge(wire.HarnessClaude, claudeError, 1); o.Result != wire.RunFailed || o.Unjudged ||
		o.ConversationID != "02de5a8a-544d-478d-a2b9-7359097391f1" || !strings.Contains(o.Error, "model x not found") {
		t.Errorf("error: %+v", o)
	}
	// is_error fails a run that exited 0.
	isErr := strings.Replace(claudeDone, `"is_error":false`, `"is_error":true`, 1)
	if o, _ := judge(wire.HarnessClaude, isErr, 0); o.Result != wire.RunFailed {
		t.Errorf("is_error: %+v", o)
	}
}

// claude that cannot log in (on macOS: run outside the GUI login session,
// where the keychain is locked, with no ~/.claude/.credentials.json) ends
// with is_error and asks for /login. The first shape is as claude 2.1.296
// printed it on this host with an empty HOME; the second, a refused login
// (HTTP 401), follows the same schema.
func TestClaudeLoggedOut(t *testing.T) {
	const notLoggedIn = `{"type":"result","subtype":"success","is_error":true,"api_error_status":null,"result":"Not logged in · Please run /login","session_id":"448e2b46-c686-4fec-b4fd-e411844ca298","permission_denials":[],"terminal_reason":"api_error"}` + "\n"
	o, _ := judge(wire.HarnessClaude, notLoggedIn, 1)
	if o.Result != wire.RunFailed || !o.AuthFailed || o.Error != "Not logged in · Please run /login" {
		t.Errorf("not logged in: %+v", o)
	}
	const refused = `{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 authentication_error","session_id":"5d1c","permission_denials":[],"terminal_reason":"api_error"}` + "\n"
	if o, _ := judge(wire.HarnessClaude, refused, 1); o.Result != wire.RunFailed || !o.AuthFailed {
		t.Errorf("401: %+v", o)
	}
	if o, _ := judge(wire.HarnessClaude, claudeError, 1); o.AuthFailed {
		t.Errorf("another error: %+v", o)
	}
}

// Shapes of codex exec --json (0.156): the failing run is as this host
// printed it; the others follow its event schema.
const (
	codexFailed = `{"type":"thread.started","thread_id":"01a157de-f894-7913-bc88-7f29467ea011"}
{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 2/5 (workspace routing discovery unauthorized (401))"}
{"type":"item.completed","item":{"id":"item_0","type":"error","message":"Falling back from WebSockets to HTTPS transport."}}
{"type":"error","message":"workspace routing discovery unauthorized (401)"}
{"type":"turn.failed","error":{"message":"workspace routing discovery unauthorized (401)"}}
`
	codexDenied = `{"type":"thread.started","thread_id":"t-1"}
{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 1/5 (stream disconnected)"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'rm -rf build'","aggregated_output":"","exit_code":null,"status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'rm -rf build'","aggregated_output":"","exit_code":null,"status":"declined"}}
{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/etc/x","kind":"add"}],"status":"failed"}}
{"type":"item.completed","item":{"id":"item_3","type":"command_execution","command":"bash -lc 'go test ./...'","aggregated_output":"FAIL\n","exit_code":1,"status":"failed"}}
{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"item_5","type":"agent_message","text":"build を消せませんでした。"}}
{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}
`
)

func TestCodexOutcome(t *testing.T) {
	o, screen := judge(wire.HarnessCodex, codexFailed, 1)
	if o.Result != wire.RunFailed || o.ConversationID != "01a157de-f894-7913-bc88-7f29467ea011" || o.Unjudged ||
		!strings.Contains(o.Error, "unauthorized (401)") {
		t.Errorf("failed: %+v", o)
	}
	if !strings.Contains(screen, "unauthorized (401)") {
		t.Errorf("screen:\n%s", screen)
	}
	// turn.failed fails it whatever the exit code.
	if o, _ := judge(wire.HarnessCodex, codexFailed, 0); o.Result != wire.RunFailed {
		t.Errorf("turn.failed with exit 0: %+v", o)
	}

	// A declined command and a file change that failed are refused
	// operations; a command that ran and failed (a failing test) is not.
	// An error event the turn recovered from does not fail the run.
	o, screen = judge(wire.HarnessCodex, codexDenied, 0)
	if o.Result != wire.RunDenied || o.Denials != 2 || o.ConversationID != "t-1" || o.Summary != "build を消せませんでした。" {
		t.Errorf("denied: %+v", o)
	}
	for _, want := range []string{"rm -rf build", "go test ./...", "build を消せませんでした。"} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
	ok := strings.NewReplacer(`"status":"declined"`, `"status":"completed"`, `"status":"failed"}}`, `"status":"completed"}}`).Replace(codexDenied)
	if o, _ := judge(wire.HarnessCodex, ok, 0); o.Result != wire.RunSucceeded || o.Denials != 0 {
		t.Errorf("succeeded: %+v", o)
	}
	// An error event with no completed turn fails it.
	noTurn := `{"type":"thread.started","thread_id":"t-2"}` + "\n" + `{"type":"error","message":"stream closed"}` + "\n"
	if o, _ := judge(wire.HarnessCodex, noTurn, 0); o.Result != wire.RunFailed || o.Error != "stream closed" {
		t.Errorf("error without a turn: %+v", o)
	}
}

// Shapes of omp -p --mode json (18.8.9) as this host printed them
// (trimmed).
const ompDenied = `{"type":"session","version":3,"id":"01a157df-ba70-75f4-9a0c-290d4152a207","timestamp":"2026-10-20T05:54:02.480Z","cwd":"/tmp/sx/omp"}
{"type":"agent_start"}
{"type":"tool_execution_start","toolCallId":"toolu_1","toolName":"write","args":{"path":"hello.txt","content":"hi"},"intent":"Creating hello.txt file"}
{"type":"tool_execution_end","toolCallId":"toolu_1","toolName":"write","result":{"content":[{"type":"text","text":"Tool \"write\" requires approval but no interactive UI available.\nOptions:\n  1. Set tools.approvalMode: yolo in /settings"}],"details":{}},"isError":true}
{"type":"tool_execution_end","toolCallId":"toolu_2","toolName":"read","result":{"content":[{"type":"text","text":"ENOENT: no such file"}]},"isError":true}
{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"Tool \"write\" requires approval"}]}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"..."},{"type":"text","text":"I couldn't create hello.txt."}],"stopReason":"stop"}}
{"type":"agent_end","messages":[]}
`

func TestOmpOutcome(t *testing.T) {
	o, screen := judge(wire.HarnessOmp, ompDenied, 0)
	if o.Result != wire.RunDenied || o.Denials != 1 || o.ConversationID != "01a157df-ba70-75f4-9a0c-290d4152a207" ||
		o.Summary != "I couldn't create hello.txt." {
		t.Errorf("denied: %+v", o)
	}
	for _, want := range []string{"write", "hello.txt", "I couldn't create hello.txt."} {
		if !strings.Contains(screen, want) {
			t.Errorf("screen lacks %q:\n%s", want, screen)
		}
	}
	errored := strings.Replace(ompDenied, `"stopReason":"stop"}`, `"stopReason":"error","errorMessage":"401 Unauthorized"}`, 1)
	if o, _ := judge(wire.HarnessOmp, errored, 0); o.Result != wire.RunFailed || o.Error != "401 Unauthorized" {
		t.Errorf("errored: %+v", o)
	}
}

// Output that is not the harness's JSON is judged by the exit code alone
// and says so; its lines still reach the terminal.
func TestUnreadableOutput(t *testing.T) {
	for _, h := range []string{wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp} {
		out := "Model \"x\" not found\nusage: …\n"
		o, screen := judge(h, out, 0)
		if o.Result != wire.RunSucceeded || !o.Unjudged {
			t.Errorf("%s exit 0: %+v", h, o)
		}
		if !strings.Contains(screen, `Model "x" not found`) {
			t.Errorf("%s screen:\n%s", h, screen)
		}
		if o, _ := judge(h, out, 3); o.Result != wire.RunFailed || !o.Unjudged {
			t.Errorf("%s exit 3: %+v", h, o)
		}
		if o, _ := judge(h, "", 0); o.Result != wire.RunSucceeded || !o.Unjudged {
			t.Errorf("%s no output: %+v", h, o)
		}
	}
}

// The summary is the last reply, at most ScheduleSummaryMax bytes of whole
// runes.
func TestSummaryIsBounded(t *testing.T) {
	const most = wire.ScheduleSummaryMax
	long := strings.Repeat("あ", most) // 3 bytes each
	out := `{"type":"result","subtype":"success","is_error":false,"result":"` + long + `","session_id":"s"}` + "\n"
	o, _ := judge(wire.HarnessClaude, out, 0)
	if len(o.Summary) > most || len(o.Summary) < most-3 || !strings.HasPrefix(long, o.Summary) {
		t.Errorf("summary of %d bytes", len(o.Summary))
	}
}
