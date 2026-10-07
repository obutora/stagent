package harness

import (
	"errors"
	"fmt"
	"net"
	"runtime"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// maxAncestry bounds walks up the process tree; a deeper one is not
	// trusted.
	maxAncestry = 256
	// maxWalks bounds the walks restarted because the tree changed under
	// them.
	maxWalks = 8
)

// Why Table.Agent cannot tell; Guard.Check refuses on each.
var (
	errGone     = errors.New("the connecting process has exited")
	errTooDeep  = fmt.Errorf("the connecting process has more than %d ancestors", maxAncestry)
	errUnstable = errors.New("the connecting process's ancestors kept changing")
)

// Table reads single processes; ptable.Process and ptable.Argv on a real
// host.
type Table struct {
	Process func(pid int) (ptable.Proc, bool)
	Argv    func(pid int) ([]string, error)
}

// Agent returns the harness that pid is, or that is one of its ancestors
// ("" when there is none). It fails when that cannot be told: pid exited,
// its ancestry is deeper than maxAncestry, or kept changing.
//
// An ancestor that cannot be read while its child is still there with the
// same parent ends the walk: it runs as another user, or is hidden like one
// (Linux's hidepid), and an agent runs as stagent's user — the only user
// ipc lets connect. When the child is gone or has a new parent instead,
// an ancestor exited meanwhile, and the walk starts again from pid along
// the new parents (up to a sandbox's subreaper).
func (t Table) Agent(pid int) (string, error) {
	for range maxWalks {
		h, again, err := t.walk(pid)
		if !again {
			return h, err
		}
	}
	return "", errUnstable
}

// walk is one walk of Agent; again asks for another.
func (t Table) walk(pid int) (h string, again bool, err error) {
	p, ok := t.Process(pid)
	if !ok {
		return "", false, errGone
	}
	for range maxAncestry {
		argv, err := t.Argv(p.PID)
		if err != nil {
			// An agent run by an interpreter is told by its argv. One that
			// cannot be read of a process still there with the same parent
			// is another user's (macOS) and leaves the name; one that
			// exited or was reparented meanwhile walks again.
			if now, still := t.Process(p.PID); !still || now.PPID != p.PPID {
				return "", true, nil
			}
		}
		if h := Of(p.Name, argv); h != "" {
			return h, false, nil
		}
		if p.PPID <= 0 || p.PPID == p.PID {
			return "", false, nil
		}
		parent, found := t.Process(p.PPID)
		if !found {
			if now, still := t.Process(p.PID); still && now.PPID == p.PPID {
				return "", false, nil
			}
			return "", true, nil
		}
		p = parent
	}
	return "", false, errTooDeep
}

// Guard tells whether a connection comes from a coding agent's process
// tree, which stagent refuses (ADR 0004): with a sandboxed agent's commands
// approved without asking, the agent could otherwise type approvals into
// its own session or commands into another one outside its sandbox.
type Guard struct {
	// PeerPID returns the pid of the process that connected c.
	PeerPID func(c net.Conn) (int, error)
	Table   Table
}

// NewGuard returns the guard of the stagent location l: nil, which refuses
// nothing, when l is isolated (STAGENT_HOME: tests and development, which
// run under agents) and on Windows, which keeps the parent pid of a process
// whose parent exited, so parents cannot be told reliably. The receiving
// side decides with its own layout, so a client cannot opt out.
func NewGuard(l *paths.Layout) *Guard {
	if l.Isolated || runtime.GOOS == "windows" {
		return nil
	}
	return &Guard{PeerPID: ipc.PeerPID, Table: Table{Process: ptable.Process, Argv: ptable.Argv}}
}

// Check returns nil when c may be served, else the agent_refused error to
// answer its requests with. A connection whose process cannot be read any
// more is refused too: a process could otherwise hand its socket to a child
// and exit before it is checked.
func (g *Guard) Check(c net.Conn) *wire.Error {
	if g == nil {
		return nil
	}
	pid, err := g.PeerPID(c)
	if err == nil && pid <= 0 {
		err = errors.New("no pid") // Linux: the peer's pid namespace is not ours
	}
	if err != nil {
		return wire.Errorf(wire.ErrAgentRefused, "cannot tell which process connected (%v); stagent refuses connections it cannot trace to a non-agent process (ADR 0004)", err)
	}
	return g.CheckPID(pid)
}

// CheckPID is Check for the process pid: the bridge asks it of itself
// before it starts a holder that would refuse its connection.
func (g *Guard) CheckPID(pid int) *wire.Error {
	if g == nil {
		return nil
	}
	h, err := g.Table.Agent(pid)
	switch {
	case err != nil:
		return wire.Errorf(wire.ErrAgentRefused, "%v (pid %d); stagent refuses connections it cannot trace to a non-agent process (ADR 0004)", err, pid)
	case h != "":
		return wire.Errorf(wire.ErrAgentRefused, "stagent refuses connections from processes started by a coding agent (%s); run this command in your own terminal (ADR 0004)", h)
	}
	return nil
}
