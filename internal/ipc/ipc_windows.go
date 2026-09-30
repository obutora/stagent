package ipc

import (
	"errors"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/paths"
)

// Listen serves the named pipe addr. The pipe's DACL grants access to the
// current user's SID only (protected, so nothing is inherited).
func Listen(addr string) (net.Listener, error) {
	sid, err := paths.CurrentUserSID()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return ln, nil
}

// Dial connects to the named pipe addr.
func Dial(addr string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(addr, &timeout)
}
