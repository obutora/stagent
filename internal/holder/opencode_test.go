package holder

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOpenCode writes an executable named opencode whose --version prints
// out and exits with code.
func fakeOpenCode(t *testing.T, out string, code int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%s'\nexit %d\n", out, code)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// OpenCode V2's terminal UI runs in a server of its own with --standalone:
// its plugin then runs in the session's environment, so its hooks are the
// session's (the background service it uses otherwise keeps the
// environment of whoever started it). The holder adds the flag to an
// OpenCode V2 terminal UI; OpenCode V1 (no such flag), its commands and a
// UI connecting to a server of the user's are left as they are.
func TestOpenCodeCommandAddsStandalone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake opencode is a shell script")
	}
	v2 := fakeOpenCode(t, `opencode v2.3.0\n`, 0)
	for _, c := range []struct {
		name string
		argv []string
		add  bool // want --standalone right after the program
	}{
		{"terminal UI", []string{v2}, true},
		{"resume", []string{v2, "--session", "ses_1"}, true},
		{"in a directory, with a prompt", []string{v2, "/w/proj", "--prompt", "fix it"}, true},
		{"later major", []string{fakeOpenCode(t, `opencode v3.0.1\n`, 0)}, true},
		{"V1", []string{fakeOpenCode(t, `1.19.5\n`, 0)}, false},
		{"version fails", []string{fakeOpenCode(t, `opencode v2.3.0\n`, 1)}, false},
		{"unreadable version", []string{fakeOpenCode(t, `dev\n`, 0)}, false},
		{"already given", []string{v2, "--standalone", "-c"}, false},
		{"a server of the user's", []string{v2, "--server", "http://127.0.0.1:4096"}, false},
		{"a server of the user's, =", []string{v2, "--server=http://127.0.0.1:4096"}, false},
		{"a command", []string{v2, "run", "fix it"}, false},
		{"not opencode", []string{"/bin/sh", "-c", "true"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := c.argv
			if c.add {
				want = append([]string{c.argv[0], "--standalone"}, c.argv[1:]...)
			}
			if got := openCodeCommand(c.argv, t.TempDir(), os.Environ()); !slices.Equal(got, want) {
				t.Fatalf("openCodeCommand(%q) = %q, want %q", c.argv, got, want)
			}
		})
	}
	// A task's prepared session: the flag goes after the opencode that
	// `stagent task run` ends in.
	task := []string{"/opt/stagent", "task", "run", "0123456789abcdef", "--", v2, "--prompt", "Issue #3"}
	want := []string{"/opt/stagent", "task", "run", "0123456789abcdef", "--", v2, "--standalone", "--prompt", "Issue #3"}
	if got := openCodeCommand(task, t.TempDir(), os.Environ()); !slices.Equal(got, want) {
		t.Fatalf("openCodeCommand(%q) = %q, want %q", task, got, want)
	}
}

// The version of an opencode binary is asked once: launching the same
// file again does not run `opencode --version` (up to 5 s before the UI
// starts), while a binary replaced or touched in place, or one that gave
// no version, is asked again.
func TestOpenCodeVersionCache(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and PATH lookup of a bare file")
	}
	var probes atomic.Int32
	answer, answered := true, true
	c := &openCodeVersionCache{probe: func(string, string, []string) (bool, bool) {
		probes.Add(1)
		return answer, answered
	}}
	ask := func(bin, dir string, want bool, wantProbes int32) {
		t.Helper()
		if got := c.v2(bin, dir, nil); got != want {
			t.Fatalf("v2(%q) = %v, want %v", bin, got, want)
		}
		if got := probes.Load(); got != wantProbes {
			t.Fatalf("after v2(%q): %d probes, want %d", bin, got, wantProbes)
		}
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ask(bin, "", true, 1)
	ask(bin, "", true, 1)
	// The same file by a link, by a path relative to the launch's
	// directory and by its name on PATH.
	link := filepath.Join(t.TempDir(), "opencode")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	ask(link, "", true, 1)
	ask("./opencode", dir, true, 1)
	t.Setenv("PATH", dir)
	ask("opencode", "", true, 1)

	// Touched in place: asked again, and the new answer kept.
	answer = false
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	ask(bin, "", false, 2)
	ask(bin, "", false, 2)
	// Replaced by a file of another size with the same time.
	answer = true
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	ask(bin, "", true, 3)

	// No version: not kept.
	other := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	answer, answered = false, false
	ask(other, "", false, 4)
	ask(other, "", false, 5)
	// A program that is not there is asked each time (and fails to run).
	ask(filepath.Join(dir, "missing"), "", false, 6)
	ask(filepath.Join(dir, "missing"), "", false, 7)

	// Launches at once share the cache safely (go test -race).
	answer, answered = true, true
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.v2(other, "", nil)
		}()
	}
	wg.Wait()
	ask(other, "", true, probes.Load())
}
