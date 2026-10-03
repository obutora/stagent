package install

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const claudeSettingsBefore = `{
  "model": "opus",
  "price": 1.50,
  "greeting": "caf\u00e9 <b>",
  "hooks": {
    "Stop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "command": "notify-send done"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "audit.sh"
          }
        ]
      }
    ]
  },
  "permissions": {
    "allow": [
      "Bash(ls)"
    ]
  }
}
`

type claudeHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type claudeSettings struct {
	Hooks map[string][]struct {
		Matcher *string      `json:"matcher"`
		Hooks   []claudeHook `json:"hooks"`
	} `json:"hooks"`
}

func parseClaude(t *testing.T, s string) claudeSettings {
	t.Helper()
	var c claudeSettings
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, s)
	}
	return c
}

func TestClaudeMergeKeepsForeignEntriesOrderAndFormatting(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	writeFile(t, path, claudeSettingsBefore)

	r := te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}})
	if got := changeIDs(r.Changes); len(got) != 1 || got[0] != hClaude || r.Changes[0].Action != "modify" {
		t.Fatalf("changes = %v", got)
	}
	out := readFile(t, path)

	// Untouched scalars keep their exact source text; top-level key order
	// is kept.
	for _, want := range []string{`"price": 1.50`, `"greeting": "caf\u00e9 <b>"`} {
		if !strings.Contains(out, want) {
			t.Errorf("lost %s:\n%s", want, out)
		}
	}
	if !(strings.Index(out, `"model"`) < strings.Index(out, `"hooks"`) && strings.Index(out, `"hooks"`) < strings.Index(out, `"permissions"`)) {
		t.Errorf("top-level key order changed:\n%s", out)
	}
	// Existing events stay first, new ones follow in registration order.
	order := []string{`"Stop"`, `"PreToolUse"`, `"SessionStart"`, `"SessionEnd"`, `"UserPromptSubmit"`, `"Notification"`, `"PermissionRequest"`}
	for i := 1; i < len(order); i++ {
		if strings.Index(out, order[i-1]) > strings.Index(out, order[i]) {
			t.Errorf("event %s appears before %s:\n%s", order[i], order[i-1], out)
		}
	}

	s := parseClaude(t, out)
	if n := len(s.Hooks["PreToolUse"]); n != 1 || s.Hooks["PreToolUse"][0].Hooks[0].Command != "audit.sh" {
		t.Errorf("PreToolUse changed: %+v", s.Hooks["PreToolUse"])
	}
	stop := s.Hooks["Stop"]
	if len(stop) != 2 || stop[0].Hooks[0].Command != "notify-send done" {
		t.Fatalf("Stop groups = %+v", stop)
	}
	wantCmd := te.hookCommand(hClaude)
	for _, ev := range claudeEvents {
		groups := s.Hooks[ev]
		last := groups[len(groups)-1].Hooks
		if len(last) != 1 || last[0].Command != wantCmd || last[0].Type != "command" {
			t.Errorf("%s: our entry missing: %+v", ev, groups)
			continue
		}
		wantTimeout := 10
		if ev == "PermissionRequest" {
			wantTimeout = 7 * 24 * 60 * 60 // held until the prompt is answered
		}
		if last[0].Timeout != wantTimeout {
			t.Errorf("%s timeout = %d, want %d", ev, last[0].Timeout, wantTimeout)
		}
	}
	if !strings.Contains(wantCmd, "[ -x '") || !strings.HasSuffix(wantCmd, " hook claude || true") {
		t.Errorf("unix hook command is not guarded: %s", wantCmd)
	}

	// Idempotent: a second apply plans nothing.
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}}); len(r.Changes) != 0 {
		t.Fatalf("second apply changed %v", changeIDs(r.Changes))
	}
}

