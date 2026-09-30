//go:build !windows

// Package proc starts stagent processes that must outlive their caller: the
// daemon (auto-started by whoever needs it first) and detached holders.
package proc

import (
	"os"
	"os/exec"
	"syscall"
)

// SpawnDetached starts exe with args in a new session so it survives the
// caller, its terminal and the SSH connection. stdin/stdout go to /dev/null,
// stderr is appended to logPath (if non-empty). env nil inherits.
func SpawnDetached(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Reap it if it exits while we are still alive; otherwise init adopts it.
	go cmd.Wait()
	return pid, nil
}

// Alive reports whether pid is a running process.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
