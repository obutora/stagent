package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// taskRemoveWait bounds what task.remove does itself: the checks, and
	// a removal without archive (git worktree remove of a big worktree).
	taskRemoveWait = 10 * time.Minute
	// A stopped session gets stopHangupWait after hangup, then
	// stopKillWait after kill.
	stopHangupWait = 5 * time.Second
	stopKillWait   = 3 * time.Second
	stopPoll       = 100 * time.Millisecond
	// stopRounds bounds stopping sessions bound to the task while its
	// sessions were being stopped.
	stopRounds = 3
	// daemonCallWait bounds each daemon call of task.remove.
	daemonCallWait = 10 * time.Second
)

func (b *Bridge) taskRemove(m *wire.Msg) {
	var p wire.TaskRemoveParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	res, err := b.doTaskRemove(p)
	b.replyResult(m.ID, res, err)
}

// doTaskRemove is 片付け (ADR 0006), on its own queue. A dry run reports
// what is in the way. Otherwise: stop the task's live sessions, then —
// with scripts.archive in the source checkout's orca.yaml — run `stagent
// task remove` in a session of the source checkout and answer when it has
// finished; without archive remove here (task.Remove). Either way the
// record goes last (task.forget, naming the kept branch). An in_place or
// missing task only loses its record (missing: and git worktree prune).
func (b *Bridge) doTaskRemove(p wire.TaskRemoveParams) (*wire.TaskRemoveResult, error) {
	const method = wire.MethodTaskRemove
	if p.ID == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: id is empty", method)
	}
	// A coding agent's process tree removes nothing (ADR 0004).
	if werr := b.agentRefusal(); werr != nil {
		return nil, werr
	}
	env := b.sessionEnv()
	for _, name := range []string{"git", "gh"} {
		env = ensureOnPath(env, name, b.l.Home)
	}
	ctx, cancel := context.WithTimeout(b.ctx, taskRemoveWait)
	defer cancel()
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	// Daemon calls get their own deadline: a removal in a session can take
	// longer than ctx.
	call := func(method string, params any) error {
		cctx, cancel := context.WithTimeout(b.ctx, daemonCallWait)
		defer cancel()
		var none struct{}
		return dc.c.Call(cctx, method, params, &none)
	}
	progress := func(stage, msg string) error {
		return call(wire.MethodTaskProgress, wire.TaskProgressParams{ID: p.ID, Stage: stage, Error: msg})
	}
	// failed takes the task back to where it was, keeping the error.
	failed := func(stage string, err error) error {
		msg := oneLine(err.Error())
		if perr := progress(stage, msg); perr != nil {
			return perr
		}
		return wire.Errorf(wire.ErrInternal, "%s", msg)
	}

	t, err := findTask(ctx, dc, p.ID)
	if err != nil {
		return nil, err
	}
	if t.State == wire.TaskRemoving {
		return nil, wire.Errorf(wire.ErrBusy, "%s: task %s is being removed", method, t.ID)
	}
	// A preparing task is busy unless its sessions may be stopped: then
	// stopPreparing stops them, which fails it (interrupted), and the
	// removal goes on with the task as it is then.
	stopPreparing := func(live []string) error {
		left, err := b.stopAll(ctx, dc, t.ID, live)
		if err != nil {
			return err
		}
		if len(left) > 0 {
			return wire.Errorf(wire.ErrBusy, "%s: %d session(s) did not stop", method, len(left))
		}
		now, err := findTask(ctx, dc, t.ID)
		if err != nil {
			return err
		}
		if now.State == wire.TaskPreparing || now.State == wire.TaskRemoving {
			return wire.Errorf(wire.ErrBusy, "%s: task %s is still %s", method, now.ID, now.State)
		}
		t = now
		return nil
	}
	if t.InPlace || t.State == wire.TaskMissing {
		if p.DryRun {
			return &wire.TaskRemoveResult{}, nil
		}
		if t.State == wire.TaskPreparing {
			if !p.StopSessions {
				return nil, wire.Errorf(wire.ErrBusy, "%s: task %s is still preparing", method, t.ID)
			}
			live, err := liveSessions(ctx, dc, t.ID, nil)
			if err != nil {
				return nil, err
			}
			if err := stopPreparing(live); err != nil {
				return nil, err
			}
		}
		if !t.InPlace {
			task.Git(ctx, env, t.Repo, "worktree", "prune") // best effort: the record goes anyway
		}
		return &wire.TaskRemoveResult{}, call(wire.MethodTaskForget, wire.TaskRemoved{ID: t.ID})
	}

	live, err := liveSessions(ctx, dc, t.ID, nil)
	if err != nil {
		return nil, err
	}
	var changes []string
	listed, _ := task.Listed(ctx, env, t.Repo, t.Worktree)
	if listed {
		if changes, err = task.Changes(ctx, env, t.Worktree, task.ShareLinks(t.Repo, t.Worktree)); err != nil {
			return nil, err
		}
	}
	if p.DryRun {
		res := &wire.TaskRemoveResult{Changes: changes, Sessions: live}
		if len(changes) > wire.TaskRemoveChangesMax {
			res.Changes, res.ChangesTruncated = changes[:wire.TaskRemoveChangesMax], true
		}
		if res.Unpushed, err = task.Unpushed(ctx, env, t.Repo, t.Branch); err != nil {
			return nil, err
		}
		return res, nil
	}
	switch {
	case len(changes) > 0 && !p.Force:
		return nil, wire.Errorf(wire.ErrNotClean, "%s: %d uncommitted change(s) in %s", method, len(changes), t.Worktree)
	case t.State == wire.TaskPreparing && !p.StopSessions:
		return nil, wire.Errorf(wire.ErrBusy, "%s: task %s is still preparing", method, t.ID)
	case len(live) > 0 && !p.StopSessions:
		return nil, wire.Errorf(wire.ErrBusy, "%s: %d live session(s)", method, len(live))
	}

	// (1) stop. A preparing task enters removing only once its prepared
	// session has ended, which fails it (interrupted). Another removal
	// that got there first makes the stop stage busy.
	if t.State == wire.TaskPreparing {
		if err := stopPreparing(live); err != nil {
			return nil, err
		}
		if err := progress(wire.TaskStageStop, ""); err != nil {
			return nil, err
		}
	} else {
		if err := progress(wire.TaskStageStop, ""); err != nil {
			return nil, err
		}
		left, err := b.stopAll(ctx, dc, t.ID, live)
		if err == nil && len(left) > 0 {
			err = wire.Errorf(wire.ErrBusy, "%s: %d session(s) did not stop", method, len(left))
		}
		if err != nil {
			progress(wire.TaskStageStop, oneLine(err.Error()))
			return nil, err
		}
	}

	// (2) archive, in a session of the source checkout.
	if !p.SkipArchive && listed {
		o, err := task.LoadOrca(t.Repo)
		if err != nil {
			return nil, failed(wire.TaskStageArchive, err)
		}
		if o != nil && o.Archive != "" {
			return b.removeInSession(t, p.Force, env, failed)
		}
	}

	// (3)–(5) here: links, worktree, branch.
	kept, err := task.Remove(ctx, task.Removal{
		Task: *t, Force: p.Force, Env: env,
		Stage: func(stage string) { progress(stage, "") },
	})
	if err != nil {
		var se *task.StageError
		stage := wire.TaskStageWorktree
		if errors.As(err, &se) {
			stage, err = se.Stage, se.Err
		}
		return nil, failed(stage, err)
	}
	if err := call(wire.MethodTaskForget, wire.TaskRemoved{ID: t.ID, KeptBranch: kept}); err != nil {
		return nil, err
	}
	return &wire.TaskRemoveResult{KeptBranch: kept}, nil
}

