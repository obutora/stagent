//go:build !windows

package daemon

import (
	"syscall"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

var endSignals = map[string]syscall.Signal{
	wire.SignalHangup:    syscall.SIGHUP,
	wire.SignalTerminate: syscall.SIGTERM,
	wire.SignalKill:      syscall.SIGKILL,
}

// signalProgram signals the program's process group (it leads a session of
// its own) and the foreground job of its terminal, as the holder does.
func signalProgram(p *ptable.Proc, sig string) {
	s := endSignals[sig]
	if p.TTY != 0 && p.Tpgid > 0 && p.Tpgid != p.PID {
		syscall.Kill(-p.Tpgid, s)
	}
	if p.Pgrp == p.PID {
		syscall.Kill(-p.PID, s)
	} else {
		syscall.Kill(p.PID, s)
	}
}

func killProcess(pid int) { syscall.Kill(pid, syscall.SIGKILL) }

// signalExitCode is the exit code a session's program killed by sig ends
// with (128 + the signal's number, as the holder reports it).
func signalExitCode(sig string) int { return 128 + int(endSignals[sig]) }
