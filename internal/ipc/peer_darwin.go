package ipc

import (
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	var cred *unix.Xucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return -1, err
	}
	if cerr != nil {
		return -1, cerr
	}
	return int(cred.Uid), nil
}

// PeerPID returns the pid LOCAL_PEERPID reports for c: the last process
// that used the peer's socket (xnu's last_pid), normally the one that
// connected — a process the socket was passed to once it uses it.
func PeerPID(c net.Conn) (int, error) {
	raw, err := rawConn(c)
	if err != nil {
		return -1, err
	}
	var pid int
	var perr error
	if err := raw.Control(func(fd uintptr) {
		pid, perr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return -1, err
	}
	if perr != nil {
		return -1, perr
	}
	return pid, nil
}
