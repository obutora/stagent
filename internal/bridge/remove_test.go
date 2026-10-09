//go:build !windows

package bridge

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// removeDaemon is a fake daemon for task.remove: one task, its sessions
// (each with a fake holder whose hangup ends it, then runs onHangup), the
// stages reported and whether the record went.
type removeDaemon struct {
	t        *testing.T
	l        *paths.Layout
	mu       sync.Mutex
	task     wire.Task
	sessions []wire.Session
	signaled []string
	stages   []wire.TaskProgressParams
	forgot   bool
	stopErr  error // the answer to task.progress stop
	onHangup func(d *removeDaemon, id string)
}

func startRemoveDaemon(t *testing.T, tk wire.Task) (*removeDaemon, *app) {
	t.Helper()
	l, a := startBridge(t)
	d := &removeDaemon{t: t, l: l, task: tk}
	serveFake(t, l.DaemonAddr, d.handle)
	return d, a
}

// addSession adds a live session of the task, its holder answering
// session.signal (locked by the caller or before the bridge runs).
func (d *removeDaemon) addSession(id string) {
	d.sessions = append(d.sessions, wire.Session{ID: id, TaskID: d.task.ID, State: wire.StateIdle})
	serveFake(d.t, d.l.HolderAddr(id), func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method == wire.MethodSessionSignal {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.signaled = append(d.signaled, id)
			for i := range d.sessions {
				if d.sessions[i].ID == id {
					code := 0
					d.sessions[i].ExitCode, d.sessions[i].State = &code, wire.StateExited
				}
			}
			if d.onHangup != nil {
				d.onHangup(d, id)
			}
		}
		return struct{}{}, nil
	})
}

func (d *removeDaemon) handle(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch m.Method {
	case wire.MethodTaskList:
		if d.forgot {
			return wire.TaskListResult{Tasks: []wire.Task{}}, nil
		}
		return wire.TaskListResult{Tasks: []wire.Task{d.task}}, nil
	case wire.MethodSessionsList:
		return wire.SessionsListResult{Sessions: slices.Clone(d.sessions)}, nil
	case wire.MethodTaskProgress:
		var p wire.TaskProgressParams
		rpc.Decode(m, &p)
		if p.Stage == wire.TaskStageStop && p.Error == "" && d.stopErr != nil {
			return nil, d.stopErr
		}
		d.stages = append(d.stages, p)
	case wire.MethodTaskForget:
		d.forgot = true
	}
	return struct{}{}, nil
}

// A preparing in_place task is busy unless its sessions may be stopped;
// with stop_sessions they are — one bound to the task meanwhile too —
// and the record goes once the task failed.
func TestTaskRemovePreparingInPlace(t *testing.T) {
	const prep, late = "00000000000000a1", "00000000000000a2"
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: "/w/app", Worktree: "/w/app", InPlace: true, State: wire.TaskPreparing, SessionID: prep})
	d.addSession(prep)
	d.onHangup = func(d *removeDaemon, id string) {
		if id == prep {
			d.task.State, d.task.Error = wire.TaskFailed, wire.TaskErrInterrupted
			d.addSession(late)
		}
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("task.remove of a preparing task: %v, want %s", e, wire.ErrBusy)
	}
	if d.forgot || len(d.signaled) != 0 {
		t.Fatalf("busy removal forgot %v, signaled %v", d.forgot, d.signaled)
	}
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1", StopSessions: true}, nil); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.forgot || !slices.Equal(d.signaled, []string{prep, late}) {
		t.Fatalf("forgot %v, signaled %v", d.forgot, d.signaled)
	}
}

// Another removal that got to the task first (the daemon answers stop
// busy) makes task.remove busy, and the record stays.
func TestTaskRemoveRacedIsBusy(t *testing.T) {
	repo := checkout(t)
	d, a := startRemoveDaemon(t, wire.Task{ID: "t1", Repo: repo, Worktree: repo + "-1", Branch: "1-x", State: wire.TaskReady})
	d.stopErr = wire.Errorf(wire.ErrBusy, "task t1 is being removed")
	if e := a.call(t, wire.MethodTaskRemove, wire.TaskRemoveParams{ID: "t1"}, nil); e == nil || e.Code != wire.ErrBusy {
		t.Fatalf("task.remove raced: %v, want %s", e, wire.ErrBusy)
	}
	if d.forgot || len(d.stages) != 0 {
		t.Fatalf("forgot %v, stages %+v", d.forgot, d.stages)
	}
}

// A coding agent's process tree removes nothing, not even a dry run: the
// daemon is not asked and nothing starts.
func TestTaskRemoveRefusedToAgents(t *testing.T) {
	d := &taskDaemon{tasks: []wire.Task{{ID: "t1", Repo: "/w/app", Worktree: "/w/app-1", State: wire.TaskReady}}}
	a, starts := startTaskBridge(t, d, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "from claude") }
	})
	for _, p := range []wire.TaskRemoveParams{{ID: "t1", DryRun: true}, {ID: "t1", Force: true, StopSessions: true}} {
		if e := a.call(t, wire.MethodTaskRemove, p, nil); e == nil || e.Code != wire.ErrAgentRefused {
			t.Fatalf("task.remove %+v from an agent: %v", p, e)
		}
	}
	if len(d.calls) != 0 || len(starts()) != 0 {
		t.Fatalf("daemon calls %v, starts %v", d.calls, starts())
	}
}
