package install

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// A settings.json with formatting our encoder keeps (2-space indent) but a
// statusLine written inline, which only an exact copy brings back.
const statusLineSettings = `{
  "model": "opus",
  "statusLine": {"type":"command", "command": "~/bin/my-status.sh",  "padding": 2, "refreshInterval": 5},
  "permissions": {
    "allow": [
      "Bash(ls)"
    ]
  }
}
`

// statusLineOf returns settings.json's statusLine object.
func statusLineOf(t *testing.T, path string) map[string]any {
	t.Helper()
	var s struct {
		StatusLine map[string]any `json:"statusLine"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &s); err != nil {
		t.Fatal(err)
	}
	return s.StatusLine
}

// `integrate --statusline` puts the relay in place of the user's
// statusLine, keeping its other keys and recording it in the manifest, so
// the relay runs it and unhook puts it back byte for byte (here with the
// hooks in the same file, removed in the same run). Integrating again
// changes nothing.
func TestIntegrateStatusLineKeepsTheOriginal(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	writeFile(t, path, statusLineSettings)

	plan := te.integrate(t, integrateOpts{harness: []string{wire.HarnessClaude}, statusLine: true})
	if got := strings.Join(changeIDs(plan.Changes), ","); got != "claude,statusline" {
		t.Fatalf("plan changes = %s", got)
	}
	sl := plan.Changes[1]
	if sl.Target != path || sl.Action != "modify" || !strings.Contains(sl.Diff, "statusline claude") || !strings.Contains(sl.Summary, "~/bin/my-status.sh") || readFile(t, path) != statusLineSettings {
		t.Fatalf("plan: %+v", sl)
	}

	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessClaude}, statusLine: true})
	got := statusLineOf(t, path)
	want := map[string]any{"type": "command", "command": "[ ! -x " + shellQuote(te.l.Bin) + " ] || exec " + shellQuote(te.l.Bin) + " statusline claude", "padding": 2.0, "refreshInterval": 5.0}
	if len(got) != len(want) {
		t.Fatalf("statusLine = %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("statusLine.%s = %v, want %v", k, got[k], v)
		}
	}
	if !strings.Contains(readFile(t, path), "hook claude") {
		t.Fatal("the hooks applied in the same run were lost")
	}
	if cmd := StatusLineCommand(te.l.Manifest, path); cmd != "~/bin/my-status.sh" {
		t.Fatalf("relayed command = %q", cmd)
	}
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessClaude}, statusLine: true}); len(r.Changes) != 0 {
		t.Fatalf("second integrate changed %v", changeIDs(r.Changes))
	}
	if d := te.doctor().StatusLine; !d.Installed || d.Replaced || !d.UserStatusLine || len(d.Files) != 1 || d.Files[0].State != statusLineRelay {
		t.Fatalf("doctor: %+v", d)
	}

	// Something else edits the file, so the backup no longer applies: the
	// statusLine still comes back exactly.
	cur := readFile(t, path)
	writeFile(t, path, strings.Replace(cur, `"model": "opus"`, `"model": "sonnet"`, 1))
	te.reload()
	te.uninstall("unhook", false, false)
	if got, want := readFile(t, path), strings.Replace(statusLineSettings, `"model": "opus"`, `"model": "sonnet"`, 1); got != want {
		t.Fatalf("after unhook:\n%s\nwant:\n%s", got, want)
	}
}

// Without a statusLine of the user's, unhook takes the relay out again
// (and deletes a settings.json stagent created); --remove statusline does
// the same.
func TestIntegrateStatusLineWithoutOriginal(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	const settings = "{\n  \"model\": \"opus\"\n}\n"
	writeFile(t, path, settings)
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessClaude}, statusLine: true})
	if StatusLineCommand(te.l.Manifest, path) != "" {
		t.Fatal("a relayed command without an original")
	}
	if d := te.doctor().StatusLine; !d.Installed || d.UserStatusLine {
		t.Fatalf("doctor: %+v", d)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{"statusline"}})
	if s := statusLineOf(t, path); s != nil || !strings.Contains(readFile(t, path), "hook claude") {
		t.Fatalf("after --remove statusline:\n%s", readFile(t, path))
	}
	te.reload()
	te.uninstall("unhook", false, false)
	if got := readFile(t, path); got != settings {
		t.Fatalf("after unhook: %q", got)
	}

	fresh := newTestEnv(t, "linux")
	fresh.integrate(t, integrateOpts{apply: true, statusLine: true})
	fresh.reload()
	fresh.uninstall("unhook", false, false)
	if exists(fresh.claudeSettings()) {
		t.Fatal("settings.json stagent created was not deleted")
	}
}

// A statusLine the user put in place of the relay is left alone: by
// integrating other things, and by unhook. doctor says the limit windows
// cannot be measured; the next `integrate --statusline` relays the new
// one, which unhook then brings back.
func TestIntegrateStatusLineReplacedByTheUser(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	writeFile(t, path, statusLineSettings)
	te.integrate(t, integrateOpts{apply: true, statusLine: true})

	// Written the way our encoder writes it: integrating the hooks below
	// rewrites the file in that form before the statusLine is recorded.
	const replaced = `{
  "model": "opus",
  "statusLine": {
    "type": "command",
    "command": "~/bin/new-status.sh"
  }
}
`
	writeFile(t, path, replaced)
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessClaude}}); !strings.Contains(statusLineOf(t, path)["command"].(string), "new-status.sh") || len(r.Changes) != 1 {
		t.Fatalf("integrating hooks touched the statusLine: %v", statusLineOf(t, path))
	}
	te.reload()
	d := te.doctor()
	if s := d.StatusLine; s.Installed || !s.Replaced || !s.UserStatusLine || len(s.Files) != 1 || s.Files[0].State != statusLineReplaced {
		t.Fatalf("doctor: %+v", s)
	}
	if !strings.Contains(strings.Join(d.Problems, "\n"), "statusLine in "+path+" was replaced") {
		t.Fatalf("problems: %v", d.Problems)
	}

	te.integrate(t, integrateOpts{apply: true, statusLine: true})
	if cmd := StatusLineCommand(te.l.Manifest, path); cmd != "~/bin/new-status.sh" {
		t.Fatalf("relayed command after re-integrating = %q", cmd)
	}
	te.reload()
	te.uninstall("unhook", false, false)
	withHooksGone := readFile(t, path)
	if withHooksGone != replaced {
		t.Fatalf("after unhook:\n%s\nwant:\n%s", withHooksGone, replaced)
	}
}

// With CLAUDE_CONFIG_DIR set, its settings.json gets the relay too; one
// integrated under an earlier value is still reached by unhook.
func TestIntegrateStatusLineConfigDir(t *testing.T) {
	te := newTestEnv(t, "linux")
	dir := filepath.Join(t.TempDir(), "claude-work")
	te.vars["CLAUDE_CONFIG_DIR"] = dir
	r := te.integrate(t, integrateOpts{apply: true, statusLine: true})
	if len(r.Changes) != 2 || r.Changes[0].Target != te.claudeSettings() || r.Changes[1].Target != filepath.Join(dir, "settings.json") {
		t.Fatalf("changes: %+v", r.Changes)
	}
	te.vars["CLAUDE_CONFIG_DIR"] = ""
	te.reload()
	te.uninstall("unhook", false, false)
	if exists(filepath.Join(dir, "settings.json")) || exists(te.claudeSettings()) {
		t.Fatal("relay left behind")
	}
}

// On Windows the relay is the binary's path with forward slashes (Git Bash
// and PowerShell run statusLine commands), quoted only when it must be.
func TestStatusLineCommandWindows(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.l.Bin = `C:\Users\u\.ssh-term\agent\bin\stagent.exe`
	if got := te.statusLineCommand(); got != "C:/Users/u/.ssh-term/agent/bin/stagent.exe statusline claude" {
		t.Fatalf("command = %q", got)
	}
	te.l.Bin = `C:\Users\John Doe\.ssh-term\agent\bin\stagent.exe`
	if got := te.statusLineCommand(); got != `"C:/Users/John Doe/.ssh-term/agent/bin/stagent.exe" statusline claude` {
		t.Fatalf("command = %q", got)
	}
}
