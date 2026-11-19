package follow

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// holderTimeout bounds dialing a holder and its session.info answer, and
// the daemon and its sessions.list answer for the session's record: a scan
// waits for them, and both answer at once unless they are stuck.
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

// daemonRecord reads the daemon's record of the session `follow --session`
// follows: the ConversationID and TranscriptPath its hooks reported. The
// holder does not know them. Like holderSession, the connection is kept
// across scans and dialed again once when an earlier scan's one fails.
type daemonRecord struct {
	id   string
	dial func() (net.Conn, error)
	c    *rpc.Client
}

// read returns the daemon's record of the session; found is false when
// the daemon answers without it. err is set when the daemon does not
// answer (not running, restarting, stuck).
func (d *daemonRecord) read() (s wire.Session, found bool, err error) {
	reused := d.c != nil
	list, err := d.list()
	if err != nil && reused {
		d.drop()
		list, err = d.list()
	}
	if err != nil {
		d.drop()
		return wire.Session{}, false, err
	}
	for _, s := range list.Sessions {
		if s.ID == d.id {
			return s, true, nil
		}
	}
	return wire.Session{}, false, nil
}

func (d *daemonRecord) list() (wire.SessionsListResult, error) {
	var res wire.SessionsListResult
	if d.c == nil {
		conn, err := d.dial()
		if err != nil {
			return res, err
		}
		d.c = rpc.NewClient(conn, nil)
	}
	ctx, cancel := context.WithTimeout(context.Background(), holderTimeout)
	defer cancel()
	err := d.c.Call(ctx, wire.MethodSessionsList, struct{}{}, &res)
	return res, err
}

func (d *daemonRecord) drop() {
	if d.c != nil {
		d.c.Close()
		d.c = nil
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
