package install

import (
	"encoding/json"
	"strings"
	"testing"
)

const codexHooksBefore = `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "bash '/home/u/.codex/herdr-agent-state.sh' session",
            "timeout": 10,
            "type": "command"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/bin/sh '/home/u/.orca/agent-hooks/codex-hook.sh'",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`

// The multi-line string mentions [features] to make sure its content is not
// taken for a table header.
const codexConfigBefore = `model = "gpt-5"
instructions = """
Do not edit
[features]
hooks = false
"""
approvals_reviewer = "user"
[projects."/home/u/dev"]
trust_level = "trusted"

[hooks.state]

[hooks.state."/home/u/.codex/hooks.json:session_start:0:0"]
enabled = true
trusted_hash = "sha256:aaaa"
`

type codexHooksFile struct {
	Hooks map[string][]struct {
		Hooks []claudeHook `json:"hooks"`
	} `json:"hooks"`
}

func TestCodexHooksAndFeatureFlagRoundTrip(t *testing.T) {
	te := newTestEnv(t, "linux")
	writeFile(t, te.codexHooks(), codexHooksBefore)
	writeFile(t, te.codexConfig(), codexConfigBefore)

	r := te.integrate(t, integrateOpts{apply: true, harness: []string{hCodex}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "codex-hooks,codex-config" {
		t.Fatalf("changes = %s", got)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "trust") {
		t.Errorf("no note about Codex hook trust: %v", r.Notes)
	}

	var hf codexHooksFile
	if err := json.Unmarshal([]byte(readFile(t, te.codexHooks())), &hf); err != nil {
		t.Fatal(err)
	}
	ss := hf.Hooks["SessionStart"]
	if len(ss) != 2 || !strings.Contains(ss[0].Hooks[0].Command, "herdr") || !isOurCommand(ss[1].Hooks[0].Command) {
		t.Fatalf("SessionStart = %+v (ours must be appended after foreign groups)", ss)
	}
	if pr := hf.Hooks["PermissionRequest"]; len(pr) != 1 || pr[0].Hooks[0].Timeout != 10 {
		t.Fatalf("PermissionRequest = %+v", pr)
	}
	if len(hf.Hooks["PreToolUse"]) != 1 {
		t.Fatal("foreign PreToolUse changed")
	}

	cfg := readFile(t, te.codexConfig())
	if want := codexConfigBefore + "\n[features]\nhooks = true\n"; cfg != want {
		t.Fatalf("config.toml:\n%s\nwant:\n%s", cfg, want)
	}

	// Codex records trust for our hook (SessionStart group 1) after the
	// user accepts it.
	trusted := cfg + "[hooks.state.\"" + te.codexHooks() + ":session_start:1:0\"]\nenabled = true\ntrusted_hash = \"sha256:bbbb\"\n"
	writeFile(t, te.codexConfig(), trusted)

	te.integrate(t, integrateOpts{apply: true, remove: []string{hCodex}})
	if got := readFile(t, te.codexConfig()); got != codexConfigBefore {
		t.Fatalf("config.toml after remove:\n%s\nwant:\n%s", got, codexConfigBefore)
	}
	if got := readFile(t, te.codexHooks()); got != codexHooksBefore {
		t.Fatalf("hooks.json after remove:\n%s", got)
	}
}

func TestCodexFeatureLineEdits(t *testing.T) {
	cases := []struct {
		name, before, after string
	}{
		{"existing table", "a = 1\n\n[features]\nother = true\n\n[tui]\nx = 1\n",
			"a = 1\n\n[features]\nhooks = true\nother = true\n\n[tui]\nx = 1\n"},
		{"explicitly disabled", "[features]\nhooks = false # off\n", "[features]\nhooks = true\n"},
		{"crlf without final newline", "a = 1\r\n[tui]\r\nx = 1", "a = 1\r\n[tui]\r\nx = 1\r\n\r\n[features]\r\nhooks = true\r\n"},
		{"already enabled", "[features]\n  hooks = true\n", "[features]\n  hooks = true\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := parseTOMLLines([]byte(c.before))
			ed, err := ensureFeatureHooks(d)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(d.bytes()); got != c.after {
				t.Fatalf("got %q want %q", got, c.after)
			}
			if ed == nil {
				return
			}
			// Reverting restores the original lines (the missing final
			// newline aside, which the append had to add).
			revertFeatureHooks(d, *ed)
			want := c.before
			if !strings.HasSuffix(want, "\n") {
				want += "\r\n"
			}
			if got := string(d.bytes()); got != want {
				t.Fatalf("revert: got %q want %q", got, want)
			}
		})
	}
}

func TestCodexInlineFeaturesFallsBackToNotify(t *testing.T) {
	te := newTestEnv(t, "linux")
	before := "features = { hooks = false }\n"
	writeFile(t, te.codexConfig(), before)
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{hCodex}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "codex-config" {
		t.Fatalf("changes = %s", got)
	}
	cfg := readFile(t, te.codexConfig())
	if want := before + notifyLine(te.l.Bin) + "\n"; cfg != want {
		t.Fatalf("config.toml = %q want %q", cfg, want)
	}
	if exists(te.codexHooks()) {
		t.Fatal("hooks.json written although hooks cannot be enabled")
	}
}

func TestCodexNotifyFallbackNeverOverwritesUserNotify(t *testing.T) {
	te := newTestEnv(t, "windows") // Codex hooks are unsupported on Windows
	before := "model = \"o3\"\nnotify = [\"python3\", \"/home/u/notify.py\"]\n\n[tui]\nx = 1\n"
	writeFile(t, te.codexConfig(), before)
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{hCodex}})
	if len(r.Changes) != 0 {
		t.Fatalf("changes = %v", changeIDs(r.Changes))
	}
	if got := readFile(t, te.codexConfig()); got != before {
		t.Fatalf("user's config changed:\n%s", got)
	}
	if !strings.Contains(strings.Join(r.Notes, "\n"), "notify.py") {
		t.Errorf("no note about the existing notify: %v", r.Notes)
	}
	// Removing Codex must not touch the user's notify either.
	if r := te.integrate(t, integrateOpts{apply: true, remove: []string{hCodex}}); len(r.Changes) != 0 {
		t.Fatalf("remove changed %v", changeIDs(r.Changes))
	}
}

