package follow

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// holderTimeout bounds dialing a holder and its session.info answer: a scan
// waits for it, and a holder answers at once unless it is stuck.
const holderTimeout = time.Second

// holderSession is the process root of `stagent follow --session`: the
// program of a stagent session, as its holder reports it. The connection
// to the holder is kept across scans and asked again on each one, so a
// session that ends — its holder answers with an exit code, or stops
// answering — reads as gone at the next scan.
type holderSession struct {
	id   string
	dial func() (net.Conn, error)
	c    *rpc.Client
}

// root returns the session's program pid. Its errors name the session and
// serve as the diag of the no_terminal target.
func (h *holderSession) root() (int, error) {
	reused := h.c != nil
	s, err := h.info()
	if err != nil && reused {
		// The connection of an earlier scan may have outlived a holder
		// that serves again; only a fresh one tells.
		h.drop()
		s, err = h.info()
	}
	if err != nil {
		h.drop()
		return 0, fmt.Errorf("stagent session %s: holder not reachable: %w", h.id, err)
	}
	switch {
	case s.ExitCode != nil:
		return 0, fmt.Errorf("stagent session %s: session ended (exit %d)", h.id, *s.ExitCode)
	case s.PID <= 0:
		return 0, fmt.Errorf("stagent session %s: holder reports no program pid", h.id)
	}
	return s.PID, nil
}

// info asks the holder for the session, dialing it when there is no
// connection.
func (h *holderSession) info() (wire.Session, error) {
	var s wire.Session
	if h.c == nil {
		conn, err := h.dial()
		if err != nil {
			return s, err
		}
		h.c = rpc.NewClient(conn, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), holderTimeout)
	defer cancel()
	err := h.c.Call(ctx, wire.MethodSessionInfo, wire.SessionRef{ID: h.id}, &s)
	return s, err
}

func (h *holderSession) drop() {
	if h.c != nil {
		h.c.Close()
		h.c = nil
	}
}

// validSessionID reports whether id is a stagent session id (16 lowercase
// hex characters, see wire.NewSessionID); it becomes part of the holder's
// address.
func validSessionID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range []byte(id) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
