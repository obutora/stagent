package bridge

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// A program that ends at once can take its holder with it before the
// bridge first reaches the holder. The holder registered with the daemon
// first, so the spawn answers the session the daemon lists rather than
// failing; a holder the daemon never heard of is still an error.
func TestSpawnOfAHolderThatEndedAtOnce(t *testing.T) {
	gone := exec.Command(os.Args[0], "-test.run=^$")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ids []string
	lay, a := startBridge(t, func(b *Bridge) {
		b.spawnProc = func(exe string, args []string, dir string, env []string, logPath string) (int, error) {
			mu.Lock()
			ids = append(ids, args[slices.Index(args, "--id")+1])
			mu.Unlock()
			return gone.Process.Pid, nil // never serves its socket
		}
	})
	code := 4
	serveFake(t, lay.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method != wire.MethodSessionsList {
			return struct{}{}, nil
		}
		mu.Lock()
		defer mu.Unlock()
		var listed []wire.Session
		if len(ids) > 0 {
			listed = append(listed, wire.Session{ID: ids[0], State: wire.StateExited, ExitCode: &code})
		}
		return wire.SessionsListResult{Sessions: listed}, nil
	})
	spawn := wire.SpawnParams{Command: []string{"false"}, Cols: 80, Rows: 24}

	var res wire.SpawnResult
	if e := a.call(t, wire.MethodSessionSpawn, spawn, &res); e != nil {
		t.Fatalf("spawn of a holder that registered and ended: %v", e)
	}
	mu.Lock()
	first := ids[0]
	mu.Unlock()
	if res.Session.ID != first || res.Session.ExitCode == nil || *res.Session.ExitCode != 4 {
		t.Fatalf("session %+v, want the daemon's ended %s", res.Session, first)
	}

	// The second holder is not among the daemon's sessions.
	if e := a.call(t, wire.MethodSessionSpawn, spawn, nil); e == nil || e.Code != wire.ErrInternal {
		t.Fatalf("spawn of a holder the daemon does not know: %v, want %s", e, wire.ErrInternal)
	}
}