func TestCodexNotifyFallbackAddsAndRemovesOurLine(t *testing.T) {
	te := newTestEnv(t, "windows")
	before := "model = \"o3\"\n\n[tui]\nx = 1\n"
	writeFile(t, te.codexConfig(), before)
	te.integrate(t, integrateOpts{apply: true, harness: []string{hCodex}})
	want := "model = \"o3\"\n" + notifyLine(te.l.Bin) + "\n\n[tui]\nx = 1\n"
	if got := readFile(t, te.codexConfig()); got != want {
		t.Fatalf("config.toml = %q want %q", got, want)
	}
	// The user appends something, so removal is element-wise, not a restore.
	writeFile(t, te.codexConfig(), want+"y = 2\n")
	te.integrate(t, integrateOpts{apply: true, remove: []string{hCodex}})
	if got := readFile(t, te.codexConfig()); got != before+"y = 2\n" {
		t.Fatalf("after remove = %q", got)
	}
}

func TestCodexFeaturesListDecidesSupport(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.bins["codex"] = "/usr/bin/codex"
	te.run.respond = func(name string, args []string) (string, error) {
		if name == "/usr/bin/codex" && strings.Join(args, " ") == "features list" {
			return "apps  stable  true\nundo  beta  false\n", nil
		}
		return "", nil
	}
	if mode, why := te.codexMode(); mode != "notify" || !strings.Contains(why, "no hooks feature") {
		t.Fatalf("mode = %s (%s)", mode, why)
	}
	te.run.respond = func(string, []string) (string, error) { return "hooks  stable  false\n", nil }
	if mode, _ := te.codexMode(); mode != "hooks" {
		t.Fatalf("mode = %s", mode)
	}
}