// removeInSession runs `stagent task remove <id>` in a new session whose
// cwd is the source checkout (its log on that terminal) and answers once
// the removal has finished: the daemon's task.removed (with the kept
// branch), or the task leaving removing with an error. The bridge watches
// that on a connection of its own.
func (b *Bridge) removeInSession(t *wire.Task, force bool, env []string, failed func(string, error) error) (*wire.TaskRemoveResult, error) {
	conn, err := b.dialDaemon()
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	holder := wire.NewSessionID()
	type end struct {
		removed, ended bool
		kept, err      string
	}
	ends := make(chan end, 1)
	settle := func(e end) {
		select {
		case ends <- e:
		default: // the first one counts
		}
	}
	w := rpc.NewClient(conn, func(m *wire.Msg) {
		switch m.Method {
		case wire.NotifyTaskRemoved:
			var r wire.TaskRemoved
			if json.Unmarshal(m.Params, &r) == nil && r.ID == t.ID {
				settle(end{removed: true, kept: r.KeptBranch})
			}
		case wire.NotifyTaskUpdated:
			var u wire.Task
			if json.Unmarshal(m.Params, &u) == nil && u.ID == t.ID && u.State != wire.TaskRemoving {
				settle(end{err: cmp.Or(u.Error, "the removal stopped")})
			}
		case wire.NotifySessionUpdated:
			var s wire.Session
			if json.Unmarshal(m.Params, &s) == nil && s.ID == holder && ended(s) {
				settle(end{ended: true})
			}
		case wire.NotifySessionRemoved:
			var s wire.SessionRef
			if json.Unmarshal(m.Params, &s) == nil && s.ID == holder {
				settle(end{ended: true})
			}
		}
	})
	defer w.Close()
	ctx, cancel := context.WithTimeout(b.ctx, daemonStartWait)
	err = w.Call(ctx, wire.MethodWatch, wire.WatchParams{}, nil)
	cancel()
	if err != nil {
		return nil, err
	}

	exe, err := daemonclient.Exe()
	if err != nil {
		return nil, failed(wire.TaskStageArchive, err)
	}
	command := []string{exe, "task", "remove"}
	if force {
		command = append(command, "--force")
	}
	command = append(command, t.ID)
	if _, err := b.startSession(holder, command, t.Repo, defaultCols, defaultRows, env, ""); err != nil {
		return nil, failed(wire.TaskStageArchive, fmt.Errorf("start the session: %w", err))
	}

	var e end
	select {
	case e = <-ends:
	case <-w.Done():
		return nil, wire.Errorf(wire.ErrUnavailable, "daemon connection lost; the removal goes on in session %s", holder)
	case <-b.ctx.Done():
		return nil, wire.Errorf(wire.ErrUnavailable, "bridge closing; the removal goes on in session %s", holder)
	}
	switch {
	case e.removed:
		return &wire.TaskRemoveResult{KeptBranch: e.kept}, nil
	case e.err != "":
		return nil, wire.Errorf(wire.ErrInternal, "%s", e.err)
	}
	// The session ended with the task still removing.
	ctx, cancel = context.WithTimeout(b.ctx, daemonStartWait)
	defer cancel()
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	now, err := findTask(ctx, dc, t.ID)
	var we *wire.Error
	if errors.As(err, &we) && we.Code == wire.ErrNotFound {
		return &wire.TaskRemoveResult{}, nil // removed after all
	}
	if err != nil {
		return nil, err
	}
	if now.State != wire.TaskRemoving {
		return nil, wire.Errorf(wire.ErrInternal, "%s", cmp.Or(now.Error, "the removal stopped"))
	}
	return nil, failed(cmp.Or(now.Stage, wire.TaskStageArchive), errors.New("stagent task remove ended before finishing (see its session)"))
}