func TestClaudeUnmergeRemovesOnlyOurEntries(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	writeFile(t, path, claudeSettingsBefore)
	te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}})

	// The user edits the file afterwards: a foreign hook next to ours in a
	// group we created, and a new top-level key.
	var doc map[string]any
	json.Unmarshal([]byte(readFile(t, path)), &doc)
	hooks := doc["hooks"].(map[string]any)
	ss := hooks["SessionStart"].([]any)
	ss = append(ss, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "echo hi"}}})
	hooks["SessionStart"] = ss
	doc["theme"] = "dark"
	b, _ := json.MarshalIndent(doc, "", "  ")
	writeFile(t, path, string(b))

	r := te.integrate(t, integrateOpts{apply: true, remove: []string{hClaude}})
	if len(r.Changes) != 1 || r.Changes[0].Action != "modify" {
		t.Fatalf("changes = %+v", r.Changes)
	}
	s := parseClaude(t, readFile(t, path))
	for ev, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if isOurCommand(h.Command) {
					t.Errorf("%s still has our entry", ev)
				}
			}
		}
	}
	if got := s.Hooks["SessionStart"]; len(got) != 1 || got[0].Hooks[0].Command != "echo hi" {
		t.Errorf("user's SessionStart hook lost: %+v", got)
	}
	if _, ok := s.Hooks["Notification"]; ok {
		t.Error("event array created by stagent was left behind empty")
	}
	if got := s.Hooks["Stop"]; len(got) != 1 || got[0].Hooks[0].Command != "notify-send done" {
		t.Errorf("pre-existing Stop entry changed: %+v", got)
	}
	if !strings.Contains(readFile(t, path), `"theme": "dark"`) {
		t.Error("user's new key lost")
	}
	if te.m.config(hClaude, path) != nil {
		t.Error("manifest still records the Claude integration")
	}
}

func TestClaudeRemoveRestoresBackupWhenUntouched(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	// Formatting that our encoder would normalise (4-space indent, inline
	// array): restoring the backup must bring it back byte for byte.
	orig := "{\n    \"permissions\": {\"allow\": [\"Bash(ls)\"]},\n    \"hooks\": {}\n}"
	writeFile(t, path, orig)
	te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}})
	rec := te.m.config(hClaude, path)
	if rec == nil || rec.Backup == "" || readFile(t, rec.Backup) != orig {
		t.Fatalf("backup not recorded: %+v", rec)
	}
	r := te.integrate(t, integrateOpts{apply: true, remove: []string{hClaude}})
	if len(r.Changes) != 1 || !strings.HasPrefix(r.Changes[0].Summary, "restore") {
		t.Fatalf("expected a restore, got %+v", r.Changes)
	}
	if got := readFile(t, path); got != orig {
		t.Fatalf("restored content differs:\n%q\nwant\n%q", got, orig)
	}
}

func TestClaudeCreatedSettingsAreDeletedOnRemove(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.claudeSettings()
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}})
	if len(r.Changes) != 1 || r.Changes[0].Action != "create" {
		t.Fatalf("changes = %+v", r.Changes)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("created settings: %v %v", st, err)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{hClaude}})
	if exists(path) {
		t.Fatal("settings.json created by stagent was not deleted")
	}
}

func TestWindowsHookCommandInvokesExeDirectly(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.l.Bin = `C:\Users\u\.ssh-term\agent\bin\stagent.exe`
	cmd := te.hookCommand(hClaude)
	if cmd != `"C:/Users/u/.ssh-term/agent/bin/stagent.exe" hook claude` {
		t.Fatalf("windows command = %s", cmd)
	}
	if !isOurCommand(cmd) || !isOurCommand(`"C:\Users\u\.ssh-term\agent\bin\stagent.exe" hook claude`) {
		t.Fatal("windows commands not recognised as ours")
	}
	// The raw config.toml line carries TOML-escaped backslashes.
	if line := notifyLine(te.l.Bin); !isOurCommand(line) || !strings.Contains(line, `"C:\\Users\\u\\`) {
		t.Fatalf("windows notify line %s", line)
	}
}
