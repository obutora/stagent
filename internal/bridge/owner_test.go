package bridge

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

func TestHelloReportsBlockedLocation(t *testing.T) {
	_, a := startBridge(t)
	var res wire.HelloResult
	if e := a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "test"}, &res); e != nil {
		t.Fatal(e)
	}
	if res.Blocked != nil {
		t.Fatalf("hello.blocked on a free location = %+v", res.Blocked)
	}

	_, a = startBridge(t, func(b *Bridge) {
		b.blocked = func() *paths.OwnerError { return &paths.OwnerError{Path: "/tmp/stagent-1000", Owner: "mallory"} }
	})
	res = wire.HelloResult{}
	if e := a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "test"}, &res); e != nil {
		t.Fatal(e)
	}
	if res.Blocked == nil || *res.Blocked != (wire.Blocked{Path: "/tmp/stagent-1000", Owner: "mallory"}) {
		t.Fatalf("hello.blocked = %+v", res.Blocked)
	}
}

// Requests that would reach a daemon or holder another user serves fail
// as foreign_owner, naming the place and its owner.
func TestForeignOwnerErrors(t *testing.T) {
	foreign := func(addr string) error { return &paths.OwnerError{Path: addr, Owner: "mallory"} }
	_, a := startBridge(t, func(b *Bridge) {
		b.dialDaemon = func() (net.Conn, error) { return nil, foreign("/tmp/stagent-1000/stagent.sock") }
		b.dialHolder = func(id string, _ time.Duration) (net.Conn, error) {
			return nil, foreign("/tmp/stagent-1000/s/" + id + ".sock")
		}
		b.spawnProc = func(string, []string, string, []string, string) (int, error) { return 0, nil }
	})
	check := func(what string, e *wire.Error, path string) {
		t.Helper()
		if e == nil || e.Code != wire.ErrForeignOwner || !strings.Contains(e.Message, path) || !strings.Contains(e.Message, "mallory") {
			t.Errorf("%s: %+v, want foreign_owner naming %s and mallory", what, e, path)
		}
	}
	check("sessions.list", a.call(t, wire.MethodSessionsList, nil, nil), "/tmp/stagent-1000/stagent.sock")
	check("session.attach", a.call(t, wire.MethodSessionAttach, wire.SessionRef{ID: idA}, nil), "/tmp/stagent-1000/s/"+idA+".sock")
	check("session.spawn", a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{"sh"}, Cols: 80, Rows: 24}, nil), "/tmp/stagent-1000/s/")
}

// A bridge in a coding agent's process tree starts no holder: the holder
// would refuse the bridge and keep running out of reach (ADR 0004).
func TestSpawnRefusedInAgentTree(t *testing.T) {
	var started atomic.Bool
	_, a := startBridge(t, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error {
			return wire.Errorf(wire.ErrAgentRefused, "stagent refuses connections from processes started by a coding agent (claude)")
		}
		b.spawnProc = func(string, []string, string, []string, string) (int, error) {
			started.Store(true)
			return 0, nil
		}
	})
	e := a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cols: 80, Rows: 24}, nil)
	if e == nil || e.Code != wire.ErrAgentRefused {
		t.Fatalf("session.spawn in an agent's tree: %+v, want %s", e, wire.ErrAgentRefused)
	}
	if started.Load() {
		t.Error("a holder was started")
	}
}
