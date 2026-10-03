package follow

import (
	"runtime"

	"github.com/obutora/stagent/internal/ptable"
)

// AgentProbe tells whether an agent still runs in stagent sessions. It
// recognizes agents exactly as `stagent follow --session` does: in the
// process tree of the session's program and in the panes of the
// multiplexer clients there. The daemon uses it to return a shell session
// that hooks promoted to an agent once that agent has left.
type AgentProbe struct {
	take func() (*ptable.Snapshot, error)
	loc  *locator
	// noForeground: the platform's terminals report no foreground process
	// group (Windows), so an agent anywhere in the session counts.
	noForeground bool
}

// SessionAgent is one session to probe: the pid of its program and the
// harness expected to run in it.
type SessionAgent struct {
	PID     int
	Harness string
}

// NewAgentProbe returns a probe of this host's process table, trusting the
// processes of the current user only (see locator.owns).
func NewAgentProbe() (*AgentProbe, error) {
	owner, err := ptable.CurrentOwner()
	if err != nil {
		return nil, err
	}
	return newAgentProbe(ptable.Take, newLocator(execMux{}, owner), runtime.GOOS == "windows"), nil
}

func newAgentProbe(take func() (*ptable.Snapshot, error), loc *locator, noForeground bool) *AgentProbe {
	return &AgentProbe{take: take, loc: loc, noForeground: noForeground}
}

// Running reports, for each session, whether an agent of its harness runs
// in it in front of the user: in the foreground of the session's terminal,
// or of the pane a multiplexer client in that foreground shows (a
// suspended or background agent does not count). Every session is judged
// from one process-table snapshot. Not safe for concurrent use.
func (p *AgentProbe) Running(sessions []SessionAgent) ([]bool, error) {
	s, err := p.take()
	if err != nil {
		return nil, err
	}
	out := make([]bool, len(sessions))
	for i, sa := range sessions {
		if sa.PID <= 0 {
			continue
		}
		pid := sa.PID
		p.loc.session = func() (int, error) { return pid, nil }
		for _, c := range p.loc.locate(s).agents {
			if c.harness == sa.Harness && (c.foreground || p.noForeground) {
				out[i] = true
				break
			}
		}
	}
	p.loc.session = nil
	return out, nil
}
