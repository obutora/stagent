package holder

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	registerTimeout = 5 * time.Second
	minBackoff      = 500 * time.Millisecond
	maxBackoff      = 30 * time.Second
	maxQueuedNotes  = 16
	noteMaxAge      = time.Minute // older program notifications are stale
)

type queuedNote struct {
	p  wire.HolderNotifyParams
	at time.Time
}

// daemonLink keeps the holder registered with the daemon: it registers the
// full session, then sends coalesced holder.update patches, holder.notify
// for program notifications and finally holder.ended. It redials with
// backoff (re-registering) whenever the daemon goes away; the session keeps
// running meanwhile. Nothing here is ever waited on by the output path.
type daemonLink struct {
	h    *Holder
	dial func() (net.Conn, error)
	// dialExisting connects without starting a daemon; used after the
	// daemon announced a deliberate stop (daemon.stopping), so that
	// `stagent uninstall --level stop` is not undone by a reconnecting
	// holder. The session re-registers once anything else starts it.
	dialExisting func() (net.Conn, error)
	stopped      atomic.Bool

	wakeCh  chan struct{}
	endCh   chan struct{}
	endOnce sync.Once
	code    int // exit code, set before endCh closes
	done    chan struct{}

	mu    sync.Mutex
	notes []queuedNote

	failing bool // logging: only transitions are logged
}

func newDaemonLink(h *Holder, dial, dialExisting func() (net.Conn, error)) *daemonLink {
	return &daemonLink{
		h: h, dial: dial, dialExisting: dialExisting,
		wakeCh: make(chan struct{}, 1), endCh: make(chan struct{}), done: make(chan struct{}),
	}
}

// wake asks the link to send whatever changed in the session.
func (l *daemonLink) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// notify queues a program notification (oldest dropped when full).
func (l *daemonLink) notify(p wire.HolderNotifyParams) {
	l.mu.Lock()
	if len(l.notes) >= maxQueuedNotes {
		l.notes = append(l.notes[:0], l.notes[1:]...)
	}
	l.notes = append(l.notes, queuedNote{p: p, at: time.Now()})
	l.mu.Unlock()
	l.wake()
}

// end reports the exit and makes run return after sending holder.ended (or
// right away when no daemon is connected).
func (l *daemonLink) end(code int) {
	l.endOnce.Do(func() {
		l.code = code
		close(l.endCh)
	})
}

func (l *daemonLink) ended() bool {
	select {
	case <-l.endCh:
		return true
	default:
		return false
	}
}

func (l *daemonLink) run() {
	defer close(l.done)
	backoff := minBackoff
	for {
		dial := l.dial
		if l.stopped.Load() {
			dial = l.dialExisting
		}
		conn, err := dial()
		if err == nil {
			var registered bool
			registered, err = l.session(conn)
			if registered {
				backoff = minBackoff
			}
			if err == nil {
				return // ended and reported
			}
		}
		if l.ended() {
			return
		}
		if !l.failing {
			l.failing = true
			l.h.logf("stagent run: daemon unavailable, retrying in the background: %v", err)
		}
		select {
		case <-time.After(backoff):
		case <-l.endCh:
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// session registers over conn and serves it until the connection drops
// (error) or the session ended and holder.ended was sent (nil).
func (l *daemonLink) session(conn net.Conn) (registered bool, err error) {
	c := rpc.NewClient(conn, func(m *wire.Msg) {
		if m.Method == wire.MethodDaemonStopping {
			l.stopped.Store(true)
		}
	})
	defer c.Close()

	cur := l.h.session()
	ctx, cancel := context.WithTimeout(context.Background(), registerTimeout)
	var res wire.HolderRegisterResult
	err = c.Call(ctx, wire.MethodHolderRegister, cur, &res)
	cancel()
	if err != nil {
		return false, err
	}
	if res.IdleAfterMs > 0 {
		l.h.det.SetIdleAfter(time.Duration(res.IdleAfterMs) * time.Millisecond)
	}
	if l.failing {
		l.failing = false
		l.h.logf("stagent run: registered with the daemon")
	}
	// A daemon that accepted us is running again: a later crash should be
	// recovered by auto-starting as usual.
	l.stopped.Store(false)
	sent := cur
	for {
		if err := l.flush(c, &sent); err != nil {
			return true, err
		}
		select {
		case <-l.wakeCh:
		case <-l.endCh:
			if err := l.flush(c, &sent); err != nil {
				return true, err
			}
			if err := c.Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: l.h.id, ExitCode: l.code}); err != nil {
				return true, err
			}
			return true, nil
		case <-c.Done():
			if err := c.Err(); err != nil {
				return true, err
			}
			return true, errors.New("daemon closed the connection")
		}
	}
}

// flush sends the session fields that changed since sent, then queued
// notifications.
func (l *daemonLink) flush(c *rpc.Client, sent *wire.Session) error {
	cur := l.h.session()
	if p, ok := sessionPatch(sent, &cur); ok {
		if err := c.Notify(wire.MethodHolderUpdate, p); err != nil {
			return err
		}
		*sent = cur
	}
	l.mu.Lock()
	notes := l.notes
	l.notes = nil
	l.mu.Unlock()
	for _, n := range notes {
		if time.Since(n.at) > noteMaxAge {
			continue
		}
		if err := c.Notify(wire.MethodHolderNotify, n.p); err != nil {
			return err
		}
	}
	return nil
}

func sessionPatch(old, cur *wire.Session) (wire.SessionPatch, bool) {
	p := wire.SessionPatch{ID: cur.ID}
	changed := false
	if old.State != cur.State || old.StateSource != cur.StateSource {
		p.State, p.StateSource = &cur.State, &cur.StateSource
		changed = true
	}
	if old.Title != cur.Title {
		p.Title = &cur.Title
		changed = true
	}
	if old.LastActivityAt != cur.LastActivityAt {
		p.LastActivityAt = &cur.LastActivityAt
		changed = true
	}
	if old.Cols != cur.Cols || old.Rows != cur.Rows {
		p.Cols, p.Rows = &cur.Cols, &cur.Rows
		changed = true
	}
	return p, changed
}
