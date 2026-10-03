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

// install after an update replaces a running daemon of another version:
// stopped deliberately, started again through the login service when there
// is one (detached when not, or when the service manager refuses), and
// reported only once the new version answers.
func TestInstallReplacesDaemonOfAnotherVersion(t *testing.T) {
	for _, c := range []struct {
		name                 string
		running              bool
		version              string
		service, serviceFail bool
		spawnFails           bool
		replaced             string
		shutdowns, spawns    int
	}{
		{name: "older daemon", running: true, version: "0.0.9", replaced: "0.0.9", shutdowns: 1, spawns: 1},
		{name: "same version", running: true},
		{name: "not running", version: "0.0.9"},
		{name: "through the service", running: true, version: "0.0.9", service: true, replaced: "0.0.9", shutdowns: 1},
		{name: "service refuses", running: true, version: "0.0.9", service: true, serviceFail: true, replaced: "0.0.9", shutdowns: 1, spawns: 1},
		{name: "does not come up", running: true, version: "0.0.9", spawnFails: true, shutdowns: 1, spawns: 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			te := newTestEnv(t, "linux")
			te.daemon.running, te.daemon.pid, te.daemon.version = c.running, 4242, c.version
			te.daemon.exitOnShutdown = true
			te.spawnFails = c.spawnFails
			if c.service {
				te.bins["systemctl"] = "/usr/bin/systemctl"
				writeFile(t, te.systemdUnitPath(), te.systemdUnitText())
				te.run.respond = func(name string, args []string) (string, error) {
					if strings.Join(args, " ") != "--user restart stagent.service" {
						return "", nil
					}
					if c.serviceFail {
						return "Failed to restart", errString("exit status 1")
					}
					te.daemon.running, te.daemon.version = true, ""
					return "", nil
				}
			}
			r, err := te.install()
			if err != nil {
				t.Fatal(err)
			}
			if r.ReplacedDaemon != c.replaced || te.daemon.shutdowns != c.shutdowns || te.spawns != c.spawns {
				t.Errorf("replaced %q, shutdowns %d, spawns %d", r.ReplacedDaemon, te.daemon.shutdowns, te.spawns)
			}
			if c.service && !te.run.ran("systemctl --user restart stagent.service") {
				t.Errorf("service not restarted; calls %v", te.run.calls)
			}
			failed := strings.Contains(strings.Join(r.Notes, "\n"), "replacing it failed")
			if failed != c.spawnFails {
				t.Errorf("notes %q", r.Notes)
			}
		})
	}
}

// install after an update rewrites the wrapper blocks already in place (and
// adds one to an existing ~/.bash_profile where bash has it), once; a host
// without the wrapper gets none.
func TestInstallRewritesExistingWrapperBlocks(t *testing.T) {
	const old = blockBegin + "\n" + blockAbout + `
__stagent_wrap() {
  "$HOME/.ssh-term/agent/bin/stagent" run -- "$@"
}
function claude { __stagent_wrap claude "$@"; }
` + blockEnd + "\n"
	te := newTestEnv(t, "darwin")
	te.vars["SHELL"] = "/bin/zsh"
	bashrc, profile, zshrc := te.home(".bashrc"), te.home(".bash_profile"), te.home(".zshrc")
	writeFile(t, bashrc, "alias ll='ls -l'\n\n"+old+"export A=1\n")
	writeFile(t, profile, "export PATH=/opt/homebrew/bin:$PATH\n")
	writeFile(t, zshrc, "setopt autocd\n")

	r, err := te.install()
	if err != nil {
		t.Fatal(err)
	}
	if got := changeIDs(r.Changes); strings.Join(got, ",") != "shell-bash,shell-bash-profile" {
		t.Fatalf("changes %v", got)
	}
	want := posixBlock(te.l.Bin)
	if got := readFile(t, bashrc); got != "alias ll='ls -l'\n\n"+want+"export A=1\n" {
		t.Errorf(".bashrc:\n%s", got)
	}
	if got := readFile(t, profile); got != "export PATH=/opt/homebrew/bin:$PATH\n\n"+want {
		t.Errorf(".bash_profile:\n%s", got)
	}
	if got := readFile(t, zshrc); got != "setopt autocd\n" {
		t.Errorf(".zshrc touched:\n%s", got)
	}
	if !strings.Contains(want, "--handoff=auto") || !strings.Contains(want, "export STAGENT_SHELL_WRAPPER=1") {
		t.Fatal("the current block lacks --handoff=auto or the marker")
	}

	te.reload()
	if r, err := te.install(); err != nil || len(r.Changes) != 0 {
		t.Fatalf("second install: %v, changes %v", err, r.Changes)
	}
	if r := te.integrate(t, integrateOpts{apply: true, remove: []string{"shell-wrapper"}}); len(r.Changes) != 2 {
		t.Fatalf("removal: changes %v", changeIDs(r.Changes))
	}
	if got := readFile(t, bashrc); got != "alias ll='ls -l'\nexport A=1\n" {
		t.Errorf(".bashrc after removal:\n%s", got)
	}
}

func TestInstallAddsNoWrapperToAHostWithout(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.vars["SHELL"] = "/bin/bash"
	writeFile(t, te.home(".bashrc"), "# my rc\n")
	writeFile(t, te.home(".bash_profile"), "# my profile\n")
	r, err := te.install()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Changes) != 0 || readFile(t, te.home(".bashrc")) != "# my rc\n" || readFile(t, te.home(".bash_profile")) != "# my profile\n" {
		t.Fatalf("changes %v", changeIDs(r.Changes))
	}
}
