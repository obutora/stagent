// Package stubcmd stands in for command-line programs (gh, winget) in
// tests, alike on every OS: Install copies the test binary under the
// program's name next to a file of rules, and Main, run first by the test
// binary's TestMain, makes that copy answer by them.
package stubcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Rule is one answer of a stub.
type Rule struct {
	// Args matches the arguments joined by spaces, as sh's case "$*" does:
	// '*' matches any run of characters, anything else itself.
	Args   string
	Stdout string
	Stderr string
	Exit   int
	// Await, when set, is a file the stub waits for before answering.
	Await string
}

// Main runs tests by m, unless this process is a stub: then it answers by
// its rules (the first matching; none: "unexpected" and exit 1) and exits.
func Main(m *testing.M) {
	exe, err := os.Executable()
	if err != nil {
		os.Exit(m.Run())
	}
	b, err := os.ReadFile(rulesPath(exe))
	if err != nil {
		os.Exit(m.Run())
	}
	var rules []Rule
	if err := json.Unmarshal(b, &rules); err != nil {
		fmt.Fprintf(os.Stderr, "stubcmd: %v\n", err)
		os.Exit(2)
	}
	os.Exit(answer(rules, strings.Join(os.Args[1:], " ")))
}

func answer(rules []Rule, args string) int {
	for _, r := range rules {
		if !match(r.Args, args) {
			continue
		}
		for r.Await != "" {
			if _, err := os.Stat(r.Await); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		os.Stdout.WriteString(r.Stdout)
		os.Stderr.WriteString(r.Stderr)
		return r.Exit
	}
	fmt.Fprintf(os.Stderr, "unexpected %s %s\n", filepath.Base(os.Args[0]), args)
	return 1
}

// match reports whether s matches pattern, whose '*' matches any run of
// characters.
func match(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(s, p)
		if i < 0 {
			return false
		}
		s = s[i+len(p):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// rulesPath is where the stub at exe finds its rules.
func rulesPath(exe string) string {
	return strings.TrimSuffix(exe, ".exe") + ".stub.json"
}

// Install puts the stub program name (name.exe on Windows) answering by
// rules into dir and returns its path. The test binary must run Main.
func Install(t testing.TB, dir, name string, rules ...Rule) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	if rules == nil {
		rules = []Rule{}
	}
	b, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rulesPath(p), b, 0o644); err != nil {
		t.Fatal(err)
	}
	// A copy, not a link: Windows cannot delete a link to the running test
	// binary, and Linux resolves symbolic links in os.Executable.
	if err := copyFile(exe, p); err != nil {
		t.Fatal(err)
	}
	return p
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
