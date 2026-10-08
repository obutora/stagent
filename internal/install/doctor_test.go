package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/hostid"
	"github.com/obutora/stagent/internal/paths"
)

func orphanTargets(r *DoctorReport) string {
	var s []string
	for _, o := range r.Orphans {
		s = append(s, o.Target+": "+o.Detail)
	}
	return strings.Join(s, "\n")
}

func TestDoctorReportsOrphans(t *testing.T) {
	te := newTestEnv(t, "linux")
	if _, err := te.install(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, te.l.Bin, "binary")
	te.integrate(t, integrateOpts{apply: true, harness: []string{hClaude}})

	// A hook entry written by hand (or by an older install) is ours but not
	// in the manifest; a tracked one is not an orphan.
	cmd, _ := json.Marshal(te.l.Bin + " hook codex") // a Windows path has backslashes
	writeFile(t, te.codexHooks(), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": `+string(cmd)+`, "timeout": 10}]}]}}`)
	te.reload()
	r := te.doctor()
	got := orphanTargets(r)
	if !strings.Contains(got, te.codexHooks()+": stagent hook entries for Stop; not recorded in the manifest") {
		t.Errorf("untracked codex entry not reported:\n%s", got)
	}
	if strings.Contains(got, te.claudeSettings()) {
		t.Errorf("tracked Claude entries reported as orphans:\n%s", got)
	}
	var claude, codex HarnessReport
	for _, h := range r.Harnesses {
		switch h.ID {
		case hClaude:
			claude = h
		case hCodex:
			codex = h
		}
	}
	if !claude.Integrated || !codex.Integrated || claude.Found {
		t.Errorf("harness reports: %+v %+v", claude, codex)
	}
	if len(r.Problems) != 0 {
		t.Errorf("problems = %v", r.Problems)
	}
	// install created the host id; doctor reports it.
	if r.HostID == "" || r.HostID != hostid.Read(te.l.HostID) {
		t.Errorf("host_id = %q, file %q", r.HostID, hostid.Read(te.l.HostID))
	}

	// With the binary gone every remaining element is an orphan.
	removeAll(t, te.l.Root)
	te.reload()
	r = te.doctor()
	got = orphanTargets(r)
	if !strings.Contains(got, te.claudeSettings()+": stagent hook entries for SessionStart") || !strings.Contains(got, "binary is missing") {
		t.Errorf("orphans without binary:\n%s", got)
	}
	if !strings.Contains(strings.Join(r.Problems, "\n"), "binary missing") {
		t.Errorf("problems = %v", r.Problems)
	}

	// unhook without a manifest still cleans them up.
	res := te.uninstall("unhook", false, false)
	if len(res.Remaining) != 0 || strings.Contains(readFile(t, te.claudeSettings()), "stagent") || strings.Contains(readFile(t, te.codexHooks()), "stagent") {
		t.Fatalf("orphans left after unhook: %+v", res.Remaining)
	}
}

func TestDoctorHarnessDiscoveryAndVersion(t *testing.T) {
	te := newTestEnv(t, "linux")
	bin := te.home(".bun", "bin", "omp")
	writeFile(t, bin, "#!/bin/sh\n")
	chmodX(t, bin)
	te.run.respond = func(name string, args []string) (string, error) {
		if name == bin && len(args) == 1 && args[0] == "--version" {
			return "omp/18.3.0\n", nil
		}
		return "", nil
	}
	r := te.doctor()
	for _, h := range r.Harnesses {
		if h.ID == hOmp && (!h.Found || h.Path != bin || h.Version != "18.3.0") {
			t.Fatalf("omp report = %+v", h)
		}
	}
}

// fakeLogind answers the busctl / loginctl queries of doctor: kill and
// linger are their outputs ("" fails), types the Type of each session of
// the user.
func fakeLogind(kill, linger *string, types *[]string) func(string, []string) (string, error) {
	return func(name string, args []string) (string, error) {
		var out string
		switch {
		case name == "busctl":
			out = *kill
		case name == "loginctl" && slices.Contains(args, "--property=Linger"):
			out = *linger
		case name == "loginctl" && slices.Contains(args, "--property=Sessions"):
			var ids []string
			for i := range *types {
				ids = append(ids, strconv.Itoa(i+1))
			}
			out = strings.Join(ids, " ") + "\n"
		case name == "loginctl" && args[0] == "show-session":
			out = strings.Join(*types, "\n\n") + "\n"
		default:
			return "", nil
		}
		if out == "" {
			return "", errString("exit status 1")
		}
		return out, nil
	}
}

// Detached sessions outlive logout unless logind kills the user's processes
// and no lingering keeps the user's service manager (and its scopes) alive,
// or the sockets are in $XDG_RUNTIME_DIR. Lingering is needed when it is
// off and, before any start through the wrapper, logind kills user
// processes or the user has a graphical session; after one, when that
// start does not survive logout where it runs, with the settings now.
func TestDoctorPersistence(t *testing.T) {
	te := newTestEnv(t, "linux")
	kill, linger, types := "b true\n", "no\n", []string{"tty"}
	te.run.respond = fakeLogind(&kill, &linger, &types)
	yes, no := true, false
	check := func(wantKill, wantLinger, wantNeeded *bool, wantReason string) {
		t.Helper()
		r := te.doctor()
		p := r.Persistence
		eq := func(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		reason := ""
		if p.LingerReason != nil {
			reason = *p.LingerReason
		}
		problem := strings.Contains(strings.Join(r.Problems, "\n"), "loginctl enable-linger")
		if p.RunDir != te.l.RunDir || !p.RunDirSurvivesLogout || !eq(p.KillUserProcesses, wantKill) || !eq(p.Linger, wantLinger) ||
			!eq(p.LingerNeeded, wantNeeded) || reason != wantReason || problem != (wantNeeded != nil && *wantNeeded) {
			t.Errorf("kill %q linger %q types %q: persistence %+v (kill %v linger %v needed %v), problems %q",
				kill, linger, types, p, p.KillUserProcesses, p.Linger, p.LingerNeeded, r.Problems)
		}
	}
	// Before any start through the wrapper.
	check(&yes, &no, &yes, lingerReasonKill)
	linger = "yes\n"
	check(&yes, &yes, &no, "")
	kill, linger = "b false\n", "no\n"
	check(&no, &no, &no, "")
	types = []string{"tty", "wayland"}
	check(&no, &no, &yes, lingerReasonGraphical)
	types = []string{"x11"}
	check(&no, &no, &yes, lingerReasonGraphical)
	linger = "yes\n"
	check(&no, &yes, &no, "")
	kill, linger = "", ""
	check(nil, nil, nil, "")
	// Lingering unknown, KillUserProcesses known: still judged.
	kill = "b true\n"
	check(&yes, nil, &yes, lingerReasonKill)

	// After a start through the wrapper, where it runs decides.
	lastRun := func(want *bool) {
		t.Helper()
		eq := func(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		if sw := te.doctor().ShellWrapper; sw.LastRunAt == nil || !eq(sw.LastRunSurvivesLogout, want) {
			t.Errorf("kill %q linger %q: shell_wrapper %+v (survives %v)", kill, linger, sw, sw.LastRunSurvivesLogout)
		}
	}
	kill, linger, types = "b true\n", "no\n", []string{"tty"}
	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 1, Placement: "other"})
	check(&yes, &no, &no, "")
	lastRun(&yes)
	kill, types = "b false\n", []string{"x11"}
	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 2, Placement: "manager"})
	check(&no, &no, &yes, lingerReasonLastRun)
	lastRun(&no)
	linger = "yes\n"
	check(&no, &yes, &no, "")
	lastRun(&yes)
	// Started under the user manager while lingering was on, lingering
	// turned off since: it ends at the next logout.
	kill, linger = "b true\n", "no\n"
	check(&yes, &no, &yes, lingerReasonLastRun)
	lastRun(&no)
	// Lingering unknown: the user manager's fate is too.
	linger = ""
	check(&yes, nil, &yes, lingerReasonKill)
	lastRun(nil)
	kill, linger = "b false\n", "no\n"
	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 3, Placement: "session"})
	check(&no, &no, &no, "")
	lastRun(&yes)
	kill = "b true\n"
	check(&yes, &no, &yes, lingerReasonLastRun)
	lastRun(&no)
	// No placement (logind could not tell): judged as before any start; a
	// survives_logout recorded on Linux is not taken.
	kill = "b false\n"
	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 4, SurvivesLogout: &yes})
	check(&no, &no, &yes, lingerReasonGraphical)
	lastRun(nil)
	// A record of stagent before 0.4.0 holds only the time.
	writeFile(t, te.l.WrapperRun, "1700000000000\n")
	check(&no, &no, &yes, lingerReasonGraphical)
	if sw := te.doctor().ShellWrapper; sw.LastRunAt == nil || *sw.LastRunAt != 1700000000000 || sw.LastRunSurvivesLogout != nil {
		t.Errorf("legacy shell_wrapper %+v", sw)
	}

	te.vars["XDG_RUNTIME_DIR"] = te.l.RunDir + "-other"
	if p := te.doctor().Persistence; !p.RunDirSurvivesLogout {
		t.Errorf("RunDir %s reported inside %s", p.RunDir, te.vars["XDG_RUNTIME_DIR"])
	}
	te.vars["XDG_RUNTIME_DIR"] = filepath.Dir(te.l.RunDir)
	if p := te.doctor().Persistence; p.RunDirSurvivesLogout {
		t.Errorf("RunDir %s not reported inside %s", p.RunDir, te.vars["XDG_RUNTIME_DIR"])
	}

	mac := newTestEnv(t, "darwin")
	if p := mac.doctor().Persistence; !p.RunDirSurvivesLogout || p.KillUserProcesses != nil || p.Linger != nil || p.LingerNeeded != nil || mac.run.ran("busctl") || mac.run.ran("loginctl") {
		t.Errorf("darwin: %+v, calls %v", p, mac.run.calls)
	}
}

