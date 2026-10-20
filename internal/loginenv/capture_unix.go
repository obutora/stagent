//go:build !windows

package loginenv

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Capture returns the environment of the user's login shell, exe being
// the stagent binary that prints it (`stagent __env`), taking within at
// most in all.
//
// An SSH exec channel runs the command through a non-login, non-interactive
// shell, and the daemon runs wherever it was started from, so PATH entries
// set in ~/.bash_profile, ~/.zprofile or ~/.zshrc (bun, nvm, Homebrew, …)
// are missing and programs installed there fail with "executable file not
// found in $PATH". The shell is run as a login, interactive shell first
// (zsh users keep PATH in .zshrc), then as a login shell only, without a
// controlling terminal, each try for TryTimeout at most and what is left
// of within.
func Capture(exe string, within time.Duration) ([]string, error) {
	deadline := time.Now().Add(within)
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if strings.ContainsRune(exe, '\'') {
		return nil, errors.New("stagent path contains a quote")
	}
	script := "exec '" + exe + "' __env"
	lastErr := errors.New("no time to run the login shell")
	for _, flags := range [][]string{{"-l", "-i", "-c"}, {"-l", "-c"}} {
		end := time.Now().Add(TryTimeout)
		if deadline.Before(end) {
			end = deadline
		}
		if !time.Now().Before(end) {
			break
		}
		env, err := runDump(end, shell, append(flags, script))
		if err == nil {
			return env, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// runDump runs the login shell until end at most: it is killed a little
// before, the rest left for a child of it that holds its output open.
func runDump(end time.Time, shell string, args []string) ([]string, error) {
	wait := min(time.Second, time.Until(end)/5)
	ctx, cancel := context.WithDeadline(context.Background(), end.Add(-wait))
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	// A new session has no controlling terminal: an interactive shell
	// cannot grab the tty, and rc files that try to (tmux attach) fail fast.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = wait
	out, err := cmd.Output()
	if env, ok := parseDump(out); ok {
		return env, nil
	}
	if err == nil {
		err = errors.New("no environment printed")
	}
	return nil, err
}
