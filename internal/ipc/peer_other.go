//go:build !linux && !darwin

package ipc

import (
	"errors"
	"net"
)

// PeerPID is unsupported on this platform (Windows: stagent does not walk
// the parents of its clients there, see ADR 0004).
func PeerPID(net.Conn) (int, error) { return -1, errors.ErrUnsupported }
