// Package statusline is `stagent statusline claude`, the statusLine command
// `stagent integrate --statusline` gives Claude Code. It runs the user's
// own statusLine (recorded in the manifest) with the same input and
// environment and returns its output and exit code; without one it prints
// nothing, so Claude Code shows what it showed before. Alongside, it sends
// the input's rate_limits (Claude's limit windows) to the daemon, at most
// every 15 s per session. The report never holds the user's statusLine
// back: an absent daemon costs at most the 100 ms connect, and nothing
// starts a daemon from here.
package statusline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/install"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// dialTimeout bounds connecting to (and writing to) the daemon.
	dialTimeout = 100 * time.Millisecond
	// reportEvery: a session sends its limit windows at most this often.
	// Claude Code runs the statusLine after every response (300 ms
	// debounce) and every refreshInterval.
	reportEvery = 15 * time.Second
	// reportWait bounds how long, from the start, the relay waits for the
	// report once the user's statusLine has ended.
	reportWait = 150 * time.Millisecond
	// markLife: the mark of a session silent for this long is deleted.
	markLife = 24 * time.Hour
)

// procIO is the process environment, replaceable in tests.
type procIO struct {
	stdin          io.Reader
	stdinTTY       bool
	stdout, stderr io.Writer
	getenv         func(string) string
	goos           string
	lookPath       func(string) (string, error)
}

// Main runs the relay and returns the exit code of the user's statusLine.
func Main(args []string) int {
	return run(args, procIO{
		stdin:    os.Stdin,
		stdinTTY: term.IsTerminal(int(os.Stdin.Fd())),
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		getenv:   os.Getenv,
		goos:     runtime.GOOS,
		lookPath: exec.LookPath,
	})
}

func run(args []string, pio procIO) int {
	if len(args) != 1 || args[0] != wire.HarnessClaude {
		fmt.Fprintln(pio.stderr, "usage: stagent statusline claude")
		return 2
	}
	start := time.Now()
	var input []byte
	if !pio.stdinTTY && pio.stdin != nil {
		input, _ = io.ReadAll(pio.stdin)
	}
	l, err := paths.Resolve()
	if err != nil {
		return 0
	}
	configDir, _ := harness.ClaudeConfigDir(l.Home, pio.getenv)

	reported := make(chan struct{})
	go func() {
		defer close(reported)
		defer func() { recover() }()
		report(l, configDir, input, time.Now())
	}()
	code := 0
	if command := install.StatusLineCommand(l.Manifest, filepath.Join(configDir, "settings.json")); command != "" {
		code = runOriginal(command, input, pio)
	}
	select {
	case <-reported:
	case <-time.After(time.Until(start.Add(reportWait))):
	}
	return code
}

// runOriginal runs the user's statusLine command the way Claude Code does
// (sh on unix; Git Bash, else PowerShell, on Windows) and returns its exit
// code. Its output goes straight to ours. A signal that ends the relay
// (Claude Code cancelling an outdated run) ends it too.
func runOriginal(command string, input []byte, pio procIO) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	name, args := shell(command, pio)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	if pio.stdinTTY {
		cmd.Stdin = pio.stdin
	}
	cmd.Stdout, cmd.Stderr = pio.stdout, pio.stderr
	err := cmd.Run()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
		return 1
	}
	if err != nil {
		fmt.Fprintf(pio.stderr, "stagent statusline: %v\n", err)
		return 1
	}
	return 0
}

// shell is the program and arguments that run command.
func shell(command string, pio procIO) (string, []string) {
	if pio.goos != "windows" {
		return "/bin/sh", []string{"-c", command}
	}
	bash := pio.getenv("CLAUDE_CODE_GIT_BASH_PATH")
	if bash == "" {
		if git, err := pio.lookPath("git"); err == nil {
			bash = task.GitBash(git)
		}
	}
	if bash != "" {
		return bash, []string{"-c", command}
	}
	return "powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", command}
}

// report sends the limit windows of the statusLine input to the daemon,
// unless the input has none or its session sent less than reportEvery
// ago.
func report(l *paths.Layout, configDir string, input []byte, now time.Time) {
	var in struct {
		SessionID      string          `json:"session_id"`
		TranscriptPath string          `json:"transcript_path"`
		RateLimits     json.RawMessage `json:"rate_limits"`
	}
	if json.Unmarshal(input, &in) != nil || len(in.RateLimits) == 0 || string(in.RateLimits) == "null" {
		return
	}
	if !takeTurn(l.StatusLineMarks, in.SessionID, now) {
		return
	}
	conn, err := daemonclient.Dial(l, dialTimeout)
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(dialTimeout))
	wire.NewCodec(conn, conn).Notify(wire.MethodUsageReport, wire.UsageReportParams{
		ConfigDir:      configDir,
		SessionID:      in.SessionID,
		TranscriptPath: in.TranscriptPath,
		RateLimits:     in.RateLimits,
	})
}

// takeTurn reports whether session may send now: its mark in dir is at
// least reportEvery old, or missing. It then marks the session as having
// sent. A new mark sweeps the marks of sessions silent for markLife.
func takeTurn(dir, session string, now time.Time) bool {
	mark := filepath.Join(dir, markName(session))
	if st, err := os.Stat(mark); err == nil {
		if d := now.Sub(st.ModTime()); d >= 0 && d < reportEvery {
			return false
		}
		os.Chtimes(mark, now, now)
		return true
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return true
	}
	if f, err := os.OpenFile(mark, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		os.Chtimes(mark, now, now)
	}
	sweep(dir, now)
	return true
}

var plainName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// markName is the mark file of a session: its id when that is a plain
// name (Claude's are UUIDs), otherwise a hash of it.
func markName(session string) string {
	if plainName.MatchString(session) {
		return session
	}
	sum := sha256.Sum256([]byte(session))
	return "h" + hex.EncodeToString(sum[:16])
}

// sweep deletes the marks older than markLife.
func sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && now.Sub(fi.ModTime()) > markLife {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
