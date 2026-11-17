package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
)

// fakeOpenCodeParent makes the process that started the hook's OpenCode
// server argv, with a terminal or not, for the test.
func fakeOpenCodeParent(t *testing.T, argv []string, tty bool) {
	t.Helper()
	prev := openCodeParent
	openCodeParent = func() (openCodeProc, bool) {
		return openCodeProc{argv: argv, tty: tty, pid: 4911, start: time.UnixMilli(1794846187000)}, true
	}
	t.Cleanup(func() { openCodeParent = prev })
}

// stagent's plugin sends Claude Code's payloads; the hook names the
// session's transcript, stagent's record of it, for the daemon.
func TestOpenCodeHookNamesTheTranscript(t *testing.T) {
	got := fakeDaemon(t, 0)
	fakeOpenCodeParent(t, []string{"/u/bin/opencode", "--standalone"}, true)
	payload := `{"hook_event_name":"UserPromptSubmit","session_id":"ses_e1ad4a0c9ffe59ybAJc2LhlyvP","cwd":"/w","exec_path":"/u/bin/opencode.exe","prompt":"run it"}`
	runHook([]string{"opencode", "UserPromptSubmit"}, payload, "0123456789abcdef")
	p := <-got
	var pl map[string]any
	json.Unmarshal(p.Payload, &pl)
	want := filepath.Join(os.Getenv(paths.EnvHome), ".ssh-term", "agent", "state", "opencode", "ses_e1ad4a0c9ffe59ybAJc2LhlyvP.jsonl")
	if p.Harness != "opencode" || p.Event != "UserPromptSubmit" || pl["transcript_path"] != want || pl["prompt"] != "run it" {
		t.Fatalf("forwarded %+v (%v), want transcript %s", p, pl, want)
	}
	// A payload naming no session (or a session id that is no file name)
	// names no transcript.
	runHook([]string{"opencode", "SessionEnd"}, `{"hook_event_name":"SessionEnd","session_id":"../x"}`, "0123456789abcdef")
	if p := <-got; strings.Contains(string(p.Payload), "transcript_path") {
		t.Fatalf("forwarded %s", p.Payload)
	}
}

// The plugin's StopCancelled (a turn cut short) reaches the daemon as it
// is, as Grok Build's does.
func TestOpenCodeHookForwardsStopCancelled(t *testing.T) {
	got := fakeDaemon(t, 0)
	fakeOpenCodeParent(t, []string{"/u/bin/opencode", "--standalone"}, true)
	runHook([]string{"opencode", "StopCancelled"}, `{"hook_event_name":"StopCancelled","session_id":"ses_1"}`, "0123456789abcdef")
	if p := <-got; p.Harness != "opencode" || p.Event != "StopCancelled" {
		t.Fatalf("forwarded %+v", p)
	}
}

// OpenCode runs plugins in its server, the terminal UI's child: how
// OpenCode was started is read from the server's parent. A hook outside a
// stagent session tells the daemon the terminal UI's terminal; a plugin
// in the server of `opencode run` reports nothing at all.
func TestOpenCodeHookReadsTheTerminalUI(t *testing.T) {
	li := launchInfo{}
	fakeOpenCodeParent(t, []string{"/u/bin/opencode", "--standalone"}, true)
	openCodeHook.launch(&li, nil)
	if li.parentTTY == nil || !*li.parentTTY || li.parentBatch {
		t.Fatalf("terminal UI: %+v", li)
	}

	got := fakeDaemon(t, 0)
	fakeOpenCodeParent(t, []string{"/u/bin/opencode", "run", "--standalone", "fix it"}, false)
	runHook([]string{"opencode", "Stop"}, `{"hook_event_name":"Stop","session_id":"ses_1"}`, "0123456789abcdef")
	select {
	case p := <-got:
		t.Fatalf("batch run forwarded %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// The pseudo-event Transcript has the hook read the session with the
// OpenCode binary that runs it (`session export --standalone`) and bring
// stagent's record up to it, with the OpenCode process that reports it
// beside; the daemon hears nothing of it.
func TestOpenCodeHookReadsTheSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh stand-in")
	}
	got := fakeDaemon(t, 0)
	fakeOpenCodeParent(t, []string{"/u/bin/opencode", "--standalone"}, true)
	dir := t.TempDir()
	args := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "opencode")
	export := `{"info":{"id":"ses_1","title":"Mock title","time":{"created":1},"location":{"directory":"/w"}},"messages":[{"id":"msg_a","time":{"created":2},"text":"run it","type":"user"}]}`
	script := "#!/bin/sh\necho \"$@\" > '" + args + "'\nprintf '%s\\n' '" + export + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runHook([]string{"opencode", "Transcript"}, `{"hook_event_name":"Transcript","session_id":"ses_1","exec_path":"`+bin+`"}`, "0123456789abcdef")
	if b, _ := os.ReadFile(args); string(b) != "session export --standalone ses_1\n" {
		t.Fatalf("ran %q", b)
	}
	root := filepath.Join(os.Getenv(paths.EnvHome), ".ssh-term", "agent", "state", "opencode")
	b, err := os.ReadFile(filepath.Join(root, "ses_1.jsonl"))
	want := `{"type":"session","id":"ses_1","title":"Mock title","directory":"/w","created":1}` + "\n" +
		`{"id":"msg_a","time":{"created":2},"text":"run it","type":"user"}` + "\n"
	if err != nil || string(b) != want {
		t.Fatalf("record %q (%v), want %q", b, err, want)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ses_1.proc")); string(b) != `{"pid":4911,"started":1794846187000}` {
		t.Fatalf("process sidecar %q", b)
	}
	select {
	case p := <-got:
		t.Fatalf("forwarded %+v", p)
	case <-time.After(300 * time.Millisecond):
	}
}

// Only an OpenCode binary is run: a payload naming another program has
// the hook run opencode from PATH.
func TestOpenCodeExportBinary(t *testing.T) {
	abs := func(p string) string {
		if runtime.GOOS == "windows" {
			return `C:` + filepath.FromSlash(p)
		}
		return p
	}
	for in, want := range map[string]string{
		abs("/u/lib/node_modules/@opencode/cli/bin/opencode.exe"): abs("/u/lib/node_modules/@opencode/cli/bin/opencode.exe"),
		abs("/home/u/.opencode/bin/opencode"):                     abs("/home/u/.opencode/bin/opencode"),
		abs("/bin/rm"):                                            "opencode",
		"":                                                        "opencode",
		"opencode":                                                "opencode",
	} {
		if got := openCodeBinary(in); got != want {
			t.Errorf("openCodeBinary(%q) = %q, want %q", in, got, want)
		}
	}
}
