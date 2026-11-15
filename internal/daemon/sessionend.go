package daemon

import (
	"net"
	"os"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/proc"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// Ending a session its holder does not answer for (session.end).
//
// The app ends a session with session.signal, which the bridge forwards to
// the session's holder over the holder's socket. A holder can stay
// registered — its connection to the daemon open, the session listed as
// live — while nothing answers on that socket: `stagent uninstall --level
// purge` (the app's "remove stagent from the server") or a tmp cleaner
// deleted it, the holder stopped accepting, or it hangs. Nothing could
// then end the session, which stayed listed until its program exited on
// its own. When the holder does not answer, the bridge has the daemon end
// the session by the pids its holder registered.

const (
	// endGrace is how long session.end waits for the end after signalling
	// the program — the holder reports the exit (holder.ended) — before it
	// kills the program and the holder and records the end itself.
	endGrace = 3 * time.Second
	endPoll  = 50 * time.Millisecond
	// A process counts as the session's program or holder by its pid only
	// when it started between startSlack before the session's started_at
	// and clockSlack after it: pids are reused, and Linux derives start
	// times from a boot time in whole seconds.
	startSlack = time.Minute
	clockSlack = 5 * time.Second
)

// sessionEnd serves session.end: it signals the program of live session
// p.ID (its process group and the terminal's foreground job) with
// p.Signal, waits up to endGrace for the holder to report the exit, then
// records the end itself — exit code of the signal, a hangup counting as
// one — and kills what is left of the program and the holder. A session
// the daemon does not list as live is already over: nil.
func (d *Daemon) sessionEnd(p wire.SessionEndParams) error {
	switch p.Signal {
	case wire.SignalHangup, wire.SignalTerminate, wire.SignalKill:
	default:
		return wire.Errorf(wire.ErrBadRequest, "session.end: unknown signal %q", p.Signal)
	}
	d.mu.Lock()
	s := d.sessions[p.ID]
	if d.closed || s == nil || s.ended {
		d.mu.Unlock()
		return nil
	}
	s.endSignal = p.Signal
	var holderConn net.Conn
	if s.holder != nil {
		holderConn = s.holder.nc
	}
	ws := s.s
	d.mu.Unlock()

	sp := findSessionProcs(ws, holderConn)
	d.logf("daemon: session %s: its holder does not answer; ending it (%s; program pid %d %s, holder pid %d %s)",
		p.ID, p.Signal, ws.PID, procState(sp.program), ws.HolderPID, procState(sp.holder))
	if sp.program != nil {
		signalProgram(sp.program, p.Signal)
	}
	if d.awaitEnd(s, sp.program) {
		return nil
	}

	// The holder did not report the end: record it, then kill what is
	// left (its connection closing finds the session ended).
	var program, holder bool
	if snap, err := ptable.Take(); err == nil {
		program = sp.program != nil && sameProc(snap.Get(sp.program.PID), sp.program)
		holder = sp.holder != nil && sameProc(snap.Get(sp.holder.PID), sp.holder)
	}
	code := signalExitCode(p.Signal)
	if program {
		code = signalExitCode(wire.SignalKill)
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return wire.Errorf(wire.ErrUnavailable, "daemon stopping")
	}
	if d.sessions[p.ID] == s && !s.ended {
		d.endLocked(s, code, false, p.Signal == wire.SignalHangup)
	}
	d.mu.Unlock()
	if program {
		signalProgram(sp.program, wire.SignalKill)
	}
	if holder {
		killProcess(sp.holder.PID)
	}
	return nil
}

// awaitEnd waits up to endGrace for session s to end. It gives up early
// when s has no holder connection to report the end and program (nil: none
// was found) is gone.
func (d *Daemon) awaitEnd(s *session, program *ptable.Proc) bool {
	deadline := time.Now().Add(endGrace)
	for {
		d.mu.Lock()
		ended, closed, connected := s.ended, d.closed, s.holder != nil
		d.mu.Unlock()
		if ended {
			return true
		}
		if closed || time.Now().After(deadline) || !connected && (program == nil || !proc.Alive(program.PID)) {
			return false
		}
		time.Sleep(endPoll)
	}
}

// sessionProcs are the processes of a session found by the pids its
// holder registered; nil when gone or not provably the session's.
type sessionProcs struct {
	holder, program *ptable.Proc
}

// findSessionProcs looks up the holder and the program of ws. The holder
// is the process with its pid that holds the session's holder connection
// (holderConn, nil while it is disconnected), or that started when the
// session did; the program is the holder's child with its pid, or the
// process with its pid that started when the session did. This process
// (the daemon) and init never count.
func findSessionProcs(ws wire.Session, holderConn net.Conn) sessionProcs {
	snap, err := ptable.Take()
	if err != nil {
		return sessionProcs{}
	}
	started := time.UnixMilli(ws.StartedAt)
	near := func(p *ptable.Proc) bool {
		return !p.Start.IsZero() && p.Start.After(started.Add(-startSlack)) && p.Start.Before(started.Add(clockSlack))
	}
	own := func(pid int) *ptable.Proc {
		if pid <= 1 || pid == os.Getpid() {
			return nil
		}
		return snap.Get(pid)
	}
	var sp sessionProcs
	if h := own(ws.HolderPID); h != nil {
		peer := -1
		if holderConn != nil {
			if pid, err := ipc.PeerPID(holderConn); err == nil {
				peer = pid
			}
		}
		if peer == h.PID || near(h) {
			sp.holder = h
		}
	}
	if p := own(ws.PID); p != nil && (sp.holder != nil && p.PPID == sp.holder.PID || near(p)) {
		sp.program = p
	}
	return sp
}

// sameProc reports whether p, looked up again, is still the process was.
func sameProc(p, was *ptable.Proc) bool {
	if p == nil || was == nil || p.PID != was.PID {
		return false
	}
	d := p.Start.Sub(was.Start)
	return d > -time.Second && d < time.Second
}

func procState(p *ptable.Proc) string {
	if p == nil {
		return "not found"
	}
	return "running"
}
