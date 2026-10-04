package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/pty"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// startWindowsBridge runs `stagent bridge` for an isolated home with SHELL
// set to shell (unset when empty), as sshd sets it to its DefaultShell.
func startWindowsBridge(t *testing.T, bin, shell string) *app {
	t.Helper()
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv(daemonclient.EnvExe, bin)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "SHELL") {
			env = append(env, kv)
		}
	}
	if shell != "" {
		env = append(env, "SHELL="+shell)
	}
	cmd := exec.Command(bin, "bridge")
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Wait()
		shutdownDaemon(l)
		if t.Failed() {
			t.Logf("bridge stderr:\n%s", stderr.String())
			logs, _ := filepath.Glob(filepath.Join(l.LogDir, "*.log"))
			for _, f := range logs {
				b, _ := os.ReadFile(f)
				t.Logf("%s:\n%s", filepath.Base(f), b)
			}
		}
	})
	a := newApp(stdout, stdin)
	var hello wire.HelloResult
	a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "e2e"}, &hello)
	if !slices.Contains(hello.Capabilities, wire.CapPersist) || !slices.Contains(hello.Capabilities, wire.CapPersistShell) {
		t.Fatalf("hello capabilities %v lack %q or %q", hello.Capabilities, wire.CapPersist, wire.CapPersistShell)
	}
	a.call(t, wire.MethodWatch, wire.WatchParams{}, nil)
	return a
}

// A kept shell on Windows runs the account's SSH shell — SHELL as sshd
// sets it, %ComSpec% without it — with no "-l" (PowerShell 5.1 would exit
// on it). The app's "end" (hangup) ends it within 4 s, recorded as a
// hangup rather than an abnormal exit.
func TestWindowsShellSessionAndHangup(t *testing.T) {
	bin := compileStagent(t)
	sys := filepath.Join(os.Getenv("SystemRoot"), "System32")
	comspec := os.Getenv("ComSpec")
	type shellCase struct {
		name, shell, want, input string
	}
	cases := []shellCase{
		// sshd lowercases the DefaultShell path it puts in SHELL.
		{"cmd", strings.ToLower(filepath.Join(sys, "cmd.exe")), "", "set /a 6*7"},
		{"unset", "", comspec, "set /a 6*7"},
		{"powershell", filepath.Join(sys, "WindowsPowerShell", "v1.0", "powershell.exe"), "", "6*7"},
	}
	if pwsh, err := exec.LookPath("pwsh"); err == nil {
		cases = append(cases, shellCase{"pwsh", pwsh, "", "6*7"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.want == "" {
				tc.want = tc.shell
			}
			a := startWindowsBridge(t, bin, tc.shell)
			var sp wire.SpawnResult
			a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cols: 100, Rows: 30}, &sp)
			id := sp.Session.ID
			if !slices.Equal(sp.Session.Command, []string{tc.want}) || sp.Session.Mode != wire.ModeDetached {
				t.Fatalf("shell session = %+v, want command [%s]", sp.Session, tc.want)
			}
			// 6*7 shows up as 42 only once the shell evaluated it.
			a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: tc.input, Submit: true}, nil)
			waitScrollback(t, a, id, "42")

			start := time.Now()
			a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalHangup}, nil)
			m := a.waitFor(t, "session_ended after hangup", func(m *wire.Msg) bool {
				_, ok := eventOf(m, wire.EventSessionEnded, id)
				return ok
			})
			if elapsed := time.Since(start); elapsed > 4*time.Second {
				t.Fatalf("hangup took %v to end the shell, want under 4 s", elapsed)
			}
			ev, _ := eventOf(m, wire.EventSessionEnded, id)
			var ended wire.SessionEndedData
			json.Unmarshal(ev.Data, &ended)
			if !ended.HungUp {
				t.Fatalf("session_ended %s, want hung_up", ev.Data)
			}
		})
	}
}

// A PC console session (the shell wrappers' `stagent run --handoff=auto`)
// ends with its console window: Windows has no handoff. The close
// (CTRL_CLOSE_EVENT) counts as a hangup, so the exit is no abnormal one
// and is not pushed, whatever its code (#269). An agent that fails on its
// own in such a session still exits abnormally.
func TestWindowsConsoleCloseIsAHangup(t *testing.T) {
	bin := compileStagent(t)
	a := startWindowsBridge(t, bin, "")

	const closed = "00000000000000f2"
	console := startInConsole(t, bin, closed, "cmd.exe", "/d", "/k", "prompt READY$G")
	a.waitFor(t, "session_started", func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionStarted, closed)
		return ok
	})
	waitScrollback(t, a, closed, "READY>")
	console.Close()
	if ended, level := sessionExit(t, a, closed); !ended.HungUp || level != wire.LevelInfo {
		t.Fatalf("after the console closed: session_ended %+v, notification level %s; want hung_up, info (not pushed)", ended, level)
	}

	const failed = "00000000000000f3"
	startInConsole(t, bin, failed, "cmd.exe", "/d", "/c", "exit 3")
	if ended, level := sessionExit(t, a, failed); ended.HungUp || ended.ExitCode != 3 || level != wire.LevelWarn {
		t.Fatalf("after exit 3: session_ended %+v, notification level %s; want exit 3, warn", ended, level)
	}
}

// startInConsole runs `stagent run --handoff=auto` as the shell wrappers do,
// in a pseudo console that stands in for a console window: closing it
// sends CTRL_CLOSE_EVENT to stagent run, as closing the window does.
func startInConsole(t *testing.T, bin, id string, argv ...string) pty.PTY {
	t.Helper()
	args := append([]string{bin, "run", "--handoff=auto", "--id", id, "--"}, argv...)
	console, err := pty.Start(args, t.TempDir(), os.Environ(), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		console.Signal(wire.SignalKill)
		console.Close()
	})
	go io.Copy(io.Discard, console) // ClosePseudoConsole may wait for its output to drain
	return console
}

// sessionExit waits for the end of session id and returns its
// session_ended and the level of its exited notification (warn is pushed).
func sessionExit(t *testing.T, a *app, id string) (wire.SessionEndedData, string) {
	t.Helper()
	m := a.waitFor(t, "session_ended of "+id, func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionEnded, id)
		return ok
	})
	ev, _ := eventOf(m, wire.EventSessionEnded, id)
	var ended wire.SessionEndedData
	json.Unmarshal(ev.Data, &ended)
	var n wire.NotificationData
	a.waitFor(t, "exited notification of "+id, func(m *wire.Msg) bool {
		ev, ok := eventOf(m, wire.EventNotification, id)
		return ok && json.Unmarshal(ev.Data, &n) == nil && n.Reason == "exited"
	})
	return ended, n.Level
}
