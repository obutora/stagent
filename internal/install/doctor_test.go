package install

import (
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