// stopAll stops the task's sessions ids, then those bound to the task
// meanwhile, for at most stopRounds rounds, and returns the ones still
// live.
func (b *Bridge) stopAll(ctx context.Context, dc *daemonConn, taskID string, ids []string) ([]string, error) {
	for round := 1; ; round++ {
		left, err := b.stopSessions(ctx, dc, taskID, ids)
		if err != nil || len(left) > 0 {
			return left, err
		}
		if ids, err = liveSessions(ctx, dc, taskID, nil); err != nil || len(ids) == 0 || round == stopRounds {
			return ids, err
		}
	}
}

// stopSessions hangs up the sessions ids, kills those still live after
// stopHangupWait and returns the ones still live stopKillWait after that.
func (b *Bridge) stopSessions(ctx context.Context, dc *daemonConn, taskID string, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	signal := func(ids []string, sig string) {
		for _, id := range ids {
			if c, err := b.holderConn(id); err == nil {
				var none struct{}
				c.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: sig}, &none)
			}
		}
	}
	await := func(ids []string, d time.Duration) ([]string, error) {
		deadline := time.Now().Add(d)
		for {
			left, err := liveSessions(ctx, dc, taskID, ids)
			if err != nil || len(left) == 0 || time.Now().After(deadline) {
				return left, err
			}
			select {
			case <-ctx.Done():
				return left, ctx.Err()
			case <-time.After(stopPoll):
			}
		}
	}
	signal(ids, wire.SignalHangup)
	left, err := await(ids, stopHangupWait)
	if err != nil || len(left) == 0 {
		return left, err
	}
	signal(left, wire.SignalKill)
	return await(left, stopKillWait)
}

// liveSessions lists the ids of the task's live sessions (agents and kept
// shells), only among ids when given.
func liveSessions(ctx context.Context, dc *daemonConn, taskID string, among []string) ([]string, error) {
	var list wire.SessionsListResult
	if err := dc.c.Call(ctx, wire.MethodSessionsList, struct{}{}, &list); err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range list.Sessions {
		if s.TaskID == taskID && !ended(s) && (among == nil || slices.Contains(among, s.ID)) {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

func ended(s wire.Session) bool {
	return s.ExitCode != nil || s.State == wire.StateExited
}

// findTask is task id as task.list reports it (not_found: none).
func findTask(ctx context.Context, dc *daemonConn, id string) (*wire.Task, error) {
	var list wire.TaskListResult
	if err := dc.c.Call(ctx, wire.MethodTaskList, struct{}{}, &list); err != nil {
		return nil, err
	}
	for i := range list.Tasks {
		if list.Tasks[i].ID == id {
			return &list.Tasks[i], nil
		}
	}
	return nil, wire.Errorf(wire.ErrNotFound, "task %s", id)
}

// oneLine joins the lines of an error message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
