//go:build !windows

package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/obutora/stagent/internal/paths"
)

// loginEnvTimeout bounds one capture attempt; rc files that hang (waiting
// for input, network) must not block session.spawn.
const loginEnvTimeout = 5 * time.Second

// captureLoginEnv returns the environment of the user's login shell.
//
// An SSH exec channel runs the command through a non-login, non-interactive
// shell, so PATH entries set in ~/.bash_profile, ~/.zprofile or ~/.zshrc
// (bun, nvm, Homebrew, …) are missing and agents installed there fail with
// "executable file not found in $PATH". The shell is run as a login,
// interactive shell first (zsh users keep PATH in .zshrc), then as a login
// shell only, without a controlling terminal and with a timeout.
func captureLoginEnv(exe string) ([]string, error) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	if strings.ContainsRune(exe, '\'') {
		return nil, errors.New("stagent path contains a quote")
	}
	script := "exec '" + exe + "' __env"
	var lastErr error
	for _, flags := range [][]string{{"-l", "-i", "-c"}, {"-l", "-c"}} {
		env, err := runEnvDump(shell, append(flags, script))
		if err == nil {
			return env, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func runEnvDump(shell string, args []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loginEnvTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	// A new session has no controlling terminal: an interactive shell
	// cannot grab the tty, and rc files that try to (tmux attach) fail fast.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if env, ok := parseEnvDump(out); ok {
		return env, nil
	}
	if err == nil {
		err = errors.New("no environment printed")
	}
	return nil, err
}

// shellCommand is what a `shell: true` session runs: the user's $SHELL as
// their login session sets it (/bin/sh when unset), as a login shell.
func shellCommand(env []string) []string {
	sh := envGet(env, "SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	return []string{sh, "-l"}
}

// terminalCommand is what a task opened as a terminal runs (task.create
// without a command): the login shell, as for `shell: true`.
func terminalCommand(env []string) []string { return shellCommand(env) }

func toolDirs(home string, getenv func(string) string) []string {
	return paths.ToolDirs(runtime.GOOS, home, getenv)
}
