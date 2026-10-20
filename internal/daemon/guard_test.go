package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// guardAs is a guard that sees every connection come from a process named
// name (whose parent is pid 1).
func guardAs(name string) *harness.Guard {
	return &harness.Guard{
		PeerPID: func(net.Conn) (int, error) { return 42, nil },
		Table: harness.Table{
			Process: func(pid int) (ptable.Proc, bool) {
				if pid != 42 {
					return ptable.Proc{}, false
				}
				return ptable.Proc{PID: 42, PPID: 1, Name: name}, true
			},
			Argv: func(int) ([]string, error) { return nil, os.ErrPermission },
		},
	}
}

func errCode(err error) string {
	if we, ok := errors.AsType[*wire.Error](err); ok {
		return we.Code
	}
	return ""
}

// TestAgentRefusedMethods: a connection from a coding agent's process tree
// may report hooks and read (schedules and their runs, a schedule's
// preview included), but not change settings, presence or notifications,
// create, update, remove, pause, resume or run a schedule now, nor stop
// the daemon.
func TestAgentRefusedMethods(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{Guard: guardAs("claude")})
	c, _ := e.client()
	do := func(method string, params any) error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return c.Call(ctx, method, params, nil)
	}
	type rpcCall struct {
		method string
		params any
	}
	daily := wire.Schedule{Name: "n", Kind: wire.ScheduleKindCommand, Command: "true", Cwd: os.TempDir(),
		When: wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}, TZ: "UTC"}
	for _, m := range []rpcCall{
		{wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{}`)}},
		{wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}},
		{wire.MethodNotifyTest, nil},
		{wire.MethodDaemonShutdown, nil},
		{wire.MethodScheduleCreate, wire.ScheduleCreateParams{Schedule: daily}},
		{wire.MethodScheduleUpdate, wire.ScheduleUpdateParams{ID: "0123456789abcdef", Schedule: daily}},
		{wire.MethodScheduleRemove, wire.ScheduleRemoveParams{ID: "0123456789abcdef", StopRun: true}},
		{wire.MethodSchedulePause, wire.ScheduleRef{ID: "0123456789abcdef"}},
		{wire.MethodScheduleResume, wire.ScheduleRef{ID: "0123456789abcdef"}},
		{wire.MethodScheduleRunNow, wire.ScheduleRef{ID: "0123456789abcdef"}},
	} {
		if err := do(m.method, m.params); errCode(err) != wire.ErrAgentRefused {
			t.Errorf("%s from an agent: %v, want %s", m.method, err, wire.ErrAgentRefused)
		}
	}
	readers := []rpcCall{
		{wire.MethodPing, nil},
		{wire.MethodSessionsList, nil},
		{wire.MethodApprovalsList, nil},
		{wire.MethodConfigGet, nil},
		{wire.MethodDaemonStatus, nil},
		{wire.MethodConversationsList, wire.ConversationsListParams{}},
		{wire.MethodHookEvent, wire.HookEventParams{Harness: wire.HarnessClaude, Event: "Stop", SessionID: sid, Payload: json.RawMessage(`{"session_id":"conv-1"}`)}},
	}
	if schedulesServed {
		readers = append(readers,
			rpcCall{wire.MethodScheduleList, nil},
			rpcCall{wire.MethodScheduleRuns, wire.ScheduleRunsParams{ID: "0123456789abcdef"}},
			rpcCall{wire.MethodSchedulePreview, wire.ScheduleCreateParams{Schedule: daily}})
		readers = append(readers,
			rpcCall{wire.MethodScheduleOutput, wire.ScheduleOutputParams{ID: "0123456789abcdef", RunID: "0123456789abcdef"}})
	}
	for _, m := range readers {
		if err := do(m.method, m.params); errCode(err) == wire.ErrAgentRefused {
			t.Errorf("%s from an agent refused: %v", m.method, err)
		}
	}
	select {
	case <-e.d.Done():
		t.Fatal("the daemon shut down for an agent")
	default:
	}
}

func TestNonAgentMaySetConfig(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{Guard: guardAs("sshd")})
	c, _ := e.client()
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{}`)}, nil)
}

// An agent's process that keeps its connection until the agent exits (and
// is reparented to init) is still refused: the connection was decided when
// it was made.
func TestAgentRefusedAfterAgentExits(t *testing.T) {
	var agentGone atomic.Bool
	guard := &harness.Guard{
		PeerPID: func(net.Conn) (int, error) { return 42, nil },
		Table: harness.Table{
			Process: func(pid int) (ptable.Proc, bool) {
				switch {
				case pid == 42 && agentGone.Load():
					return ptable.Proc{PID: 42, PPID: 1, Name: "sh"}, true
				case pid == 42:
					return ptable.Proc{PID: 42, PPID: 7, Name: "sh"}, true
				case pid == 7 && !agentGone.Load():
					return ptable.Proc{PID: 7, PPID: 1, Name: "claude"}, true
				case pid == 1:
					return ptable.Proc{PID: 1, PPID: 0, Name: "systemd"}, true
				}
				return ptable.Proc{}, false
			},
			Argv: func(int) ([]string, error) { return nil, os.ErrPermission },
		},
	}
	e := startDaemon(t, fastConfig(), Options{Guard: guard})
	c, _ := e.client()
	call(t, c, wire.MethodPing, nil, nil)
	agentGone.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Call(ctx, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{}`)}, nil)
	if errCode(err) != wire.ErrAgentRefused {
		t.Fatalf("config.set after the agent exited: %v, want %s", err, wire.ErrAgentRefused)
	}
}
