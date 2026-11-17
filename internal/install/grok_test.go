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

// grokHookGroup is one matcher group of a Grok Build hooks file.
type grokHookGroup struct {
	Matcher string `json:"matcher"`
	Hooks   []struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
	} `json:"hooks"`
}

func readGrokHooks(t *testing.T, path string) map[string][]grokHookGroup {
	t.Helper()
	var f struct {
		Hooks map[string][]grokHookGroup `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &f); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return f.Hooks
}

// Integrating Grok Build writes stagent's hooks file into ~/.grok/hooks,
// which Grok Build reads with every *.json there: Claude Code's form, each
// event running `stagent hook grok <event>` (PostToolUse for its question
// tool only). Removing it deletes the file; doctor reports Grok Build.
func TestGrokHooks(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".grok", "hooks", "ssh-term-stagent.json")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessGrok}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "grok" {
		t.Fatalf("changes = %s", got)
	}
	hooks := readGrokHooks(t, path)
	events := slices.Sorted(maps.Keys(hooks))
	if want := []string{"Notification", "PostToolUse", "PreToolUse", "SessionEnd", "SessionStart", "Stop", "StopCancelled", "UserPromptSubmit"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	bin := te.l.Bin
	for ev, groups := range hooks {
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", ev, groups)
		}
		want := ""
		if ev == "PostToolUse" {
			want = "ask_user_question"
		}
		if groups[0].Matcher != want {
			t.Errorf("%s: matcher %q, want %q", ev, groups[0].Matcher, want)
		}
		h := groups[0].Hooks[0]
		wantCmd := "[ -x " + shellQuote(bin) + " ] && " + shellQuote(bin) + " hook grok " + ev + " || true"
		if h.Type != "command" || h.Command != wantCmd || h.Args != nil || h.Timeout != hookTimeoutSec {
			t.Errorf("%s: %+v", ev, h)
		}
	}
	if h := grokReport(t, te); !h.Integrated || h.ConfigPath != path {
		t.Fatalf("doctor: %+v", h)
	}

	if r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessGrok}}); len(r.Changes) != 0 {
		t.Fatalf("second integrate: %+v", r.Changes)
	}

	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessGrok}})
	if exists(path) || exists(te.home(".grok")) {
		t.Fatalf("hooks left behind: %v, ~/.grok %v", exists(path), exists(te.home(".grok")))
	}
	if h := grokReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

// On Windows Grok Build runs a hook's command under the shell it finds
// (pwsh, Windows PowerShell or Git Bash): stagent's hooks give the binary
// with its arguments (args), which runs with no shell.
func TestGrokHooksWindows(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.l.Bin = `C:\Users\o'neil\.ssh-term\agent\bin\stagent.exe`
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessGrok}})
	path := te.home(".grok", "hooks", "ssh-term-stagent.json")
	h := readGrokHooks(t, path)["Stop"][0].Hooks[0]
	if h.Command != te.l.Bin || !slices.Equal(h.Args, []string{"hook", "grok", "Stop"}) {
		t.Errorf("Stop hook %+v", h)
	}
	if !grokReport(t, te).Integrated {
		t.Error("doctor does not see the Windows hooks")
	}
}

// GROK_HOME moves Grok Build's directory, hooks included.
func TestGrokHooksHonorGrokHome(t *testing.T) {
	te := newTestEnv(t, "linux")
	dir := filepath.Join(t.TempDir(), "grok-home")
	te.vars["GROK_HOME"] = dir
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessGrok}})
	if !exists(filepath.Join(dir, "hooks", "ssh-term-stagent.json")) {
		t.Error("no hooks file under GROK_HOME")
	}
	if exists(te.home(".grok")) {
		t.Error("~/.grok written")
	}
}

// A hooks file of the user's at that path is left alone.
func TestGrokHooksLeaveUsersFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".grok", "hooks", "ssh-term-stagent.json")
	mine := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"./notify.sh"}]}]}}` + "\n"
	writeFile(t, path, mine)
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessGrok}})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, path) != mine {
		t.Fatalf("changes %+v, %v", r.Changes, err)
	}
}

// `stagent uninstall --level unhook` takes the hooks file out too.
func TestGrokHooksUnhook(t *testing.T) {
	te := newTestEnv(t, "linux")
	path := te.home(".grok", "hooks", "ssh-term-stagent.json")
	te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessGrok}})
	te.reload()
	te.uninstall("unhook", false, false)
	if exists(path) {
		t.Fatal("hooks file left after unhook")
	}
}

func grokReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessGrok {
			return h
		}
	}
	t.Fatal("doctor has no grok")
	return HarnessReport{}
}
