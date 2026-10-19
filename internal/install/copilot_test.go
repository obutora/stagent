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

// copilotHookEntry is one command hook of a Copilot CLI hooks file.
type copilotHookEntry struct {
	Type       string `json:"type"`
	Bash       string `json:"bash"`
	PowerShell string `json:"powershell"`
	TimeoutSec int    `json:"timeoutSec"`
}

func readCopilotHooks(t *testing.T, path string) (version int, hooks map[string][]copilotHookEntry) {
	t.Helper()
	var f struct {
		Version int                           `json:"version"`
		Hooks   map[string][]copilotHookEntry `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f.Version, f.Hooks
}

// Integrating Copilot CLI writes stagent's hooks file into
// ~/.copilot/hooks: PascalCase events (the payload comes snake_case, as
// Claude Code's), each running `stagent hook copilot <event>` from bash and
// from PowerShell. Removing it deletes the file; doctor reports Copilot.
func TestCopilotHooks(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".copilot", "hooks", "ssh-term-stagent.json")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "copilot" {
		t.Fatalf("changes = %s", got)
	}
	version, hooks := readCopilotHooks(t, path)
	if version != 1 {
		t.Errorf("version = %d", version)
	}
	events := slices.Sorted(maps.Keys(hooks))
	if want := []string{"Notification", "PermissionRequest", "SessionEnd", "SessionStart", "Stop", "UserPromptSubmit"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	bin := te.l.Bin
	for ev, entries := range hooks {
		if len(entries) != 1 {
			t.Fatalf("%s: %d entries", ev, len(entries))
		}
		h := entries[0]
		wantBash := "[ -x " + shellQuote(bin) + " ] && " + shellQuote(bin) + " hook copilot " + ev + " || true"
		if h.Type != "command" || h.Bash != wantBash || h.TimeoutSec != hookTimeoutSec {
			t.Errorf("%s: %+v", ev, h)
		}
		if !strings.Contains(h.PowerShell, "& '"+bin+"' hook copilot "+ev) {
			t.Errorf("%s: powershell %q", ev, h.PowerShell)
		}
	}
	if h := copilotReport(t, te); !h.Integrated || h.ConfigPath != path {
		t.Fatalf("doctor: %+v", h)
	}

	// Integrating again changes nothing.
	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}

	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessCopilot}})
	if exists(path) || exists(te.home(".copilot")) {
		t.Fatalf("hooks left behind: %v, ~/.copilot %v", exists(path), exists(te.home(".copilot")))
	}
	if h := copilotReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

// On Windows Copilot CLI runs the powershell command of a hook; a missing
// binary is skipped there as the bash command's guard does.
func TestCopilotHooksWindows(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.l.Bin = `C:\Users\o'neil\.ssh-term\agent\bin\stagent.exe`
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}})
	_, hooks := readCopilotHooks(t, te.home(".copilot", "hooks", "ssh-term-stagent.json"))
	q := `'C:\Users\o''neil\.ssh-term\agent\bin\stagent.exe'`
	want := "if (Test-Path -LiteralPath " + q + ") { & " + q + " hook copilot Stop }"
	if got := hooks["Stop"][0].PowerShell; got != want {
		t.Errorf("powershell = %q, want %q", got, want)
	}
}

// COPILOT_HOME moves Copilot CLI's directory, hooks included.
func TestCopilotHooksHonorCopilotHome(t *testing.T) {
	te := newTestEnv(t, "linux")
	dir := filepath.Join(t.TempDir(), "copilot-home")
	te.vars["COPILOT_HOME"] = dir
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}})
	if !exists(filepath.Join(dir, "hooks", "ssh-term-stagent.json")) {
		t.Error("no hooks file under COPILOT_HOME")
	}
	if exists(te.home(".copilot")) {
		t.Error("~/.copilot written")
	}
}

// A hooks file of the user's at that path is left alone.
func TestCopilotHooksLeaveUsersFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".copilot", "hooks", "ssh-term-stagent.json")
	mine := `{"version":1,"hooks":{"Stop":[{"type":"command","bash":"./notify.sh"}]}}` + "\n"
	writeFile(t, path, mine)
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, path) != mine {
		t.Fatalf("changes %+v, %v", r.Changes, err)
	}
}

// `stagent uninstall --level unhook` takes the hooks file out too.
func TestCopilotHooksUnhook(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".copilot", "hooks", "ssh-term-stagent.json")
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessCopilot}})
	te.reload()
	te.uninstall("unhook", false, false)
	if exists(path) {
		t.Fatal("hooks file left after unhook")
	}
}

func copilotReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessCopilot {
			return h
		}
	}
	t.Fatal("doctor has no copilot")
	return HarnessReport{}
}
