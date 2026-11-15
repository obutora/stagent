package bridge

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const idC = "cccccccccccccccc"

// A session.signal that ends a session whose holder does not answer — no
// socket (a purge deleted it), or a holder that stays silent — is the
// daemon's to carry out (session.end), so the app's 終了 ends it instead
// of failing with "no holder answers" while the session stays listed.
func TestEndingSignalFallsBackToTheDaemon(t *testing.T) {
	l, a := startBridge(t, func(b *Bridge) { b.holderEndWait = 200 * time.Millisecond })
	var mu sync.Mutex
	var ends []wire.SessionEndParams
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodSessionEnd {
			var p wire.SessionEndParams
			if err := rpc.Decode(m, &p); err != nil {
				return nil, err
			}
			mu.Lock()
			ends = append(ends, p)
			mu.Unlock()
		}
		return struct{}{}, nil
	})
	endsAre := func(want ...wire.SessionEndParams) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(ends, want) {
			t.Fatalf("session.end calls %+v, want %+v", ends, want)
		}
	}
	signal := func(id, sig string) *wire.Error {
		t.Helper()
		return a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: sig}, nil)
	}

	endA := wire.SessionEndParams{ID: idA, Signal: wire.SignalHangup}
	if e := signal(idA, wire.SignalHangup); e != nil {
		t.Fatalf("hangup of a session no holder answers for: %v", e)
	}
	endsAre(endA)
	// Ctrl-C ends nothing: it still fails.
	if e := signal(idA, wire.SignalInterrupt); e == nil || e.Code != wire.ErrNotFound {
		t.Fatalf("interrupt without a holder: %v", e)
	}
	endsAre(endA)

	// A holder that answers gets the signal, its answer goes to the app.
	var got []string
	serveFake(t, l.HolderAddr(idB), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		var p wire.SignalParams
		rpc.Decode(m, &p)
		mu.Lock()
		got = append(got, p.Signal)
		n := len(got)
		mu.Unlock()
		if n > 1 {
			return nil, wire.Errorf(wire.ErrSessionEnded, "session ended")
		}
		return struct{}{}, nil
	})
	if e := signal(idB, wire.SignalTerminate); e != nil {
		t.Fatalf("terminate through the holder: %v", e)
	}
	if e := signal(idB, wire.SignalHangup); e == nil || e.Code != wire.ErrSessionEnded {
		t.Fatalf("hangup the holder refuses: %v, want its session_ended", e)
	}
	mu.Lock()
	if !slices.Equal(got, []string{wire.SignalTerminate, wire.SignalHangup}) {
		t.Fatalf("holder B got %q", got)
	}
	mu.Unlock()
	endsAre(endA)

	// A holder that stays silent is ended by the daemon after holderEndWait.
	serveFake(t, l.HolderAddr(idC), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return rpc.Async, nil
	})
	if e := signal(idC, wire.SignalKill); e != nil {
		t.Fatalf("kill of a silent holder's session: %v", e)
	}
	endsAre(endA, wire.SessionEndParams{ID: idC, Signal: wire.SignalKill})
}

// A daemon before session.end leaves the error as it was.
func TestEndingSignalWithADaemonWithoutSessionEnd(t *testing.T) {
	l, a := startBridge(t)
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method)
	})
	e := a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: idA, Signal: wire.SignalHangup}, nil)
	if e == nil || e.Code != wire.ErrNotFound || !strings.Contains(e.Message, "no holder answers") {
		t.Fatalf("hangup without a holder, older daemon: %v", e)
	}
}
