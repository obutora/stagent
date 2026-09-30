// Package pty runs a program on a pseudo terminal: openpty (creack/pty) on
// Linux and macOS, ConPTY on Windows 10 1809 and later.
package pty

import (
	"errors"
	"io"
)

// PTY is a running program and the master side of its terminal.
type PTY interface {
	// Read returns program output; io.EOF (or another error) once the
	// terminal is gone.
	io.Reader
	// Write sends input to the program.
	io.Writer
	// Resize changes the terminal size.
	Resize(cols, rows int) error
	// Wait blocks until the program exits and returns its exit code
	// (128+signal for a signal death on Unix). Safe to call repeatedly.
	Wait() (exitCode int, err error)
	// Pid is the program's process id.
	Pid() int
	// Signal delivers wire.SignalInterrupt (Ctrl-C through the terminal),
	// wire.SignalTerminate, wire.SignalKill or SignalHangup.
	Signal(name string) error
	// Close releases the terminal. Output still buffered may be lost; call
	// it after Wait.
	Close() error
}

// SignalHangup tells the program its terminal went away (SIGHUP on Unix,
// the same as terminate on Windows).
const SignalHangup = "hangup"

// ErrUnknownSignal is returned by Signal for names it does not know.
var ErrUnknownSignal = errors.New("pty: unknown signal")
