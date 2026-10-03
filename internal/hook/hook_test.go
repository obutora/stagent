package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// fakeDaemon answers hook.event after hold and records what it got.
func fakeDaemon(t *testing.T, hold time.Duration) <-chan wire.HookEventParams {
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
				time.Sleep(hold)
				return struct{}{}, nil
			})
		}
	}()
	return got
}

func runHook(args []string, stdin string, sessionID string) int {
	return run(args, procIO{stdin: strings.NewReader(stdin), sessionID: sessionID})
}

func TestPermissionRequestIsForwarded(t *testing.T) {
	got := fakeDaemon(t, 0)
	payload := `{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"ls"}}`
	if code := runHook([]string{"claude"}, payload, "0123456789abcdef"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	p := <-got
	if p.Harness != "claude" || p.Event != "PermissionRequest" || p.SessionID != "0123456789abcdef" || string(p.Payload) != payload {
		t.Fatalf("forwarded %+v", p)
	}
	runHook([]string{"omp", "Stop"}, `{"session_id":"x","cwd":"/p"}`, "")
	if p := <-got; p.Harness != "omp" || p.Event != "Stop" {
		t.Fatalf("forwarded %+v", p)
	}
}

// claude's PermissionRequest waits for as long as the daemon holds it (the
// prompt is open); every other call gives up after callTimeout.
func TestOnlyClaudePermissionRequestWaitsPastCallTimeout(t *testing.T) {
	hold := callTimeout + 500*time.Millisecond
	fakeDaemon(t, hold)
	start := time.Now()
	runHook([]string{"claude"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`, "s")
	if d := time.Since(start); d < hold {
		t.Fatalf("claude PermissionRequest returned after %v, before the daemon answered", d)
	}
	start = time.Now()
	runHook([]string{"codex"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`, "s")
	if d := time.Since(start); d >= hold {
		t.Fatalf("codex PermissionRequest waited %v", d)
	}
}

func TestCodexNotifyTakesJSONFromArgv(t *testing.T) {
	got := fakeDaemon(t, 0)
	arg := `{"type":"agent-turn-complete","thread-id":"t1","cwd":"/p"}`
	runHook([]string{"codex", "notify", arg}, "", "")
	p := <-got
	if p.Event != "notify" || string(p.Payload) != arg {
		t.Fatalf("forwarded %+v", p)
	}
}

// Outside a stagent session the hook tells how the agent was started; in
// one it does not look.
func TestLaunchFactsOnlyOutsideSessions(t *testing.T) {
	got := fakeDaemon(t, 0)
	tty := true
	asked := 0
	launch := func(h string) launchInfo {
		asked++
		if h != "codex" {
			t.Errorf("launch asked for %q", h)
		}
		return launchInfo{shellWrapper: true, entrypoint: "cli", parentTTY: &tty, parentBatch: true}
	}
	stdin := `{"hook_event_name":"SessionStart","session_id":"c1"}`
	run([]string{"codex"}, procIO{stdin: strings.NewReader(stdin), launch: launch})
	if p := <-got; !p.ShellWrapper || p.Entrypoint != "cli" || p.ParentTTY == nil || !*p.ParentTTY || !p.ParentBatch {
		t.Fatalf("forwarded %+v", p)
	}
	run([]string{"codex"}, procIO{stdin: strings.NewReader(stdin), sessionID: "0123456789abcdef", launch: launch})
	if p := <-got; p.ShellWrapper || p.Entrypoint != "" || p.ParentTTY != nil || p.ParentBatch || asked != 1 {
		t.Fatalf("in a session: forwarded %+v, launch asked %d times", p, asked)
	}
}

// batchArgs follows the shell wrapper's rule for non-interactive runs.
func TestBatchArgs(t *testing.T) {
	for _, c := range []struct {
		harness string
		argv    []string
		want    bool
	}{
		{"claude", []string{"claude", "-p", "hi"}, true},
		{"claude", []string{"node", "/lib/cli.js", "--model", "haiku", "--print", "hi"}, true},
		{"claude", []string{"claude", "fix the -p flag"}, false},
		{"claude", []string{"claude"}, false},
		{"codex", []string{"/vendor/codex", "exec", "hi"}, true},
		{"codex", []string{"/vendor/codex", "e", "hi"}, true},
		{"codex", []string{"/vendor/codex", "--model", "o3", "exec"}, false}, // only the first argument, like the wrapper
		{"codex", []string{"/vendor/codex", "resume", "-p", "x"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "-p", "hi"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode", "json"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode=rpc"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode", "text"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode=text", "--model", "x"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode"}, false},
		{"claude", []string{"claude", "--mode", "json"}, false}, // --mode is omp's
		{"other", []string{"vim", "-p"}, false},
	} {
		if got := batchArgs(c.harness, c.argv); got != c.want {
			t.Errorf("batchArgs(%s, %q) = %v, want %v", c.harness, c.argv, got, c.want)
		}
	}
}

// The installed codex command keeps `sh -c` between the harness and the
// hook: the hook looks past it.
func TestHarnessPIDSkipsTheHookShell(t *testing.T) {
	if os.Getenv("STAGENT_TEST_HARNESS_PID") == "1" {
		fmt.Print(harnessPID())
		os.Exit(0)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no single-process lookup on Windows")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `[ -x "$0" ] && "$0" -test.run='^TestHarnessPIDSkipsTheHookShell$' || true`, self)
	cmd.Env = append(os.Environ(), "STAGENT_TEST_HARNESS_PID=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("harness pid %q, want %d (the test process above sh)", got, os.Getpid())
	}
}

func TestDaemonDownExitsQuietlyAndFast(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	start := time.Now()
	if code := runHook([]string{"claude"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`, ""); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("took %v without a daemon", d)
	}
}

func TestOversizedPayloadIsReduced(t *testing.T) {
	got := fakeDaemon(t, 0)
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
	got := fakeDaemon(t, 0)
	r, w := io.Pipe()
	defer w.Close()
	go w.Write([]byte(`{"hook_event_name":"UserPromptSubmit","prompt":"hi"}` + "\n"))
	start := time.Now()
	run([]string{"claude"}, procIO{stdin: r})
	if d := time.Since(start); d >= stdinWait {
		t.Fatalf("waited %v for stdin to close", d)
	}
	if p := <-got; p.Event != "UserPromptSubmit" {
		t.Fatalf("forwarded %+v", p)
	}
}