// On macOS the last start through the wrapper and the running daemon each
// report whether they swapped to the per-user bootstrap port, with the
// reason and a problem when not; a daemon before 0.4.0 says nothing.
// Lingering stays out of it.
func TestDoctorBootstrapMacOS(t *testing.T) {
	te := newTestEnv(t, "darwin")
	yes, no := true, false
	const why = "bootstrap_look_up_per_user: (ipc/send) invalid destination port (0x10000003)"
	te.daemon.running = true
	r := te.doctor()
	if r.ShellWrapper.LastRunSurvivesLogout != nil || r.ShellWrapper.LastRunBootstrapError != nil || r.Daemon.BootstrapSwapped != nil || r.Daemon.BootstrapError != nil {
		t.Errorf("nothing recorded, old daemon: %+v %+v", r.ShellWrapper, r.Daemon)
	}

	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 1, SurvivesLogout: &yes})
	te.daemon.bootstrap = &yes
	r = te.doctor()
	if sw := r.ShellWrapper; sw.LastRunSurvivesLogout == nil || !*sw.LastRunSurvivesLogout || sw.LastRunBootstrapError != nil ||
		r.Daemon.BootstrapSwapped == nil || !*r.Daemon.BootstrapSwapped || r.Daemon.BootstrapError != nil || strings.Contains(strings.Join(r.Problems, "\n"), "bootstrap") {
		t.Errorf("swapped: %+v %+v %q", sw, r.Daemon, r.Problems)
	}

	te.l.WriteWrapperRun(paths.WrapperRunRecord{At: 2, SurvivesLogout: &no, BootstrapError: why})
	te.daemon.bootstrap, te.daemon.bootstrapErr = &no, why
	r = te.doctor()
	sw, d := r.ShellWrapper, r.Daemon
	if sw.LastRunSurvivesLogout == nil || *sw.LastRunSurvivesLogout || sw.LastRunBootstrapError == nil || *sw.LastRunBootstrapError != why {
		t.Errorf("shell_wrapper %+v", sw)
	}
	if d.BootstrapSwapped == nil || *d.BootstrapSwapped || d.BootstrapError == nil || *d.BootstrapError != why {
		t.Errorf("daemon %+v", d)
	}
	n := 0
	for _, p := range r.Problems {
		if strings.Contains(p, "per-user bootstrap port failed: "+why) {
			n++
		}
	}
	if n != 2 || r.Persistence.LingerNeeded != nil {
		t.Errorf("problems %q, linger_needed %v", r.Problems, r.Persistence.LingerNeeded)
	}
	b, _ := json.Marshal(r)
	for _, k := range []string{`"bootstrap_swapped":false`, `"last_run_bootstrap_error":"` + why} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON lacks %s: %s", k, b)
		}
	}
}

