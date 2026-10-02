package install

import (
	"path/filepath"
	"strings"
	"testing"
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
	writeFile(t, te.codexHooks(), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "`+te.l.Bin+` hook codex", "timeout": 10}]}]}}`)
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
	res := te.uninstall("unhook", false)
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

// Detached sessions outlive logout unless logind kills the user's processes
// and no lingering keeps the user's service manager (and its scopes) alive,
// or the sockets are in $XDG_RUNTIME_DIR.
func TestDoctorPersistence(t *testing.T) {
	te := newTestEnv(t, "linux")
	kill, linger := "b true\n", "no\n"
	te.run.respond = func(name string, args []string) (string, error) {
		switch name {
		case "busctl":
			return kill, nil
		case "loginctl":
			return linger, nil
		}
		return "", nil
	}
	check := func(wantKill, wantLinger *bool, wantProblem bool) {
		t.Helper()
		r := te.doctor()
		p := r.Persistence
		eq := func(a, b *bool) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
		problem := strings.Contains(strings.Join(r.Problems, "\n"), "loginctl enable-linger")
		if p.RunDir != te.l.RunDir || !p.RunDirSurvivesLogout || !eq(p.KillUserProcesses, wantKill) || !eq(p.Linger, wantLinger) || problem != wantProblem {
			t.Errorf("kill %q linger %q: persistence %+v (kill %v linger %v), problems %q", kill, linger, p, p.KillUserProcesses, p.Linger, r.Problems)
		}
	}
	yes, no := true, false
	check(&yes, &no, true)
	linger = "yes\n"
	check(&yes, &yes, false)
	kill, linger = "b false\n", "no\n"
	check(&no, &no, false)
	te.run.respond = func(string, []string) (string, error) { return "", errString("exit status 1") }
	check(nil, nil, false)

	te.vars["XDG_RUNTIME_DIR"] = te.l.RunDir + "-other"
	if p := te.doctor().Persistence; !p.RunDirSurvivesLogout {
		t.Errorf("RunDir %s reported inside %s", p.RunDir, te.vars["XDG_RUNTIME_DIR"])
	}
	te.vars["XDG_RUNTIME_DIR"] = filepath.Dir(te.l.RunDir)
	if p := te.doctor().Persistence; p.RunDirSurvivesLogout {
		t.Errorf("RunDir %s not reported inside %s", p.RunDir, te.vars["XDG_RUNTIME_DIR"])
	}

	mac := newTestEnv(t, "darwin")
	if p := mac.doctor().Persistence; !p.RunDirSurvivesLogout || p.KillUserProcesses != nil || p.Linger != nil || mac.run.ran("busctl") || mac.run.ran("loginctl") {
		t.Errorf("darwin: %+v, calls %v", p, mac.run.calls)
	}
}
