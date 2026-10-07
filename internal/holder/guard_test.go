//go:build !windows

package holder

import (
	"context"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// TestAgentRefusedOnEveryMethod: a connection from a coding agent's process
// tree gets agent_refused for whatever it asks first, then is closed; other
// connections are served.
func TestAgentRefusedOnEveryMethod(t *testing.T) {
	l := isolate(t)
	var agent atomic.Bool
	agent.Store(true)
	guard := &harness.Guard{
		PeerPID: func(net.Conn) (int, error) { return 42, nil },
		Table: harness.Table{
			Process: func(pid int) (ptable.Proc, bool) {
				name := "sshd"
				if agent.Load() {
					name = "claude"
				}
				return ptable.Proc{PID: pid, PPID: 1, Name: name}, pid == 42
			},
			Argv: func(int) ([]string, error) { return nil, os.ErrPermission },
		},
	}
	id := wire.NewSessionID()
	done := make(chan runResult, 1)
	go func() {
		code, err := Run(context.Background(), Options{
			Command: []string{"sh", "-c", "read x"}, Detached: true, ID: id, Cols: 80, Rows: 24,
			Dir: t.TempDir(), Layout: l, Logf: func(string, ...any) {}, Guard: guard,
		})
		done <- runResult{code, err}
	}()

	ref := wire.SessionRef{ID: id}
	for _, c := range []struct {
		method string
		params any
	}{
		{wire.MethodSessionInfo, ref},
		{wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}},
		{wire.MethodSessionDetach, ref},
		{wire.MethodSessionInput, wire.InputParams{ID: id, Text: "x", Submit: true}},
		{wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 100, Rows: 30, Force: true}},
		{wire.MethodSessionScrollback, wire.ScrollbackParams{ID: id}},
		{wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: "interrupt"}},
	} {
		tc := dialHolder(t, l, id)
		if err := tc.call(t, c.method, c.params, nil); errCode(err) != wire.ErrAgentRefused {
			t.Fatalf("%s from an agent: %v, want %s", c.method, err, wire.ErrAgentRefused)
		}
		select {
		case <-tc.Done():
		case <-time.After(testTimeout):
			t.Fatalf("%s from an agent: connection not closed after the refusal", c.method)
		}
	}

	agent.Store(false)
	tc := dialHolder(t, l, id)
	if err := tc.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "x", Submit: true}, nil); err != nil {
		t.Fatalf("session.input from sshd's tree: %v", err)
	}
	if r := waitExit(t, done); r.code != 0 || r.err != nil {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}
