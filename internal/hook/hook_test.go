package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// fakeDaemon answers hook.event with result and records what it got.
func fakeDaemon(t *testing.T, result wire.HookEventResult) <-chan wire.HookEventParams {
	t.Helper()
	t.Setenv(paths.EnvHome, t.TempDir())
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := ipc.Listen(l.DaemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan wire.HookEventParams, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go rpc.Serve(context.Background(), c, func(_ context.Context, _ *rpc.Conn, m *wire.Msg) (any, error) {
				var p wire.HookEventParams
				if err := rpc.Decode(m, &p); err != nil {
					return nil, err
				}
				got <- p
				return result, nil
			})
		}
	}()
	return got
}

func runHook(args []string, stdin string, sessionID string) (string, int) {
	var out bytes.Buffer
	code := run(args, procIO{stdin: strings.NewReader(stdin), stdout: &out, sessionID: sessionID})
	return out.String(), code
}

func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("output %q is not JSON: %v", s, err)
	}
	return v
}

func TestClaudePermissionAllow(t *testing.T) {
	got := fakeDaemon(t, wire.HookEventResult{Decision: wire.DecisionAllow})
	payload := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`
	out, code := runHook([]string{"claude"}, payload, "0123456789abcdef")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := decodeJSON(t, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"}}}`)
	if !reflect.DeepEqual(decodeJSON(t, out), want) {
		t.Fatalf("stdout %q", out)
	}
	p := <-got
	if p.Harness != "claude" || p.Event != "PermissionRequest" || p.SessionID != "0123456789abcdef" || string(p.Payload) != payload {
		t.Fatalf("forwarded %+v", p)
	}
}

func TestCodexPermissionDenyCarriesMessage(t *testing.T) {
	fakeDaemon(t, wire.HookEventResult{Decision: wire.DecisionDeny, Message: "not now"})
	out, _ := runHook([]string{"codex"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`, "")
	want := decodeJSON(t, `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"deny","message":"not now"}}}`)
	if !reflect.DeepEqual(decodeJSON(t, out), want) {
		t.Fatalf("stdout %q", out)
	}
}

func TestNoDecisionAndOtherEventsPrintNothing(t *testing.T) {
	got := fakeDaemon(t, wire.HookEventResult{Decision: wire.DecisionNone})
	if out, code := runHook([]string{"claude"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`, ""); out != "" || code != 0 {
		t.Fatalf("no decision: %q %d", out, code)
	}
	<-got
	// A non-blocking event prints nothing even if the daemon said something.
	if out, _ := runHook([]string{"omp", "Stop"}, `{"session_id":"x","cwd":"/p"}`, ""); out != "" {
		t.Fatalf("Stop printed %q", out)
	}
	if p := <-got; p.Harness != "omp" || p.Event != "Stop" {
		t.Fatalf("forwarded %+v", p)
	}
}

func TestCodexNotifyTakesJSONFromArgv(t *testing.T) {
	got := fakeDaemon(t, wire.HookEventResult{})
	arg := `{"type":"agent-turn-complete","thread-id":"t1","cwd":"/p"}`
	if out, _ := runHook([]string{"codex", "notify", arg}, "", ""); out != "" {
		t.Fatalf("notify printed %q", out)
	}
	p := <-got
	if p.Event != "notify" || string(p.Payload) != arg {
		t.Fatalf("forwarded %+v", p)
	}
}

func TestDaemonDownExitsQuietlyAndFast(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	start := time.Now()
	out, code := runHook([]string{"claude"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`, "")
	if out != "" || code != 0 {
		t.Fatalf("got %q %d", out, code)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %v without a daemon", d)
	}
}

func TestOversizedPayloadIsReduced(t *testing.T) {
	got := fakeDaemon(t, wire.HookEventResult{})
	big := `{"session_id":"s1","hook_event_name":"PermissionRequest","cwd":"/p","tool_name":"Write","tool_input":{"file_path":"/p/big.txt","content":"` +
		strings.Repeat("x", maxPayload+100) + `"}}`
	runHook([]string{"claude"}, big, "")
	p := <-got
	if p.Event != "PermissionRequest" {
		t.Fatalf("event %q", p.Event)
	}
	var reduced struct {
		SessionID string            `json:"session_id"`
		ToolName  string            `json:"tool_name"`
		ToolInput map[string]string `json:"tool_input"`
	}
	if err := json.Unmarshal(p.Payload, &reduced); err != nil || len(p.Payload) > 4096 {
		t.Fatalf("payload %d bytes: %v", len(p.Payload), err)
	}
	if reduced.SessionID != "s1" || reduced.ToolName != "Write" || reduced.ToolInput["file_path"] != "/p/big.txt" {
		t.Fatalf("reduced payload %s", p.Payload)
	}
}

func TestPayloadOnUnclosedStdin(t *testing.T) {
	got := fakeDaemon(t, wire.HookEventResult{})
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"hi"}` + "\n"))
	start := time.Now()
	run([]string{"claude"}, procIO{stdin: r, stdout: io.Discard})
	if d := time.Since(start); d >= stdinWait {
		t.Fatalf("waited %v for stdin to close", d)
	}
	if p := <-got; p.Event != "UserPromptSubmit" {
		t.Fatalf("forwarded %+v", p)
	}
}