// The claude item carries remoteControlAtStartup from the user's
// ~/.claude/settings.json (null when absent or not a boolean); the other
// harness items have no such key. The file is only read.
func TestDoctorRemoteControlAtStartup(t *testing.T) {
	te := newTestEnv(t, "linux")
	for _, c := range []struct{ settings, want string }{
		{"", "null"},
		{`{"remoteControlAtStartup": true, "hooks": {}}`, "true"},
		{`{"remoteControlAtStartup": false}`, "false"},
		{`{"remoteControlAtStartup": "yes"}`, "null"},
		{`{"theme": "dark"}`, "null"},
		{`{not json`, "null"},
	} {
		os.Remove(te.claudeSettings())
		if c.settings != "" {
			writeFile(t, te.claudeSettings(), c.settings)
		}
		var doc struct {
			Harnesses []map[string]json.RawMessage `json:"harnesses"`
		}
		if err := json.Unmarshal(asciiJSON(te.doctor(), false), &doc); err != nil {
			t.Fatal(err)
		}
		for _, h := range doc.Harnesses {
			v, ok := h["remote_control_at_startup"]
			switch id := string(h["id"]); id {
			case `"claude"`:
				if !ok || string(v) != c.want {
					t.Errorf("settings %q: remote_control_at_startup = %s (present %v), want %s", c.settings, v, ok, c.want)
				}
			default:
				if ok {
					t.Errorf("%s item has remote_control_at_startup", id)
				}
			}
		}
		if c.settings != "" && readFile(t, te.claudeSettings()) != c.settings {
			t.Errorf("doctor changed %s", te.claudeSettings())
		}
	}
}

