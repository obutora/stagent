// Package stubcmd stands in for command-line programs (gh, winget, git) in
// tests, alike on every OS: Install copies the test binary under the
// program's name next to a file of rules, and Main, run first by the test
// binary's TestMain, makes that copy answer by them.
package stubcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
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
	// Await, when set, is a file the stub waits for before answering: one
	// never made is a program that hangs until it is killed.
	Await string
	// Exec, when set, is a program the stub then runs with its own
	// arguments, standard streams and environment, exiting as it does
	// (Stdout, Stderr and Exit unused): the real program behind a stub
	// that only stands in for some calls.
	Exec string
	// Log, when set, is a file the stub appends the call to (its arguments
	// and environment, see Calls) before answering.
	Log string
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
		if r.Log != "" {
			if err := logCall(r.Log); err != nil {
				fmt.Fprintf(os.Stderr, "stubcmd: %v\n", err)
				return 2
			}
		}
		for r.Await != "" {
			if _, err := os.Stat(r.Await); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if r.Exec != "" {
			return run(r.Exec, os.Args[1:])
		}
		os.Stdout.WriteString(r.Stdout)
		os.Stderr.WriteString(r.Stderr)
		return r.Exit
	}
	fmt.Fprintf(os.Stderr, "unexpected %s %s\n", filepath.Base(os.Args[0]), args)
	return 1
}

// Call is one call a stub logged (Rule.Log).
type Call struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

// logCall appends this process's call to the file path, one JSON object a
// line.
func logCall(path string) error {
	b, err := json.Marshal(Call{Args: os.Args[1:], Env: os.Environ()})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Calls are the calls logged to the file path (Rule.Log), oldest first;
// none when there is no such file.
func Calls(t testing.TB, path string) []Call {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []Call
	for line := range strings.Lines(string(b)) {
		var c Call
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, c)
	}
	return calls
}

// run runs program with args on this process's standard streams and
// returns its exit code.
func run(program string, args []string) int {
	cmd := exec.Command(program, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "stubcmd: %v\n", err)
		return 1
	}
	return 0
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
