package install

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// TestMain checks first that every harness of the registry has its
// integration (plan, targets, doctor report): without one most
// tests of the package would panic before a test could name the harness.
func TestMain(m *testing.M) {
	missing := harness.Missing(integrations)
	for _, id := range missing {
		fmt.Fprintf(os.Stderr, "harness %q has no entry in integrations (internal/install/%s.go)\n", id, id)
	}
	if len(missing) > 0 {
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// integrations has no entry for anything but a harness of the registry.
func TestIntegrationsCoverRegistry(t *testing.T) {
	for id := range integrations {
		if !harness.Known(id) {
			t.Errorf("integrations has %q, which is not a harness of the registry", id)
		}
	}
}

// The batch functions of the wrapper scripts, generated from the registry,
// judge every harness's command line as harness.BatchArgs does. Each
// command is run with the batch arguments of every harness, so a rule of
// one harness that leaks into another fails too.
func TestWrapperBatchMatchesRegistry(t *testing.T) {
	argvs := [][]string{{"hi"}}
	for _, h := range harness.All {
		b := h.Batch
		for _, s := range b.Subcommands {
			argvs = append(argvs, []string{s, "x"}, []string{"x", s})
		}
		for _, f := range b.Flags {
			argvs = append(argvs, []string{f}, []string{"x", f})
		}
		for _, p := range b.Prefixes {
			argvs = append(argvs, []string{p + "hi"}, []string{"x", p}, []string{"x" + p})
		}
		if b.Mode != "" {
			argvs = append(argvs,
				[]string{b.Mode, "json"}, []string{b.Mode, b.ModeInteractive, "x"}, []string{b.Mode},
				[]string{b.Mode + "=json"}, []string{b.Mode + "=" + b.ModeInteractive, "x"})
		}
	}
	type call struct {
		argv []string
		want string
	}
	var calls []call
	for _, h := range harness.All {
		for _, args := range argvs {
			argv := append([]string{h.Command}, args...)
			want := "stagent run --handoff=auto -- " + strings.Join(argv, " ")
			if harness.BatchArgs(h.ID, argv) {
				want = "real " + strings.Join(argv, " ")
			}
			calls = append(calls, call{argv, want})
		}
	}
	script := func(quote func(string) string) string {
		lines := make([]string, len(calls))
		for i, c := range calls {
			lines[i] = c.argv[0] + " " + quoteAll(c.argv[1:], quote, " ")
		}
		return strings.Join(lines, "\n")
	}
	var want []string
	for _, c := range calls {
		want = append(want, c.want)
	}
	for _, sh := range []struct {
		name string
		rc   func(stagent, dir string) string
		argv func(bin, rc string) []string
	}{
		{"bash", func(s, _ string) string { return posixBlock(s) }, func(bin, rc string) []string {
			return []string{bin, "--norc", "--noprofile", "-i", "-c", ". " + shellQuote(rc) + "\n" + script(shellQuote)}
		}},
		{"zsh", func(s, _ string) string { return posixBlock(s) }, func(bin, rc string) []string {
			return []string{bin, "-f", "-i", "-c", ". " + shellQuote(rc) + "\n" + script(shellQuote)}
		}},
		{"fish", func(s, _ string) string { return fishFile(s) }, func(bin, rc string) []string {
			return []string{bin, "--no-config", "-i", "-c", "source " + fishQuote(rc) + "\n" + script(fishQuote)}
		}},
		{"pwsh", func(s, dir string) string { return powershellBlock(psBinExpr(s, dir)) }, func(bin, rc string) []string {
			return []string{bin, "-NoLogo", "-NoProfile", "-Command", ". " + psQuote(rc) + "\n" + script(psQuote)}
		}},
	} {
		t.Run(sh.name, func(t *testing.T) {
			bin, err := exec.LookPath(sh.name)
			if err != nil {
				t.Skip(sh.name + " not available")
			}
			dir, stagent := fakePrograms(t)
			rc := filepath.Join(dir, "rc")
			if sh.name == "pwsh" {
				rc += ".ps1"
			}
			writeFile(t, rc, sh.rc(stagent, dir))
			env := []string{"PATH=" + dir + ":/usr/bin:/bin", "HOME=" + dir, "USERPROFILE=" + dir, "TERM=dumb"}
			if got := runOnTerminal(t, sh.argv(bin, rc), env); !slices.Equal(got, want) {
				t.Errorf("printed\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// A harness found on the host gets its restart note when its integration
// changes, not on an integrate that leaves it as it is.
func TestRestartNoteOnlyWhenChanged(t *testing.T) {
	for _, c := range []struct{ id, note string }{
		{wire.HarnessOmp, noteOmpRestart},
		{wire.HarnessPi, notePiRestart},
		{wire.HarnessCopilot, noteCopilotRestart},
	} {
		te := newTestEnv(t, "linux")
		h, _ := harness.Lookup(c.id)
		te.bins[h.Command] = "/usr/bin/" + h.Command
		first := te.integrate(t, integrateOpts{apply: true, harness: []string{c.id}})
		again := te.integrate(t, integrateOpts{apply: true, harness: []string{c.id}})
		if n := len(notesWith(first.Notes, c.note)); n != 1 {
			t.Errorf("%s: %d %s notes on install", c.id, n, c.note)
		}
		if n := len(notesWith(again.Notes, c.note)); n != 0 || len(again.Changes) != 0 {
			t.Errorf("%s: %d %s notes on an unchanged integrate (changes %+v)", c.id, n, c.note, again.Changes)
		}
	}
}

// A harness whose directory comes from the environment is removed where it
// was integrated, even when the variable is unset (unhook) or points
// elsewhere (--remove) by then: the file, the directories created for it
// and the manifest record go.
func TestRemovalFollowsRecordedDir(t *testing.T) {
	for _, c := range []struct{ id, env, file string }{
		{wire.HarnessPi, "PI_CODING_AGENT_DIR", filepath.Join("extensions", "ssh-term-stagent.ts")},
		{wire.HarnessCopilot, "COPILOT_HOME", filepath.Join("hooks", "ssh-term-stagent.json")},
	} {
		for _, how := range []string{"unhook", "remove"} {
			te := newTestEnv(t, "linux")
			dir := filepath.Join(t.TempDir(), "custom")
			te.vars[c.env] = dir
			te.integrate(t, integrateOpts{apply: true, harness: []string{c.id}})
			if !exists(filepath.Join(dir, c.file)) {
				t.Fatalf("%s: nothing installed under %s", c.id, dir)
			}
			if how == "unhook" {
				delete(te.vars, c.env)
				te.reload()
				te.uninstall("unhook", false, false)
			} else {
				te.vars[c.env] = filepath.Join(t.TempDir(), "other")
				te.integrate(t, integrateOpts{apply: true, remove: []string{c.id}})
			}
			te.reload()
			if exists(dir) {
				t.Errorf("%s %s: %s left behind", c.id, how, dir)
			}
			for _, rec := range te.m.Configs {
				if rec.ID == c.id {
					t.Errorf("%s %s: manifest still records %s", c.id, how, rec.Path)
				}
			}
		}
	}
}
