package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// integrateRaw runs integrate on a fresh command state without failing on
// change errors.
func (te *testEnv) integrateRaw(t *testing.T, o integrateOpts) *IntegrateResult {
	t.Helper()
	te.reload()
	r, err := te.env.integrate(o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func changeByID(r *IntegrateResult, id string) *Change {
	for _, c := range r.Changes {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// readOnlyDir makes dir unwritable for the test's duration.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes to read-only directories")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
}

func TestIntegrateResult(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		te := newTestEnv(t, "linux")
		te.vars["SHELL"] = "/bin/bash"
		r := te.integrateRaw(t, integrateOpts{shellWrapper: true})
		if r.Result != resultOK || r.LoginShell != "bash" || len(r.Changes) != 1 {
			t.Fatalf("plan: result %s, login_shell %q, changes %v", r.Result, r.LoginShell, changeIDs(r.Changes))
		}
		if r := te.integrateRaw(t, integrateOpts{apply: true, shellWrapper: true}); r.Result != resultOK || !r.Applied {
			t.Fatalf("apply: %+v", r)
		}
		// Everything already in place is ok too.
		if r := te.integrateRaw(t, integrateOpts{apply: true, shellWrapper: true}); r.Result != resultOK || len(r.Changes) != 0 {
			t.Fatalf("again: result %s, changes %v", r.Result, changeIDs(r.Changes))
		}
	})

	t.Run("nothing_to_do", func(t *testing.T) {
		te := newTestEnv(t, "linux")
		te.vars["SHELL"] = "/usr/bin/tcsh"
		r := te.integrateRaw(t, integrateOpts{shellWrapper: true, harness: []string{hOmp}})
		if r.Result != resultNothingToDo || r.LoginShell != "tcsh" {
			t.Fatalf("result %s, login_shell %q", r.Result, r.LoginShell)
		}
		// Without --shell-wrapper the same host is ok.
		if r := te.integrateRaw(t, integrateOpts{harness: []string{hOmp}}); r.Result != resultOK {
			t.Fatalf("harness only: result %s", r.Result)
		}
	})

	t.Run("partial and failed", func(t *testing.T) {
		te := newTestEnv(t, "linux")
		te.vars["SHELL"] = "/bin/zsh"
		zdot := filepath.Join(t.TempDir(), "zdot")
		te.vars["ZDOTDIR"] = zdot
		writeFile(t, filepath.Join(zdot, ".zshrc"), "# zsh\n")
		readOnlyDir(t, zdot)

		// Only the unwritable .zshrc: planned, then failed.
		r := te.integrateRaw(t, integrateOpts{shellWrapper: true})
		if r.Result != resultOK {
			t.Fatalf("plan: result %s", r.Result)
		}
		r = te.integrateRaw(t, integrateOpts{apply: true, shellWrapper: true})
		c := changeByID(r, "shell-zsh")
		if r.Result != resultFailed || r.Applied || c == nil || c.Action != "modify" || c.ErrorCode != codeNotWritable || c.Error == "" {
			t.Fatalf("apply: result %s, change %+v", r.Result, c)
		}

		// With a writable .bashrc next to it: partial, and the error code
		// is in the JSON the app reads.
		writeFile(t, te.home(".bashrc"), "# bash\n")
		r = te.integrateRaw(t, integrateOpts{apply: true, shellWrapper: true})
		if r.Result != resultPartial || changeByID(r, "shell-bash").Error != "" {
			t.Fatalf("apply: result %s, changes %+v", r.Result, r.Changes)
		}
		var doc struct {
			Result  string `json:"result"`
			Changes []struct {
				ID        string `json:"id"`
				ErrorCode string `json:"error_code"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(asciiJSON(r, false), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Result != resultPartial || len(doc.Changes) != 2 || doc.Changes[1].ID != "shell-zsh" || doc.Changes[1].ErrorCode != codeNotWritable || doc.Changes[0].ErrorCode != "" {
			t.Fatalf("JSON = %s", asciiJSON(r, false))
		}
	})

	t.Run("planning failure is a skip change", func(t *testing.T) {
		te := newTestEnv(t, "linux")
		te.vars["SHELL"] = "/usr/bin/fish"
		writeFile(t, te.home(".bashrc"), "# bash\n")
		fish := te.home(".config", "fish", "conf.d", "ssh-term-stagent.fish")
		writeFile(t, fish, "# someone else's\n")
		for _, apply := range []bool{false, true} {
			r := te.integrateRaw(t, integrateOpts{apply: apply, shellWrapper: true})
			c := changeByID(r, "shell-fish")
			if r.Result != resultPartial || c == nil || c.Action != "skip" || c.Target != fish || c.ErrorCode != codeUnmanagedFile || !strings.Contains(c.Error, "not managed by stagent") {
				t.Fatalf("apply=%v: result %s, change %+v", apply, r.Result, c)
			}
		}
		if got := readFile(t, fish); got != "# someone else's\n" {
			t.Fatalf("foreign fish file changed: %q", got)
		}
	})
}

// The LaunchAgent goes into the user domain as a Background-session agent
// of a standard process type, never into the GUI domain, which ends at
// logout; an older load in the GUI domain is booted out first.
func TestLaunchAgentUserDomain(t *testing.T) {
	te := newTestEnv(t, "darwin")
	te.integrate(t, integrateOpts{apply: true, service: true})
	// The marker comment holds "--", which Go's XML parser refuses (and
	// CFPropertyList skips).
	text := strings.Replace(readFile(t, te.launchdPlistPath()), "<!-- "+serviceMarker+" -->", "", 1)
	root, err := parsePlist([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	if s, p := root.get("LimitLoadToSessionType"), root.get("ProcessType"); s == nil || s.text != "Background" || p == nil || p.text != "Standard" {
		t.Errorf("LimitLoadToSessionType %+v, ProcessType %+v", s, p)
	}
	var bootstraps []string
	for _, c := range te.run.calls {
		if strings.HasPrefix(c, "launchctl bootstrap ") {
			bootstraps = append(bootstraps, c)
		}
	}
	if len(bootstraps) != 1 || bootstraps[0] != "launchctl bootstrap user/1000 "+te.launchdPlistPath() || !te.run.ran("launchctl bootout gui/1000/"+launchdLabel) {
		t.Errorf("calls %q", te.run.calls)
	}
}