// doctor reports the daemon's last push failures from its file, with a
// problem per failing channel; none is `"last_error": {}`.
func TestDoctorNotifyLastError(t *testing.T) {
	te := newTestEnv(t, "linux")
	lastError := func() string {
		t.Helper()
		var doc struct {
			Notify map[string]json.RawMessage `json:"notify"`
		}
		if err := json.Unmarshal(asciiJSON(te.doctor(), false), &doc); err != nil {
			t.Fatal(err)
		}
		return string(doc.Notify["last_error"])
	}
	if got := lastError(); got != "{}" {
		t.Fatalf("last_error without failures = %s", got)
	}
	writeFile(t, te.l.NotifyErrors, `{"ntfy": {"at": 1767225600000, "status": 429, "error": "429 Too Many Requests"}}`)
	if got := lastError(); got != `{"ntfy":{"at":1767225600000,"status":429,"error":"429 Too Many Requests"}}` {
		t.Fatalf("last_error = %s", got)
	}
	problems := strings.Join(te.doctor().Problems, "\n")
	if !strings.Contains(problems, "notify: the last push to ntfy failed at ") || !strings.Contains(problems, ": 429 Too Many Requests") ||
		strings.Contains(problems, "webhook") {
		t.Fatalf("problems = %s", problems)
	}

	// The chat destination's line names the kind; revoked says what to do.
	at := time.UnixMilli(1767225600000).Format(time.RFC3339)
	for _, c := range []struct{ failure, line string }{
		{`{"at": 1767225600000, "status": 404, "error": "404 Not Found: Unknown Webhook (10015)", "kind": "revoked"}`,
			"notify: the last push to chat failed at " + at + " (revoked): 404 Not Found: Unknown Webhook (10015); " +
				"the destination was disabled on the service side; set a new URL from the app"},
		{`{"at": 1767225600000, "status": 403, "error": "403 Forbidden: bot was blocked by the user", "kind": "unreachable"}`,
			"notify: the last push to chat failed at " + at + " (unreachable): 403 Forbidden: bot was blocked by the user"},
		{`{"at": 1767225600000, "error": "not a Discord webhook"}`,
			"notify: the last push to chat failed at " + at + ": not a Discord webhook"},
	} {
		writeFile(t, te.l.NotifyErrors, `{"chat": `+c.failure+`}`)
		if problems := te.doctor().Problems; !slices.Contains(problems, c.line) {
			t.Errorf("problems = %q, want %q", problems, c.line)
		}
	}
}

func TestDoctorReportsBlockedLocation(t *testing.T) {
	te := newTestEnv(t, "linux")
	blocked := func() string {
		t.Helper()
		var doc struct {
			Blocked json.RawMessage `json:"blocked"`
		}
		if err := json.Unmarshal(asciiJSON(te.doctor(), false), &doc); err != nil {
			t.Fatal(err)
		}
		return string(doc.Blocked)
	}
	if got := blocked(); got != "null" {
		t.Fatalf("blocked without a foreign owner = %s", got)
	}
	te.blocked = func() *paths.OwnerError { return &paths.OwnerError{Path: "/tmp/stagent-1000", Owner: "mallory"} }
	if got := blocked(); got != `{"path":"/tmp/stagent-1000","owner":"mallory"}` {
		t.Fatalf("blocked = %s", got)
	}
	if problems := strings.Join(te.doctor().Problems, "\n"); !strings.Contains(problems, "another user (mallory) owns /tmp/stagent-1000") {
		t.Fatalf("problems = %s", problems)
	}
}
