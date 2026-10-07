// Package ipc provides the local, same-user-only transport between stagent
// processes: Unix domain sockets on Linux/macOS and named pipes on Windows.
// Nothing here ever listens on TCP.
package ipc

import (
	"errors"
	"time"

	"github.com/obutora/stagent/internal/paths"
)

// ErrInUse is returned by Listen when another live process already serves
// the address (e.g. a second daemon racing the first).
var ErrInUse = errors.New("ipc: address already served by a live process")

// blockedProbeTimeout bounds Blocked's dial of the daemon address.
const blockedProbeTimeout = 300 * time.Millisecond

// Blocked reports the stagent location of l that another user owns, so
// that no daemon or kept shell can work: a directory in a shared temporary
// directory (paths.Layout.ForeignOwned), or the daemon's socket or named
// pipe. It returns nil when nothing is blocked. It creates nothing; the
// daemon address is probed with a Dial that is closed right away.
func Blocked(l *paths.Layout) *paths.OwnerError {
	if oe := l.ForeignOwned(); oe != nil {
		return oe
	}
	c, err := Dial(l.DaemonAddr, blockedProbeTimeout)
	if err == nil {
		c.Close()
		return nil
	}
	var oe *paths.OwnerError
	if errors.As(err, &oe) {
		return oe
	}
	return nil
}
