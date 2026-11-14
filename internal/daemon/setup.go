package daemon

import (
	"encoding/json"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// setup の承認 (ADR 0023): `stagent task run` waits (task.await_setup) on
// a worktree's scripts.setup that is neither the source checkout's nor
// approved; the task shows it (Task.SetupPending) until the app runs or
// skips it (task.setup). Running it approves the content for the source
// checkout in <root>/state/setup-approvals.json, written by the daemon
// only and read by `stagent task run` (task.SetupApproved).

// setupWait is a task.await_setup waiting for its answer.
type setupWait struct {
	c    *rpc.Conn
	id   *int64
	hash string
}

// taskAwaitSetup answers at once when the content is approved for the
// task's source checkout, or task.setup decided it while the caller was
// away (its connection lost); otherwise the task shows the script and the
// call is answered when task.setup decides.
func (d *Daemon) taskAwaitSetup(c *rpc.Conn, id *int64, p wire.TaskAwaitSetupParams) (any, error) {
	if p.Script == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: script is empty", wire.MethodTaskAwaitSetup)
	}
	hash := task.SetupHash(p.Script)
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.taskLocked(p.ID)
	switch {
	case t == nil:
		return nil, wire.Errorf(wire.ErrNotFound, "task %s", p.ID)
	case t.State != wire.TaskPreparing:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: task %s is %s", wire.MethodTaskAwaitSetup, p.ID, t.State)
	}
	if dec, ok := d.setupDecided[t.ID]; ok && dec.Hash == hash {
		delete(d.setupDecided, t.ID)
		return wire.TaskAwaitSetupResult{Run: dec.Run}, nil
	}
	if task.SetupApproved(d.layout.SetupApprovals, t.Repo, hash) {
		return wire.TaskAwaitSetupResult{Run: true}, nil
	}
	snap := d.snapshotTasksLocked()
	t.Stage, t.SetupPending = wire.TaskStageSetup, &wire.TaskSetup{Script: p.Script, Hash: hash}
	if err := d.taskCommittedLocked(snap, t); err != nil {
		return nil, err
	}
	d.setupWaits[t.ID] = setupWait{c: c, id: id, hash: hash}
	return rpc.Async, nil
}

// taskSetup decides the scripts.setup task p.ID waits on, named by its
// hash: running it approves the content for the task's source checkout
// first. The decision answers the waiting task.await_setup, or is kept
// for the next one with that content.
func (d *Daemon) taskSetup(p wire.TaskSetupParams) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.taskLocked(p.ID)
	switch {
	case t == nil:
		return wire.Errorf(wire.ErrNotFound, "task %s", p.ID)
	case t.State != wire.TaskPreparing || t.SetupPending == nil:
		return wire.Errorf(wire.ErrBadRequest, "%s: task %s does not wait on a scripts.setup", wire.MethodTaskSetup, p.ID)
	case t.SetupPending.Hash != p.Hash:
		return wire.Errorf(wire.ErrBadRequest, "%s: task %s waits on another scripts.setup", wire.MethodTaskSetup, p.ID)
	}
	if p.Run {
		if err := d.approveSetupLocked(t.Repo, p.Hash); err != nil {
			return wire.Errorf(wire.ErrInternal, "setup approvals: %v", err)
		}
	}
	snap := d.snapshotTasksLocked()
	t.SetupPending = nil
	if err := d.taskCommittedLocked(snap, t); err != nil {
		return err
	}
	// Kept as well: a reply to a connection that is going away is lost.
	d.setupDecided[t.ID] = p
	if w, ok := d.setupWaits[t.ID]; ok {
		delete(d.setupWaits, t.ID)
		if w.hash == p.Hash {
			w.c.ReplyResult(w.id, wire.TaskAwaitSetupResult{Run: p.Run}, nil)
		}
	}
	return nil
}

// approveSetupLocked adds hash for repo to setup-approvals.json (0600,
// atomically).
func (d *Daemon) approveSetupLocked(repo, hash string) error {
	path := d.layout.SetupApprovals
	a, err := task.LoadSetupApprovals(path)
	if err != nil {
		return err
	}
	if a.Has(repo, hash) {
		return nil
	}
	a.Approved = append(a.Approved, task.SetupApproval{Repo: repo, Hash: hash, At: time.Now().UTC().Truncate(time.Second)})
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, b)
}

// dropSetupLocked forgets what task id waited on: it is no longer
// preparing at that point (it failed, was interrupted, moved on or went),
// or is prepared again.
func (d *Daemon) dropSetupLocked(t *wire.Task) {
	t.SetupPending = nil
	delete(d.setupWaits, t.ID)
	delete(d.setupDecided, t.ID)
}
