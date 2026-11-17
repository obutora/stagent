package install

import (
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// qwenManifest is what the test reads of stagent's Qwen Code extension.
type qwenManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hooks   map[string][]struct {
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Shell   string `json:"shell"`
			Timeout int    `json:"timeout"`
			Async   *bool  `json:"async"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readQwenManifest(t *testing.T, path string) qwenManifest {
	t.Helper()
	var m qwenManifest
	if err := json.Unmarshal([]byte(readFile(t, path)), &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

// Integrating Qwen Code installs an extension of stagent's,
// ~/.qwen/extensions/stagent/qwen-extension.json, whose hooks (Claude Code's
// form) run `stagent hook qwen <event>` and are waited for (no async: the
// order of the events matters). The user's settings.json is never touched.
// Removing it deletes the extension's directory; doctor reports Qwen Code.
func TestQwenExtension(t *testing.T) {
	te := newTestEnv(t, "linux")
	settings := te.home(".qwen", "settings.json")
	mine := "{\n  // the user's\n  \"model\": {\"name\": \"qwen3\"}\n}\n"
	writeFile(t, settings, mine)
	te.bins["qwen"] = "/usr/bin/qwen"
	path := te.home(".qwen", "extensions", "stagent", "qwen-extension.json")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessQwen}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "qwen" {
		t.Fatalf("changes = %s", got)
	}
	if n := len(notesWith(r.Notes, noteQwenRestart)); n != 1 {
		t.Errorf("%d restart notes", n)
	}
	m := readQwenManifest(t, path)
	if m.Name != "stagent" || m.Version == "" {
		t.Errorf("manifest %+v", m)
	}
	events := slices.Sorted(maps.Keys(m.Hooks))
	if want := []string{"Notification", "PermissionRequest", "PostToolUse", "PreToolUse", "SessionEnd", "SessionStart", "Stop", "StopFailure", "UserPromptSubmit"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	bin := te.l.Bin
	for ev, groups := range m.Hooks {
		if len(groups) != 1 || groups[0].Matcher != nil || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, groups)
		}
		h := groups[0].Hooks[0]
		want := "[ -x " + shellQuote(bin) + " ] && " + shellQuote(bin) + " hook qwen " + ev + " || true"
		if h.Type != "command" || h.Command != want || h.Shell != "" || h.Timeout != hookTimeoutSec || h.Async != nil {
			t.Errorf("%s: %+v", ev, h)
		}
	}
	if h := qwenReport(t, te); !h.Integrated || h.ConfigPath != path || !h.Found {
		t.Fatalf("doctor: %+v", h)
	}

	// Integrating again changes nothing.
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessQwen}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}

	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessQwen}})
	if exists(path) || exists(te.home(".qwen", "extensions")) {
		t.Fatalf("extension left behind: %v, extensions dir %v", exists(path), exists(te.home(".qwen", "extensions")))
	}
	if readFile(t, settings) != mine {
		t.Error("settings.json changed")
	}
	if h := qwenReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

// On Windows Qwen Code runs a hook's command in the shell it names: the
// PowerShell one, which skips a missing binary as the bash command does.
func TestQwenExtensionWindows(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.l.Bin = `C:\Users\o'neil\.ssh-term\agent\bin\stagent.exe`
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessQwen}})
	m := readQwenManifest(t, te.home(".qwen", "extensions", "stagent", "qwen-extension.json"))
	q := `'C:\Users\o''neil\.ssh-term\agent\bin\stagent.exe'`
	h := m.Hooks["Stop"][0].Hooks[0]
	if want := "if (Test-Path -LiteralPath " + q + ") { & " + q + " hook qwen Stop }"; h.Command != want || h.Shell != "powershell" {
		t.Errorf("command %q (shell %q), want %q", h.Command, h.Shell, want)
	}
}

// QWEN_HOME moves Qwen Code's directory, extensions included.
func TestQwenExtensionHonorsQwenHome(t *testing.T) {
	te := newTestEnv(t, "linux")
	dir := filepath.Join(t.TempDir(), "qwen-home")
	te.vars["QWEN_HOME"] = dir
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessQwen}})
	if !exists(filepath.Join(dir, "extensions", "stagent", "qwen-extension.json")) {
		t.Error("no extension under QWEN_HOME")
	}
	if exists(te.home(".qwen")) {
		t.Error("~/.qwen written")
	}
}

// An extension of the user's at that path is left alone.
func TestQwenExtensionLeavesUsersFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".qwen", "extensions", "stagent", "qwen-extension.json")
	mine := `{"name":"stagent","version":"1.0.0","hooks":{"Stop":[{"hooks":[{"type":"command","command":"./notify.sh"}]}]}}` + "\n"
	writeFile(t, path, mine)
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessQwen}})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, path) != mine {
		t.Fatalf("changes %+v, %v", r.Changes, err)
	}
}

// Qwen Code runs no hook while its settings turn them all off; doctor
// says so.
func TestQwenDoctorHooksDisabled(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessQwen}})
	if h := qwenReport(t, te); len(h.Notes) != 0 {
		t.Fatalf("notes %v", h.Notes)
	}
	writeFile(t, te.home(".qwen", "settings.json"), "{\n  // off for now\n  \"disableAllHooks\": true\n}\n")
	if h := qwenReport(t, te); len(h.Notes) != 1 || !strings.Contains(h.Notes[0], "disableAllHooks") {
		t.Fatalf("notes %v", h.Notes)
	}
}

func qwenReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessQwen {
			return h
		}
	}
	t.Fatal("doctor has no qwen")
	return HarnessReport{}
}
