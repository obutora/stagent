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

	"github.com/obutora/stagent/internal/wire"
)

// Integrating OpenCode writes stagent's plugin into OpenCode's global
// plugin directory, running stagent's binary; no setting of the user's is
// touched and no restart is asked for (OpenCode reloads its plugins).
// Removing it deletes the file; doctor reports OpenCode.
func TestOpenCodePlugin(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.bins["opencode"] = "/usr/bin/opencode"
	settings := te.home(".config", "opencode", "opencode.json")
	mine := "{\n  \"model\": \"anthropic/claude\"\n}\n"
	writeFile(t, settings, mine)
	te.reload()
	plugin := te.home(".config", "opencode", "plugins", "ssh-term-stagent.js")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessOpenCode}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "opencode" {
		t.Fatalf("changes = %s", got)
	}
	for _, n := range r.Notes {
		if strings.Contains(n.Code, "opencode") {
			t.Errorf("note %+v", n)
		}
	}
	src := readFile(t, plugin)
	if src != openCodePluginSource(te.l.Bin) || !strings.HasPrefix(src, extensionMarker+"\n") {
		t.Fatalf("plugin:\n%s", src)
	}
	if want := "const BIN = " + jsString(te.l.Bin) + ";"; !strings.Contains(src, want) {
		t.Errorf("plugin lacks %s", want)
	}
	if h := openCodeReport(t, te); !h.Integrated || h.ConfigPath != plugin || !h.Found {
		t.Fatalf("doctor: %+v", h)
	}
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessOpenCode}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}

	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessOpenCode}})
	if exists(plugin) || exists(filepath.Dir(plugin)) {
		t.Fatalf("plugin left behind: %v, plugins dir %v", exists(plugin), exists(filepath.Dir(plugin)))
	}
	if readFile(t, settings) != mine {
		t.Error("opencode.json changed")
	}
	if h := openCodeReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// OPENCODE_CONFIG_DIR replaces OpenCode's configuration directory, else
// XDG_CONFIG_HOME holds it.
func TestOpenCodePluginHonorsConfigDir(t *testing.T) {
	for _, env := range []string{"OPENCODE_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		te := newTestEnv(t, "linux")
		dir := filepath.Join(t.TempDir(), "conf")
		te.vars[env] = dir
		te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessOpenCode}})
		want := filepath.Join(dir, "plugins", "ssh-term-stagent.js")
		if env == "XDG_CONFIG_HOME" {
			want = filepath.Join(dir, "opencode", "plugins", "ssh-term-stagent.js")
		}
		if !exists(want) {
			t.Errorf("%s: no plugin at %s", env, want)
		}
		if exists(te.home(".config")) {
			t.Errorf("%s: ~/.config written", env)
		}
	}
}

// A plugin of the user's at that path is left alone.
func TestOpenCodePluginLeavesUsersFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".config", "opencode", "plugins", "ssh-term-stagent.js")
	mine := "export default { id: \"mine\", setup() {} };\n"
	writeFile(t, path, mine)
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessOpenCode}})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, path) != mine {
		t.Fatalf("changes %+v, %v", r.Changes, err)
	}
}

func openCodeReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessOpenCode {
			return h
		}
	}
	t.Fatal("doctor has no opencode")
	return HarnessReport{}
}

// hookCall is one `stagent hook opencode <event>` the plugin ran.
type hookCall struct {
	Args    []string
	Payload map[string]any
}

// pluginStep is an event fed to the plugin (OpenCode V2's event object)
// and the hooks it is to run.
type pluginStep struct {
	event map[string]any
	hooks []string
}

// stepHooks is the hooks steps are to run, in order.
func stepHooks(steps []pluginStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.hooks...)
	}
	return out
}

