package install

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// orcaAntigravityHooks is ~/.gemini/config/hooks.json as Orca writes it
// for its own Antigravity hooks (its group "orca-status"; home
// shortened).
const orcaAntigravityHooks = `{
  "orca-status": {
    "PreInvocation": [
      {
        "type": "command",
        "command": "/bin/sh -c 'if [ -f '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -r '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -x '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ]; then ORCA_ANTIGRAVITY_EVENT='\\''PreInvocation'\\'' /bin/sh '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\''; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi'",
        "timeout": 10
      }
    ],
    "PostInvocation": [
      {
        "type": "command",
        "command": "/bin/sh -c 'if [ -f '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -r '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -x '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ]; then ORCA_ANTIGRAVITY_EVENT='\\''PostInvocation'\\'' /bin/sh '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\''; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi'",
        "timeout": 10
      }
    ],
    "Stop": [
      {
        "type": "command",
        "command": "/bin/sh -c 'if [ -f '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -r '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -x '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ]; then ORCA_ANTIGRAVITY_EVENT='\\''Stop'\\'' /bin/sh '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\''; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi'",
        "timeout": 10
      }
    ],
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/bin/sh -c 'if [ -f '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -r '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -x '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ]; then ORCA_ANTIGRAVITY_EVENT='\\''PreToolUse'\\'' /bin/sh '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\''; else printf '\\''%s\\n'\\'' '\\''{\"decision\":\"ask\"}'\\''; { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi'",
            "timeout": 10
          }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "*",
        "hooks": [
          {
            "type": "command",
            "command": "/bin/sh -c 'if [ -f '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -r '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ] && [ -x '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\'' ]; then ORCA_ANTIGRAVITY_EVENT='\\''PostToolUse'\\'' /bin/sh '\\''/home/u/.orca/agent-hooks/antigravity-hook.sh'\\''; else { command -p cat 2>/dev/null || cat; } >/dev/null 2>&1 || :; fi'",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
`

// agyHandler is one handler of an Antigravity CLI hooks group.
type agyHandler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// agyEntry is a flat handler or a matcher group of handlers.
type agyEntry struct {
	agyHandler
	Matcher *string      `json:"matcher"`
	Hooks   []agyHandler `json:"hooks"`
}

func readAgyHooks(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(readFile(t, path)), &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f
}

func readAgyGroup(t *testing.T, path string) map[string][]agyEntry {
	t.Helper()
	var g map[string][]agyEntry
	raw, ok := readAgyHooks(t, path)[antigravityGroup]
	if !ok {
		t.Fatalf("%s has no group %s", path, antigravityGroup)
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return g
}

func agyReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessAntigravity {
			return h
		}
	}
	t.Fatal("doctor has no antigravity")
	return HarnessReport{}
}

// Integrating Antigravity CLI where it has no hooks file yet creates
// ~/.gemini/config/hooks.json with stagent's group: a flat handler for
// SessionStart, PreInvocation and Stop, PreToolUse for every tool and
// PostToolUse for its question tool in matcher groups, each running
// `stagent hook antigravity <event>`. A second integrate changes nothing;
// removing deletes the file and the directories made for it. No restart
// note: a running Antigravity CLI reads the file again.
func TestAntigravityHooks(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.bins["agy"] = "/home/u/.local/bin/agy"
	path := te.home(".gemini", "config", "hooks.json")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "antigravity" || r.Changes[0].Action != "create" {
		t.Fatalf("changes = %+v", r.Changes)
	}
	for _, n := range r.Notes {
		if strings.HasSuffix(n.Code, "_restart") {
			t.Errorf("note %+v", n)
		}
	}
	if keys := slices.Collect(maps.Keys(readAgyHooks(t, path))); !slices.Equal(keys, []string{antigravityGroup}) {
		t.Fatalf("groups %v", keys)
	}
	g := readAgyGroup(t, path)
	bin := shellQuote(te.l.Bin)
	for _, ev := range []string{"SessionStart", "PreInvocation", "PreToolUse", "PostToolUse", "Stop"} {
		entries := g[ev]
		if len(entries) != 1 {
			t.Fatalf("%s: %+v", ev, entries)
		}
		h, matcher := entries[0].agyHandler, ""
		switch ev {
		case "PreToolUse", "PostToolUse":
			if entries[0].Matcher == nil || len(entries[0].Hooks) != 1 || entries[0].Command != "" {
				t.Fatalf("%s: %+v, want one matcher group", ev, entries[0])
			}
			h, matcher = entries[0].Hooks[0], *entries[0].Matcher
		default:
			if entries[0].Matcher != nil || entries[0].Hooks != nil {
				t.Fatalf("%s: %+v, want a flat handler", ev, entries[0])
			}
		}
		if want := map[string]string{"PreToolUse": "*", "PostToolUse": "ask_question"}[ev]; matcher != want {
			t.Errorf("%s: matcher %q, want %q", ev, matcher, want)
		}
		if want := "[ -x " + bin + " ] && " + bin + " hook antigravity " + ev + " || true"; h.Type != "command" || h.Command != want || h.Timeout != hookTimeoutSec {
			t.Errorf("%s: %+v", ev, h)
		}
	}
	if len(g) != 5 {
		t.Errorf("events %v", g)
	}
	if h := agyReport(t, te); !h.Integrated || h.ConfigPath != path || !h.Found {
		t.Fatalf("doctor: %+v", h)
	}

	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}

	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessAntigravity}})
	if exists(path) || exists(te.home(".gemini")) {
		t.Fatalf("left behind: hooks.json %v, ~/.gemini %v", exists(path), exists(te.home(".gemini")))
	}
	if h := agyReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

