package install

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// omp asks the user through its ask tool. The extension reports omp's
// tool_call event of ask as PreToolUse and its tool_result (answered,
// cancelled or failed) as PostToolUse, with the tool's name and input
// (omp 18.12.6: {type, toolName, toolCallId, input}); other tools are not
// reported. Run with node (one that strips TypeScript types) when it is
// installed: the extension calls a stand-in stagent that records what it
// gets.
func TestOmpExtensionReportsAsk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in stagent is a shell script")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	home := t.TempDir()
	out := filepath.Join(home, "calls")
	bin := filepath.Join(home, ".ssh-term", "agent", "bin", "stagent")
	writeFile(t, bin, "#!/bin/sh\nf=\""+out+"/$$\"\n{ echo \"$*\"; cat; } > \"$f.tmp\" && mv \"$f.tmp\" \"$f\"\n")
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	ext := filepath.Join(home, "ext.ts")
	writeFile(t, ext, ompExtensionSource)
	runner := `
const mod = await import(process.argv[1]);
const handlers = {};
mod.default({ on: (ev, fn) => (handlers[ev] ??= []).push(fn) });
const ctx = { cwd: "/home/u/proj", hasUI: true, sessionManager: { getSessionId: () => "s-1", getSessionFile: () => "/t/s-1.jsonl", getHeader: () => ({}) } };
const input = { questions: [{ id: "color", question: "Which color do you like?", header: "Color", options: [{ label: "Red" }, { label: "Blue", description: "cool" }], recommended: 0 }] };
const emit = async (ev, e) => { for (const fn of handlers[ev] ?? []) await fn(e, ctx); };
await emit("tool_call", { type: "tool_call", toolName: "read", toolCallId: "c0", input: { path: "a.txt" } });
await emit("tool_result", { type: "tool_result", toolName: "read", toolCallId: "c0", input: { path: "a.txt" }, content: [], isError: false });
await emit("tool_call", { type: "tool_call", toolName: "ask", toolCallId: "c1", input });
await emit("tool_result", { type: "tool_result", toolName: "ask", toolCallId: "c1", input, content: [{ type: "text", text: "User selected: Red" }], isError: false });
`
	cmd := exec.Command(node, "--input-type=module", "-e", runner, ext)
	cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	if b, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(b), "ERR_UNKNOWN_FILE_EXTENSION") || strings.Contains(string(b), "ERR_UNSUPPORTED_NODE_MODULES_TYPE_STRIPPING") {
			t.Skip("this node does not strip TypeScript types")
		}
		t.Fatalf("node: %v\n%s", err, b)
	}

	// The hooks run detached: wait for both, then for any stray one.
	var calls []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		calls = calls[:0]
		entries, _ := os.ReadDir(out)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".tmp") {
				calls = append(calls, readFile(t, filepath.Join(out, e.Name())))
			}
		}
		if len(calls) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if entries, _ := os.ReadDir(out); len(entries) != 2 {
		t.Fatalf("%d hook calls, want 2: %q", len(entries), calls)
	}

	type payload struct {
		Event     string          `json:"hook_event_name"`
		SessionID string          `json:"session_id"`
		Cwd       string          `json:"cwd"`
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	var events []string
	for _, c := range calls {
		args, body, _ := strings.Cut(c, "\n")
		if args != "hook omp" {
			t.Errorf("stagent %s", args)
		}
		var p payload
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("payload %q: %v", body, err)
		}
		if p.ToolName != "ask" || p.SessionID != "s-1" || p.Cwd != "/home/u/proj" ||
			!strings.Contains(string(p.ToolInput), `"question":"Which color do you like?"`) {
			t.Errorf("payload %s", body)
		}
		events = append(events, p.Event)
	}
	slices.Sort(events)
	if !slices.Equal(events, []string{"PostToolUse", "PreToolUse"}) {
		t.Fatalf("events %v", events)
	}
}
