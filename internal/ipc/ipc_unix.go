//go:build !windows

package ipc

import (
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/obutora/stagent/internal/paths"
)

// Listen serves addr (a socket path). A stale socket file left by a crashed
// process is removed; a live one yields ErrInUse. The socket is chmod 0600
// and every accepted connection is checked to come from the same UID.
func Listen(addr string) (net.Listener, error) {
	if _, err := os.Lstat(addr); err == nil {
		if c, err := net.DialTimeout("unix", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil, ErrInUse
		}
		if err := os.Remove(addr); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", addr)
	if err != nil {
		return nil, err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(true)
	if err := os.Chmod(addr, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &sameUserListener{ln}, nil
}

// Dial connects to addr and makes sure the process serving it runs as us:
// a socket another user created first (e.g. in a shared /tmp/stagent-<uid>
// they made) is closed unused and reported as a *paths.OwnerError.
func Dial(addr string, timeout time.Duration) (net.Conn, error) {
	c, err := net.DialTimeout("unix", addr, timeout)
	if err != nil {
		return nil, err
	}
	uid, err := serverUID(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("ipc: %s: cannot tell who serves it: %w", addr, err)
	}
	if uid != os.Getuid() {
		c.Close()
		return nil, &paths.OwnerError{Path: addr, Owner: paths.UIDOwner(uid)}
	}
	return c, nil
}

// serverUID returns the UID of the process at the other end of a dialed
// connection. Tests replace it: without root they cannot serve a socket as
// another user.
var serverUID = func(c net.Conn) (int, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return -1, errors.New("not a unix socket")
	}
	return peerUID(uc)
}

type sameUserListener struct{ net.Listener }

func (l *sameUserListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uc, ok := c.(*net.UnixConn)
		if !ok {
			return c, nil
		}
		uid, err := peerUID(uc)
		if err == nil && uid == os.Getuid() {
			return c, nil
		}
		c.Close()
	}
}