// The hooks file is shared (the Antigravity app, Orca): integrating adds
// stagent's group after the others, which stay as they were, and is
// idempotent; removing it gives the file back as it was.
func TestAntigravityHooksKeepOtherGroups(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".gemini", "config", "hooks.json")
	writeFile(t, path, orcaAntigravityHooks)
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
	got := readFile(t, path)
	orca := strings.TrimSuffix(strings.TrimSuffix(orcaAntigravityHooks, "\n"), "\n}")
	if !strings.HasPrefix(got, orca+",\n  \""+antigravityGroup+"\": {") {
		t.Fatalf("Orca's group not kept as it was:\n%s", got)
	}
	if !sameJSON(readAgyHooks(t, path)["orca-status"], readAgyHooks0(t, orcaAntigravityHooks)["orca-status"]) {
		t.Fatal("Orca's group changed")
	}
	if g := readAgyGroup(t, path); len(g) != 5 {
		t.Fatalf("stagent's group %v", g)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, %v: the file's own mode is kept", st.Mode(), err)
	}
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessAntigravity}})
	if got := readFile(t, path); got != orcaAntigravityHooks {
		t.Fatalf("after remove:\n%s", got)
	}
}

// A file edited since stagent wrote it loses only stagent's group on
// removal: the user's edit and the other groups stay. A group that went
// stale (an older command) is updated in place.
func TestAntigravityHooksRemoveAfterEdits(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".gemini", "config", "hooks.json")
	writeFile(t, path, orcaAntigravityHooks)
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
	stale := strings.ReplaceAll(readFile(t, path), "hook antigravity Stop", "hook antigravity Stop --old")
	mine := `"lint": {"PostToolUse": [{"matcher": "edit_file", "hooks": [{"command": "./lint.sh"}]}]}`
	stale = strings.Replace(stale, "{\n", "{\n  "+mine+",\n", 1)
	writeFile(t, path, stale)
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
	if len(r.Changes) != 1 || r.Changes[0].Action != "modify" || strings.Contains(readFile(t, path), "--old") {
		t.Fatalf("stale group not updated: %+v\n%s", r.Changes, readFile(t, path))
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessAntigravity}})
	f := readAgyHooks(t, path)
	if _, ok := f[antigravityGroup]; ok || f["lint"] == nil || !sameJSON(f["orca-status"], readAgyHooks0(t, orcaAntigravityHooks)["orca-status"]) {
		t.Fatalf("after remove:\n%s", readFile(t, path))
	}
}

// A group of that name not made by stagent, or a file that is no JSON
// object, is left alone.
func TestAntigravityHooksLeaveUsersFile(t *testing.T) {
	for _, mine := range []string{
		`{"ssh-term-stagent": {"Stop": [{"command": "./notify.sh"}]}}` + "\n",
		`["not", "groups"]` + "\n",
		`{"broken": ` + "\n",
	} {
		te := newTestEnv(t, "linux")
		path := te.home(".gemini", "config", "hooks.json")
		writeFile(t, path, mine)
		te.reload()
		r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
		if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, path) != mine {
			t.Errorf("%s: changes %+v, %v", mine, r.Changes, err)
		}
	}
}

// On Windows Antigravity CLI runs a hook's command with cmd /c: the
// binary's path with backslashes (cmd takes / for a switch), quoted when
// it has a space. doctor still sees stagent's group.
func TestAntigravityHooksWindows(t *testing.T) {
	for bin, want := range map[string]string{
		`C:\Users\u\.ssh-term\agent\bin\stagent.exe`:        `C:\Users\u\.ssh-term\agent\bin\stagent.exe hook antigravity Stop`,
		`C:/Users/Jane Doe/.ssh-term/agent/bin/stagent.exe`: `"C:\Users\Jane Doe\.ssh-term\agent\bin\stagent.exe" hook antigravity Stop`,
	} {
		te := newTestEnv(t, "windows")
		te.l.Bin = bin
		te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
		path := te.home(".gemini", "config", "hooks.json")
		if h := readAgyGroup(t, path)["Stop"][0]; h.Command != want {
			t.Errorf("Stop hook %+v, want %s", h, want)
		}
		if !agyReport(t, te).Integrated {
			t.Error("doctor does not see the Windows hooks")
		}
	}
}

// `stagent uninstall --level unhook` takes stagent's group out too.
func TestAntigravityHooksUnhook(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".gemini", "config", "hooks.json")
	writeFile(t, path, orcaAntigravityHooks)
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessAntigravity}})
	te.reload()
	te.uninstall("unhook", false, false)
	if got := readFile(t, path); got != orcaAntigravityHooks {
		t.Fatalf("after unhook:\n%s", got)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
}

func readAgyHooks0(t *testing.T, src string) map[string]json.RawMessage {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal([]byte(src), &f); err != nil {
		t.Fatal(err)
	}
	return f
}
