package daemon

import (
	"syscall"

	"github.com/obutora/stagent/internal/ptable"
)

// signalProgram terminates the program: Windows has no signals, and
// TerminateProcess(…, 1) is the holder's own last resort for every one.
func signalProgram(p *ptable.Proc, _ string) { killProcess(p.PID) }

func killProcess(pid int) {
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return
	}
	defer syscall.CloseHandle(h)
	syscall.TerminateProcess(h, 1)
}

// signalExitCode is the exit code of a program TerminateProcess ended.
func signalExitCode(string) int { return 1 }
