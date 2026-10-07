//go:build linux || darwin

package ipc

import (
	"errors"
	"net"
	"syscall"
)

// rawConn is the socket of c, a connection Listen accepted.
func rawConn(c net.Conn) (syscall.RawConn, error) {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return nil, errors.New("ipc: not a socket connection")
	}
	return sc.SyscallConn()
}
