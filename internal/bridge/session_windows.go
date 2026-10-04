package bridge

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// watchSession returns a channel closed when the SSH session process that
// runs the bridge ends, or nil when it cannot be watched.
//
// The bridge ends with stdin's EOF, which on Windows needs every handle to
// the pipe's write end closed. When Win32-OpenSSH's session process
// (sshd -z) is killed, the write end can live on in another process and
// stdin never ends (#270). sshd -z creates the pipe, so the pipe's server
// process is the session.
func watchSession(stdin *os.File) <-chan struct{} {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(stdin.Fd()), &pid); err != nil || int(pid) == os.Getpid() {
		return nil // not a pipe, or one the bridge made itself
	}
	ended := make(chan struct{})
	p, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) { // no such process
		close(ended)
		return ended
	}
	if err != nil {
		return nil
	}
	// The pipe existed before the bridge started, so a server created later
	// is another process that got the pid of the ended one.
	if mine, its := created(windows.CurrentProcess()), created(p); mine != 0 && its > mine {
		windows.CloseHandle(p)
		close(ended)
		return ended
	}
	go func() {
		windows.WaitForSingleObject(p, windows.INFINITE)
		windows.CloseHandle(p)
		close(ended)
	}()
	return ended
}

// created returns the creation time of process p (0 when unknown).
func created(p windows.Handle) int64 {
	var creation, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(p, &creation, &exit, &kernel, &user) != nil {
		return 0
	}
	return creation.Nanoseconds()
}
