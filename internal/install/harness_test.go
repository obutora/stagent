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
