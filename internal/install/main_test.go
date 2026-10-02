package install

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/paths"
)

func TestCLIOutputIsASCIIJSON(t *testing.T) {
	te := newTestEnv(t, "linux")
	home := filepath.Join(t.TempDir(), "café😀")
	t.Setenv(paths.EnvHome, home)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	te.l = l
	var out bytes.Buffer
	if code := mainWith("install", []string{"--json"}, &out, &out, te.env); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, b := range out.Bytes() {
		if b >= 0x80 {
			t.Fatalf("non-ASCII output: %s", out.String())
		}
	}
	if bytes.Count(out.Bytes(), []byte("\n")) != 1 {
		t.Fatalf("--json must print one line: %q", out.String())
	}
	var r InstallResult
	if err := json.Unmarshal(out.Bytes(), &r); err != nil || !r.OK || !strings.HasPrefix(r.Layout.Root, home) {
		t.Fatalf("decoded %+v (%v)", r, err)
	}
}

func TestCLIUsageErrors(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		args []string
	}{
		{"uninstall", []string{"--json"}},
		{"uninstall", []string{"--json", "--level", "everything"}},
		{"integrate", []string{"--json", "--plan", "--apply"}},
		{"integrate", []string{"--json", "--harness", "claude,aider"}},
	} {
		te := newTestEnv(t, "linux")
		var out bytes.Buffer
		code := mainWith(c.cmd, c.args, &out, &bytes.Buffer{}, te.env)
		var r map[string]string
		if code != 2 || json.Unmarshal(out.Bytes(), &r) != nil || r["error"] == "" {
			t.Errorf("%s %v: exit %d, output %q", c.cmd, c.args, code, out.String())
		}
	}
}

// install stops a daemon of an earlier version still listening in
// $XDG_RUNTIME_DIR, only for the real (non-isolated) installation on Linux.
func TestInstallStopsLegacyDaemon(t *testing.T) {
	for _, c := range []struct {
		goos     string
		isolated bool
		running  bool
		stop     bool
	}{
		{"linux", false, true, true},
		{"linux", false, false, false},
		{"linux", true, true, false},
		{"darwin", false, true, false},
	} {
		te := newTestEnv(t, c.goos)
		te.l.Isolated = c.isolated
		xdg := t.TempDir()
		te.vars["XDG_RUNTIME_DIR"] = xdg
		legacy := &fakeDaemon{running: c.running, exitOnShutdown: true}
		te.others[filepath.Join(xdg, "stagent", "stagent.sock")] = legacy
		r, err := te.install()
		if err != nil {
			t.Fatal(err)
		}
		noted := strings.Contains(strings.Join(r.Notes, "\n"), "Stopped the daemon of an earlier stagent version")
		if (legacy.shutdowns == 1) != c.stop || noted != c.stop {
			t.Errorf("%+v: shutdowns %d, notes %q", c, legacy.shutdowns, r.Notes)
		}
	}
}