// runOpenCodePlugin loads the plugin with node in a server started as
// argv, feeds it the steps' events in order and returns the hooks it ran,
// in order, with a stand-in for stagent's binary that logs each call.
//
// Nothing depends on how fast a hook's process runs: settled, each event
// is sent once the stand-in logged the hooks of the steps before it (each
// Transcript read has started, so a new one is queued); in a burst, the
// first hook is held until the plugin has taken every event, so they all
// arrive while it runs.
func runOpenCodePlugin(t *testing.T, argv []string, steps []pluginStep, burst bool) []hookCall {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("node or sh not available")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.jsonl")
	release := filepath.Join(dir, "release")
	bin := filepath.Join(dir, "stagent")
	fake := "#!/bin/sh\n"
	if burst {
		// mkdir succeeds for the first call only.
		fake += "if mkdir " + shellQuote(filepath.Join(dir, "held")) + " 2>/dev/null; then\n" +
			"  while [ ! -e " + shellQuote(release) + " ]; do sleep 0.01; done\n" +
			"fi\n"
	}
	fake += "p=$(cat)\nprintf '{\"args\":\"%s\",\"payload\":%s}\\n' \"$*\" \"$p\" >> " + shellQuote(log) + "\n"
	if err := os.WriteFile(bin, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plugin.mjs"), []byte(openCodePluginSource(bin)), 0o644); err != nil {
		t.Fatal(err)
	}
	type feed struct {
		Event map[string]any `json:"event"`
		Wait  int            `json:"wait"`
	}
	var feeds []feed
	logged := 0
	for _, s := range steps {
		w := logged
		if burst {
			w = 0
		}
		feeds = append(feeds, feed{s.event, w})
		logged += len(s.hooks)
	}
	feedsJSON, _ := json.Marshal(feeds)
	args, _ := json.Marshal(argv)
	// The driver waits on the log at most 10 s per event, so that a plugin
	// running fewer hooks than expected fails the test instead of hanging
	// it. Once the events end, node lives on until the queued hooks ran
	// (the plugin's timer of the running hook keeps it).
	driver := `import fs from "node:fs";
import plugin from "./plugin.mjs";
process.argv = [process.argv[0], "/$bunfs/root/opencode", ...` + string(args) + `];
const LOG = ` + jsString(log) + `;
const RELEASE = ` + jsString(release) + `;
const feeds = ` + string(feedsJSON) + `;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function logged() {
  try {
    return fs.readFileSync(LOG, "utf8").split("\n").length - 1;
  } catch {
    return 0;
  }
}
async function until(n) {
  for (const end = Date.now() + 10000; logged() < n && Date.now() < end; ) await sleep(5);
}
let cleanup;
const ctx = {
  event: {
    subscribe: () =>
      (async function* () {
        for (const f of feeds) {
          await until(f.wait);
          yield f.event;
        }
        // Asked for the next event: the plugin has handled the last one.
        fs.writeFileSync(RELEASE, "");
        if (cleanup) cleanup();
      })(),
  },
};
cleanup = await plugin.setup(ctx);
`
	if err := os.WriteFile(filepath.Join(dir, "driver.mjs"), []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "driver.mjs")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []hookCall
	for line := range strings.Lines(string(b)) {
		var c struct {
			Args    string         `json:"args"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		calls = append(calls, hookCall{strings.Fields(c.Args), c.Payload})
	}
	return calls
}

// hookNames checks that each call is `stagent hook opencode <event>` with
// a payload naming that event, and returns the events.
func hookNames(t *testing.T, calls []hookCall) []string {
	t.Helper()
	var got []string
	for _, c := range calls {
		if len(c.Args) != 3 || c.Args[0] != "hook" || c.Args[1] != "opencode" || c.Payload["hook_event_name"] != c.Args[2] {
			t.Fatalf("call %+v", c)
		}
		got = append(got, c.Args[2])
	}
	return got
}

func ev(typ string, data map[string]any) map[string]any {
	return map[string]any{"type": typ, "data": data, "location": map[string]any{"directory": "/w/proj"}}
}

// The plugin turns a turn of OpenCode 2.3.0 (the events it sent for a
// shell command asked about and allowed, then a question) into Claude
// Code's hook events, run one after the other in the order of the events,
// and has the session read again (Transcript) as it is written.
func TestOpenCodePluginReportsATurn(t *testing.T) {
	const sid = "ses_e1ad4a0c9ffe59ybAJc2LhlyvP"
	steps := []pluginStep{
		{ev("session.created", map[string]any{"sessionID": sid, "location": map[string]any{"directory": "/w/proj"}}), []string{"SessionStart"}},
		{ev("session.inbox.enqueued", map[string]any{"sessionID": sid, "item": map[string]any{"type": "user", "payload": map[string]any{"text": "run it #approval"}}}), nil},
		{ev("session.execution.started", map[string]any{"sessionID": sid}), []string{"UserPromptSubmit", "Transcript"}},
		{ev("session.tool.input.started", map[string]any{"sessionID": sid, "id": "call_1", "name": "shell"}), nil},
		{ev("session.tool.called", map[string]any{"sessionID": sid, "id": "call_1", "input": map[string]any{"command": "echo hi"}}), nil},
		{ev("permission.asked", map[string]any{"id": "per_1", "sessionID": sid, "action": "shell", "resources": []string{"echo hi"}, "save": []string{"echo *"}, "source": map[string]any{"type": "tool", "id": "call_1"}}), []string{"PermissionRequest"}},
		{ev("permission.replied", map[string]any{"sessionID": sid, "requestID": "per_1", "reply": "once"}), []string{"PermissionReplied"}},
		{ev("session.step.ended", map[string]any{"sessionID": sid}), []string{"Transcript"}},
		{ev("form.created", map[string]any{"form": map[string]any{"id": "frm_1", "sessionID": sid, "title": "Questions", "metadata": map[string]any{"kind": "question"},
			"fields": []any{map[string]any{"key": "q0", "title": "Color", "description": "Which color do you prefer?", "type": "string",
				"options": []any{map[string]any{"value": "Red", "label": "Red"}, map[string]any{"value": "Green", "label": "Green"}}}}}}), []string{"PreToolUse"}},
		{ev("form.replied", map[string]any{"id": "frm_1", "sessionID": sid, "answer": map[string]any{"q0": "Green"}}), []string{"PostToolUse"}},
		{ev("session.step.ended", map[string]any{"sessionID": sid}), []string{"Transcript"}},
		{ev("session.text.ended", map[string]any{"sessionID": sid, "text": "Thanks for answering."}), nil},
		{ev("session.execution.succeeded", map[string]any{"sessionID": sid}), []string{"Stop", "Transcript"}},
		{ev("location.shutdown", map[string]any{}), []string{"SessionEnd"}},
	}
	calls := runOpenCodePlugin(t, []string{"serve", "--stdio", "--port", "0"}, steps, false)
	if got, want := hookNames(t, calls), stepHooks(steps); !slices.Equal(got, want) {
		t.Fatalf("hooks\n  %v\nwant\n  %v", got, want)
	}
	for _, c := range calls {
		if c.Payload["session_id"] != sid || c.Payload["cwd"] != "/w/proj" || c.Payload["exec_path"] == "" {
			t.Errorf("%s: payload %v", c.Args[2], c.Payload)
		}
	}
	payload := func(event string) map[string]any {
		for _, c := range calls {
			if c.Args[2] == event {
				return c.Payload
			}
		}
		return nil
	}
	if p := payload("UserPromptSubmit"); p["prompt"] != "run it #approval" {
		t.Errorf("prompt %v", p)
	}
	if p := payload("PermissionRequest"); p["tool_name"] != "shell" || jsonOf(p["tool_input"]) != `{"command":"echo hi"}` {
		t.Errorf("permission %v", p)
	}
	if p := payload("PermissionReplied"); p["reply"] != "once" {
		t.Errorf("replied %v", p)
	}
	if p := payload("PreToolUse"); p["tool_name"] != "question" ||
		jsonOf(p["tool_input"]) != `{"questions":[{"header":"Color","multiSelect":false,"options":[{"label":"Red"},{"label":"Green"}],"question":"Which color do you prefer?"}]}` {
		t.Errorf("question %v", jsonOf(p["tool_input"]))
	}
	if p := payload("Stop"); p["last_assistant_message"] != "Thanks for answering." {
		t.Errorf("stop %v", p)
	}
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A turn cut short (a permission turned down on the PC, or Ctrl-C) ends
// with StopCancelled: no turn-complete notification for what the user did
// themselves. A failed turn still ends with Stop.
func TestOpenCodePluginInterruptedTurn(t *testing.T) {
	const sid = "ses_1"
	steps := []pluginStep{
		{ev("session.execution.started", map[string]any{"sessionID": sid}), []string{"UserPromptSubmit", "Transcript"}},
		{ev("permission.asked", map[string]any{"id": "per_1", "sessionID": sid, "action": "shell", "resources": []string{"echo hi"}}), []string{"PermissionRequest"}},
		{ev("permission.replied", map[string]any{"sessionID": sid, "requestID": "per_1", "reply": "reject"}), []string{"PermissionReplied"}},
		{ev("session.step.failed", map[string]any{"sessionID": sid}), []string{"Transcript"}},
		{ev("session.execution.interrupted", map[string]any{"sessionID": sid, "reason": "shutdown"}), []string{"StopCancelled", "Transcript"}},
		{ev("session.execution.started", map[string]any{"sessionID": sid}), []string{"UserPromptSubmit", "Transcript"}},
		{ev("session.text.ended", map[string]any{"sessionID": sid, "text": "Something broke."}), nil},
		{ev("session.execution.failed", map[string]any{"sessionID": sid}), []string{"Stop", "Transcript"}},
	}
	calls := runOpenCodePlugin(t, []string{"serve", "--stdio", "--port", "0"}, steps, false)
	if got, want := hookNames(t, calls), stepHooks(steps); !slices.Equal(got, want) {
		t.Fatalf("hooks\n  %v\nwant\n  %v", got, want)
	}
	if p := calls[len(calls)-2].Payload; p["last_assistant_message"] != "Something broke." {
		t.Errorf("stop %v", p)
	}
}

// Reads of a session waiting to run are not queued again (events that come
// at once while the hooks run); a permission asked about a tool call it
// has not seen names the action and what it asks for.
func TestOpenCodePluginCoalescesReads(t *testing.T) {
	const sid = "ses_1"
	steps := []pluginStep{{ev("session.execution.started", map[string]any{"sessionID": sid}), []string{"UserPromptSubmit", "Transcript"}}}
	for range 5 {
		steps = append(steps, pluginStep{ev("session.step.ended", map[string]any{"sessionID": sid}), nil})
	}
	steps = append(steps, pluginStep{ev("permission.asked", map[string]any{"id": "per_1", "sessionID": sid, "action": "edit", "resources": []string{"a.txt"}}), []string{"PermissionRequest"}})
	calls := runOpenCodePlugin(t, []string{"serve", "--stdio", "--port", "0"}, steps, true)
	if got, want := hookNames(t, calls), stepHooks(steps); !slices.Equal(got, want) {
		t.Fatalf("hooks %v, want %v", got, want)
	}
	if p := calls[2].Payload; p["tool_name"] != "edit" || jsonOf(p["tool_input"]) != `{"action":"edit","resources":["a.txt"]}` {
		t.Errorf("permission %v", p)
	}
}

// The plugin stays silent in a server that is not one terminal UI's own:
// the background service, `opencode serve`.
func TestOpenCodePluginSilentInSharedServers(t *testing.T) {
	steps := []pluginStep{{ev("session.execution.started", map[string]any{"sessionID": "ses_1"}), nil}}
	for _, argv := range [][]string{{"serve", "--service"}, {"serve", "--port", "4096"}} {
		if calls := runOpenCodePlugin(t, argv, steps, false); len(calls) != 0 {
			t.Errorf("%q: hooks %+v", argv, calls)
		}
	}
}
