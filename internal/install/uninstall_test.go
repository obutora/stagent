package install

import (
	"strings"
	"testing"
)

// installed sets up a full installation: binary, manifest, Claude hooks,
// bash wrapper and the systemd service, with a daemon running.
func installed(t *testing.T) (*testEnv, string) {
	t.Helper()
	te := newTestEnv(t, "linux")
	te.bins["systemctl"] = "/usr/bin/systemctl"
	te.vars["SHELL"] = "/bin/bash"
	const settings = "{\n  \"model\": \"opus\"\n}\n"
	writeFile(t, te.claudeSettings(), settings)
	writeFile(t, te.home(".bashrc"), "# my rc\n")
	if _, err := te.install(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, te.l.Bin, "binary")
	te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}, shellWrapper: true, service: true})
	if !exists(te.systemdUnitPath()) || !te.run.ran("systemctl --user enable stagent.service") {
		t.Fatalf("service not installed; calls %v", te.run.calls)
	}
	te.daemon.running, te.daemon.pid, te.daemon.exitOnShutdown = true, 4242, true
	te.run.calls = nil
	// systemctl is-active reports the service running until it is stopped.
	active := true
	te.run.respond = func(name string, args []string) (string, error) {
		switch strings.Join(args, " ") {
		case "--user is-active stagent.service":
			if active {
				return "active\n", nil
			}
			return "inactive\n", errString("exit status 3")
		case "--user stop stagent.service", "--user disable --now stagent.service":
			active = false
		}
		return "", nil
	}
	return te, settings
}

func stepActions(r *UninstallReport) string {
	var s []string
	for _, st := range r.Steps {
		if !st.OK {
			s = append(s, st.Action+"!"+st.Error)
			continue
		}
		s = append(s, st.Action)
	}
	return strings.Join(s, ",")
}

func TestUninstallStopKeepsIntegrations(t *testing.T) {
	te, _ := installed(t)
	te.reload()
	r := te.uninstall("stop", false, false)
	if got := stepActions(r); got != "stop-service,stop-daemon" {
		t.Fatalf("steps = %s", got)
	}
	if te.daemon.shutdowns != 1 || te.daemon.running {
		t.Fatal("daemon not shut down over IPC")
	}
	if len(r.Remaining) != 0 {
		t.Fatalf("remaining after stop = %+v", r.Remaining)
	}
	if !strings.Contains(readFile(t, te.claudeSettings()), "hook claude") || !exists(te.systemdUnitPath()) {
		t.Fatal("stop removed integrations")
	}
}

func TestUninstallStopKillsUnresponsiveDaemon(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.daemon.running, te.daemon.pid = true, 777 // ignores shutdown
	r := te.uninstall("stop", false, false)
	if got := stepActions(r); got != "kill-daemon" {
		t.Fatalf("steps = %s", got)
	}
	if len(te.killed) != 1 || te.killed[0] != 777 {
		t.Fatalf("killed = %v", te.killed)
	}
}

func TestUninstallUnhookRestoresConfigsAndRemovesService(t *testing.T) {
	te, settings := installed(t)
	te.reload()
	r := te.uninstall("unhook", false, false)
	for _, want := range []string{"stop-service", "stop-daemon", "restore", "disable-service"} {
		if !strings.Contains(","+stepActions(r)+",", ","+want+",") {
			t.Errorf("missing step %s in %s", want, stepActions(r))
		}
	}
	if got := readFile(t, te.claudeSettings()); got != settings {
		t.Errorf("settings.json = %q", got)
	}
	if got := readFile(t, te.home(".bashrc")); got != "# my rc\n" {
		t.Errorf(".bashrc = %q", got)
	}
	if exists(te.systemdUnitPath()) || !te.run.ran("systemctl --user disable --now stagent.service") || !te.run.ran("systemctl --user daemon-reload") {
		t.Errorf("service not removed; calls %v", te.run.calls)
	}
	if len(r.Remaining) != 0 {
		t.Errorf("remaining = %+v", r.Remaining)
	}
	// The installation itself stays, with a manifest that no longer lists
	// integrations but keeps the backups for purge.
	te.reload()
	if !exists(te.l.Bin) || len(te.m.Configs) != 0 || len(te.m.Services) != 0 || len(te.m.Backups) == 0 {
		t.Fatalf("manifest after unhook: %+v", te.m)
	}
}

func TestUninstallPurgeRemovesEverything(t *testing.T) {
	te, _ := installed(t)
	writeFile(t, te.l.UploadsDir+"/photo.png", "x")
	te.reload()
	backups := append([]string(nil), te.m.Backups...)
	r := te.uninstall("purge", true, false)
	if len(r.Failed) != 0 {
		t.Fatalf("failed = %+v", r.Failed)
	}
	for _, p := range append([]string{te.l.Root, te.l.UploadsDir}, backups...) {
		if exists(p) {
			t.Errorf("%s still exists", p)
		}
		found := false
		for _, rm := range r.Removed {
			if rm == p {
				found = true
			}
		}
		if !found {
			t.Errorf("%s not reported as removed: %v", p, r.Removed)
		}
	}
	if exists(te.home(".ssh-term")) {
		t.Error("empty ~/.ssh-term left behind")
	}
	if len(r.Remaining) != 0 {
		t.Errorf("remaining = %+v", r.Remaining)
	}
}

func TestUninstallRemovesWindowsTasks(t *testing.T) {
	te := newTestEnv(t, "windows")
	te.run.respond = func(name string, args []string) (string, error) {
		if name == "schtasks" && strings.Join(args, " ") == "/Query /FO CSV /NH" {
			return "\"\\Microsoft\\Other\",\"N/A\",\"Ready\"\r\n\"\\stagent\\daemon\",\"N/A\",\"Running\"\r\n\"\\stagent\\spawn-0a1b2c\",\"N/A\",\"Ready\"\r\n", nil
		}
		return "", nil
	}
	r := te.uninstall("unhook", false, false)
	for _, want := range []string{`schtasks /Delete /TN \stagent\daemon /F`, `schtasks /Delete /TN \stagent\spawn-0a1b2c /F`} {
		if !te.run.ran(want) {
			t.Errorf("did not run %q; calls %v", want, te.run.calls)
		}
	}
	if te.run.ran(`schtasks /Delete /TN \Microsoft\Other`) {
		t.Error("deleted a foreign task")
	}
	if !strings.Contains(stepActions(r), "delete-task") {
		t.Errorf("steps = %s", stepActions(r))
	}
}
