//go:build !windows

package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	cpty "github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/obutora/stagent/internal/wire"
)

type unixPTY struct {
	f   *os.File
	fd  int // f's descriptor, read once: os.File.Fd races with Close
	cmd *exec.Cmd

	done chan struct{}
	code int
	err  error

	mu     sync.RWMutex // closed vs. ioctls on fd
	closed bool
}

// Start runs argv in dir with env (nil inherits) on a new terminal of the
// given size. The program leads a new session with the terminal as its
// controlling tty. A program that cannot be found yields an error wrapping
// exec.ErrNotFound.
func Start(argv []string, dir string, env []string, cols, rows int) (PTY, error) {
	if len(argv) == 0 {
		return nil, errors.New("pty: empty command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if cmd.Err != nil {
		return nil, cmd.Err
	}
	cmd.Dir = dir
	cmd.Env = env
	// StartWithSize sets Setsid and Setctty.
	f, err := cpty.StartWithSize(cmd, &cpty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	p := &unixPTY{f: f, fd: int(f.Fd()), cmd: cmd, done: make(chan struct{})}
	go func() {
		werr := cmd.Wait()
		p.code, p.err = exitStatus(cmd.ProcessState, werr)
		close(p.done)
	}()
	return p, nil
}

func exitStatus(ps *os.ProcessState, err error) (int, error) {
	if ps == nil {
		return -1, err
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal()), nil
	}
	return ps.ExitCode(), nil
}

func (p *unixPTY) Read(b []byte) (int, error) {
	n, err := p.f.Read(b)
	if err != nil && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		// Linux reports a master whose slave side is gone as EIO.
		err = io.EOF
	}
	return n, err
}

func (p *unixPTY) Write(b []byte) (int, error) { return p.f.Write(b) }

func (p *unixPTY) Resize(cols, rows int) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.closed {
		return os.ErrClosed
	}
	return unix.IoctlSetWinsize(p.fd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
}

func (p *unixPTY) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *unixPTY) Pid() int { return p.cmd.Process.Pid }

func (p *unixPTY) Signal(name string) error {
	switch name {
	case wire.SignalInterrupt:
		_, err := p.f.Write([]byte{0x03})
		return err
	case wire.SignalTerminate:
		return p.kill(syscall.SIGTERM)
	case wire.SignalKill:
		return p.kill(syscall.SIGKILL)
	case SignalHangup:
		return p.kill(syscall.SIGHUP)
	}
	return ErrUnknownSignal
}

// kill signals the program's process group and, when a job-control shell
// runs something else in the foreground, that foreground group too — what
// a terminal does on hangup.
func (p *unixPTY) kill(sig syscall.Signal) error {
	select {
	case <-p.done:
		return nil // reaped; the pid may already belong to someone else
	default:
	}
	pid := p.cmd.Process.Pid
	p.mu.RLock()
	if !p.closed {
		if fg, err := unix.IoctlGetInt(p.fd, unix.TIOCGPGRP); err == nil && fg > 0 && fg != pid {
			syscall.Kill(-fg, sig)
		}
	}
	p.mu.RUnlock()
	err := syscall.Kill(-pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		err = syscall.Kill(pid, sig)
	}
	return err
}

func (p *unixPTY) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.f.Close()
}
