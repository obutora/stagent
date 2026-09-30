//go:build !windows

package install

import (
	"os/exec"
	"syscall"
	"time"
)

func hideWindow(*exec.Cmd) {}

// killProcess sends SIGTERM, then SIGKILL if the process is still there
// after a grace period.
func killProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	for range 20 {
		time.Sleep(100 * time.Millisecond)
		if syscall.Kill(pid, 0) != nil {
			return nil
		}
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}
