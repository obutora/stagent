package install

import (
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// refusingDaemon answers daemon.shutdown as the daemon answers a coding
// agent (ADR 0004).
type refusingDaemon struct{ *fakeDaemon }

func (d refusingDaemon) Shutdown() error {
	return wire.Errorf(wire.ErrAgentRefused, "refused")
}

// TestAgentCannotStopDaemon: run by a coding agent, install and uninstall
// report the refusal and do not kill the daemon instead.
func TestAgentCannotStopDaemon(t *testing.T) {
	te := newTestEnv(t, "linux")
	te.daemon.running, te.daemon.pid, te.daemon.version = true, 4242, "0.0.9"
	te.env.daemon = refusingDaemon{te.daemon}
	r, err := te.install()
	if err != nil {
		t.Fatal(err)
	}
	if r.ReplacedDaemon != "" || te.spawns != 0 || len(te.killed) != 0 {
		t.Fatalf("install: replaced %q, spawns %d, killed %v", r.ReplacedDaemon, te.spawns, te.killed)
	}
	refused := false
	for _, n := range te.notes {
		refused = refused || n.Code == noteDaemonReplaceFailed && strings.Contains(n.Text, wire.ErrAgentRefused)
	}
	if !refused {
		t.Errorf("install notes %+v do not say the daemon refused", te.notes)
	}

	te.reload()
	u := te.uninstall("stop", false, false)
	if len(te.killed) != 0 || len(u.Steps) != 1 || u.Steps[0].OK || !strings.Contains(u.Steps[0].Error, wire.ErrAgentRefused) {
		t.Fatalf("uninstall --level stop: killed %v, steps %+v", te.killed, u.Steps)
	}
}
