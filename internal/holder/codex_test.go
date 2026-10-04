package holder

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// fakeCodex writes an executable named codex whose --version prints out
// and exits with code.
func fakeCodex(t *testing.T, out string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%s'\nexit %d\n", out, code)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCodexCommandAddsNoDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	v160 := fakeCodex(t, `codex-cli 0.160.0\n`, 0)
	for _, c := range []struct {
		name string
		argv []string
		add  bool // want --no-daemon right after the program
	}{
		{"interactive", []string{v160}, true},
		{"resume", []string{v160, "resume", "abc"}, true},
		{"first version with the flag", []string{fakeCodex(t, `codex-cli 0.156.0\n`, 0), "-m", "x"}, true},
		{"pre-release", []string{fakeCodex(t, `codex-cli 0.161.0-alpha.2\n`, 0)}, true},
		{"older codex", []string{fakeCodex(t, `codex-cli 0.155.1\n`, 0)}, false},
		{"version fails", []string{fakeCodex(t, `codex-cli 0.160.0\n`, 1)}, false},
		{"unreadable version", []string{fakeCodex(t, `codex 1.0\n`, 0)}, false},
		{"already given", []string{v160, "--no-daemon", "resume"}, false},
		{"agents refuses it", []string{v160, "agents"}, false},
		{"remote refuses it", []string{v160, "--remote", "ws://127.0.0.1:1"}, false},
		{"remote= refuses it", []string{v160, "--remote=ws://127.0.0.1:1"}, false},
		{"not codex", []string{"/bin/sh", "-c", "true"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := c.argv
			if c.add {
				want = append([]string{c.argv[0], "--no-daemon"}, c.argv[1:]...)
			}
			if got := codexCommand(c.argv, t.TempDir(), os.Environ()); !slices.Equal(got, want) {
				t.Fatalf("codexCommand(%q) = %q, want %q", c.argv, got, want)
			}
		})
	}
}
