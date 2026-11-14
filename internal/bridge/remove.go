package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// taskRemoveWait bounds what task.remove does itself: the checks and
	// stopping the sessions. The removal goes on in a session.
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
	res, _, err := b.doTaskRemove(p, "")
	b.replyResult(m.ID, res, err)
}

// doTaskRemove is 片付け (ADR 0006), on its own queue. A dry run reports
// what is in the way. Otherwise: stop the task's live sessions, then run
// `stagent task remove` — scripts.archive of the source checkout's
// orca.yaml (unless skipped), the links, the worktree, the branch, the
// record (task.forget, naming the kept branch) — in a session of the
// source checkout (removeInSession), so that it goes on when the app's
// connection, and with it the bridge, goes. A missing task's removal runs
// there too (`stagent task remove --missing`): its own worktree
// unregistered when its directory is gone, the branch stage, the record
// (task.RemoveMissing). An in_place task only loses its record;
// one whose source checkout and worktree are both gone its record, its
// branch named as kept: no git can run there. With folder (the worktree
// folder: worktree.remove) the session also removes the worktree's parent
// when it is directly in folder and left empty, and that parent is
// returned (task.RemoveEmptyParent).
func (b *Bridge) doTaskRemove(p wire.TaskRemoveParams, folder string) (*wire.TaskRemoveResult, string, error) {
	const method = wire.MethodTaskRemove
	if p.ID == "" {
		return nil, "", wire.Errorf(wire.ErrBadRequest, "%s: id is empty", method)
	}
	// A coding agent's process tree removes nothing (ADR 0004).
	if werr := b.agentRefusal(); werr != nil {
		return nil, "", werr
	}
	env := b.sessionEnv()
	for _, name := range []string{"git", "gh"} {
		env = ensureOnPath(env, name, b.l.Home)
	}
	ctx, cancel := context.WithTimeout(b.ctx, taskRemoveWait)
	defer cancel()
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, "", upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
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
	// handOff hands the removal to the session it goes on in (see
	// removeInSession).
	handOff := func(stage, session string) error {
		return call(wire.MethodTaskProgress, wire.TaskProgressParams{ID: p.ID, Stage: stage, SessionID: session})
	}

	t, err := findTask(ctx, dc, p.ID)
	if err != nil {
		return nil, "", err
	}
	if t.State == wire.TaskRemoving {
		return nil, "", wire.Errorf(wire.ErrBusy, "%s: task %s is being removed", method, t.ID)
	}
	// A preparing task is busy unless its sessions may be stopped: then
	// stopPreparing stops them, which fails it (interrupted), and the
	// removal goes on with the task as it is then.
	stopPreparing := func(live []string) error {
		left, err := b.stopAll(ctx, dc, ofTask(t.ID), live)
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
	// Removed by hand: neither the source checkout nor the worktree is
	// there, so no git can run and nothing but the record is left; the
	// branch, wherever the checkout went, is not deleted.
	gone := !t.InPlace && !task.IsDir(t.Repo) && !task.IsDir(t.Worktree)
	if t.InPlace || t.State == wire.TaskMissing || gone {
		if p.DryRun {
			return &wire.TaskRemoveResult{}, "", nil
		}
		if t.State == wire.TaskPreparing {
			if !p.StopSessions {
				return nil, "", wire.Errorf(wire.ErrBusy, "%s: task %s is still preparing", method, t.ID)
			}
			live, err := liveSessions(ctx, dc, ofTask(t.ID), nil)
			if err != nil {
				return nil, "", err
			}
			if err := stopPreparing(live); err != nil {
				return nil, "", err
			}
		}
		var kept string
		switch {
		case t.InPlace:
		case !task.IsDir(t.Repo):
			kept = t.Branch // no git can run there to check it
		default:
			// Missing: in a session too, so that the connection going
			// does not stop it.
			if err := progress(wire.TaskStageStop, ""); err != nil {
				return nil, "", err
			}
			return b.removeInSession(t, []string{"--missing"}, wire.TaskStageWorktree, folder, env, handOff, failed)
		}
		return &wire.TaskRemoveResult{KeptBranch: kept}, "", call(wire.MethodTaskForget, wire.TaskRemoved{ID: t.ID, KeptBranch: kept})
	}

	live, err := liveSessions(ctx, dc, ofTask(t.ID), nil)
	if err != nil {
		return nil, "", err
	}
	ws, err := task.ListWorktrees(ctx, env, t.Repo)
	if err != nil {
		return nil, "", wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
	}
	// A worktree a killed `git worktree add` left half made has nothing of
	// the user's (git status would list every file deleted) and nothing to
	// archive; task.Remove takes it despite the add's lock.
	var changes []string
	w := task.FindWorktree(ws, t.Worktree)
	listed := w != nil && !w.HalfMade()
	if listed {
		if changes, err = task.Changes(ctx, env, t.Worktree, task.ShareLinks(t.Repo, t.Worktree)); err != nil {
			return nil, "", err
		}
	}
	refusals, err := task.Refusals(ctx, env, ws, t.Worktree, true)
	if err != nil {
		return nil, "", wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
	}
	if p.DryRun {
		res := &wire.TaskRemoveResult{Refusals: refusals, Changes: changes, Sessions: live}
		if len(changes) > wire.TaskRemoveChangesMax {
			res.Changes, res.ChangesTruncated = changes[:wire.TaskRemoveChangesMax], true
		}
		if res.Unpushed, err = task.Unpushed(ctx, env, t.Repo, t.Branch); err != nil {
			return nil, "", err
		}
		return res, "", nil
	}
	switch {
	case len(refusals) > 0:
		return nil, "", refusedError(method, t.Worktree, refusals)
	case len(changes) > 0 && !p.Force:
		return nil, "", wire.Errorf(wire.ErrNotClean, "%s: %d uncommitted change(s) in %s", method, len(changes), t.Worktree)
	case t.State == wire.TaskPreparing && !p.StopSessions:
		return nil, "", wire.Errorf(wire.ErrBusy, "%s: task %s is still preparing", method, t.ID)
	case len(live) > 0 && !p.StopSessions:
		return nil, "", wire.Errorf(wire.ErrBusy, "%s: %d live session(s)", method, len(live))
	}

	// (1) stop. A preparing task enters removing only once its prepared
	// session has ended, which fails it (interrupted). Another removal
	// that got there first makes the stop stage busy.
	if t.State == wire.TaskPreparing {
		if err := stopPreparing(live); err != nil {
			return nil, "", err
		}
		if err := progress(wire.TaskStageStop, ""); err != nil {
			return nil, "", err
		}
	} else {
		if err := progress(wire.TaskStageStop, ""); err != nil {
			return nil, "", err
		}
		left, err := b.stopAll(ctx, dc, ofTask(t.ID), live)
		if err == nil && len(left) > 0 {
			err = wire.Errorf(wire.ErrBusy, "%s: %d session(s) did not stop", method, len(left))
		}
		if err != nil {
			progress(wire.TaskStageStop, oneLine(err.Error()))
			return nil, "", err
		}
	}

	// (2)–(5) in a session of the source checkout: archive (not when git
	// no longer lists the worktree, or it is half made), links, worktree,
	// branch, record.
	flags, stage := []string{}, wire.TaskStageArchive
	if p.Force {
		flags = append(flags, "--force")
	}
	if p.SkipArchive || !listed {
		flags, stage = append(flags, "--skip-archive"), wire.TaskStageWorktree
	}
	return b.removeInSession(t, flags, stage, folder, env, handOff, failed)
}

// removeInSession runs `stagent task remove <flags…> <id>` in a new
// session whose cwd is the source checkout (its log on that terminal; see
// runRemoval), having handed the removal to that session first (handOff:
// task.progress of the session's first stage, stage, with its
// session_id): it goes on when the bridge goes, and the daemon takes the
// task back (interrupted) when the session ends before it finished. It
// answers once the removal is over: the daemon's task.removed (with the
// kept branch), or the task leaving removing with an error — refused when
// the removal's own check found refusals (they came after the bridge's).
// With folder the session removes the worktree's empty parent there, which
// is returned.
func (b *Bridge) removeInSession(t *wire.Task, flags []string, stage, folder string, env []string, handOff func(stage, session string) error, failed func(string, error) error) (*wire.TaskRemoveResult, string, error) {
	if folder != "" {
		flags = append(flags, "--folder", folder)
	}
	match := func(m *wire.Msg) (removalEnd, bool) {
		switch m.Method {
		case wire.NotifyTaskRemoved:
			var r wire.TaskRemoved
			if json.Unmarshal(m.Params, &r) == nil && r.ID == t.ID {
				return removalEnd{removed: true, kept: r.KeptBranch}, true
			}
		case wire.NotifyTaskUpdated:
			var u wire.Task
			if json.Unmarshal(m.Params, &u) == nil && u.ID == t.ID && u.State != wire.TaskRemoving {
				return removalEnd{err: cmp.Or(u.Error, "the removal stopped")}, true
			}
		}
		return removalEnd{}, false
	}
	e, r, err := b.runRemoval("remove", flags, []string{t.ID}, t.Repo, env, match,
		func(holder string) error { return handOff(stage, holder) },
		func(err error) error { return failed(stage, err) })
	if err != nil {
		return nil, "", err
	}
	var parent string
	if r != nil {
		if len(r.Refusals) > 0 {
			return nil, "", refusedError(wire.MethodTaskRemove, t.Worktree, r.Refusals)
		}
		parent = r.RemovedParent
	}
	switch {
	case e.removed:
		return &wire.TaskRemoveResult{KeptBranch: e.kept}, parent, nil
	case e.err != "":
		return nil, "", wire.Errorf(wire.ErrInternal, "%s", e.err)
	}
	// The session ended first: the task is gone, or back where it was.
	ctx, cancel := context.WithTimeout(b.ctx, daemonStartWait)
	defer cancel()
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, "", upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	now, err := findTask(ctx, dc, t.ID)
	var we *wire.Error
	if errors.As(err, &we) && we.Code == wire.ErrNotFound {
		return &wire.TaskRemoveResult{}, parent, nil // removed after all
	}
	if err != nil {
		return nil, "", err
	}
	if now.State != wire.TaskRemoving {
		return nil, "", wire.Errorf(wire.ErrInternal, "%s", cmp.Or(now.Error, "the removal stopped"))
	}
	return nil, "", failed(cmp.Or(now.Stage, stage), errors.New("stagent task remove ended before finishing (see its session)"))
}

// runRemoval runs `<stagent> task <sub> <flags…> --result <file>
// <operands…>` in a new session holder whose cwd is the source checkout
// cwd (Windows cannot remove a worktree that is a process's cwd), handing
// the removal over first (handOff, when set, told holder), and waits for
// its end (watchRemoval with match). The session goes on when the bridge
// goes. Its result comes back in <file> (nil: the session wrote none), a
// file of the run directory that runRemoval makes, waits for and deletes;
// once the bridge no longer waits the file is gone and the session writes
// none. A failure before the session started is fail(err).
func (b *Bridge) runRemoval(sub string, flags, operands []string, cwd string, env []string, match func(*wire.Msg) (removalEnd, bool), handOff func(holder string) error, fail func(error) error) (removalEnd, *task.RemovalResult, error) {
	holder := wire.NewSessionID()
	exe, err := daemonclient.Exe()
	if err != nil {
		return removalEnd{}, nil, fail(err)
	}
	result := filepath.Join(b.l.RunDir, "remove-"+holder+".json")
	command := append(append([]string{exe, "task", sub}, flags...), "--result", result)
	command = append(command, operands...)
	w, ends, err := b.watchRemoval(holder, match)
	if err != nil {
		return removalEnd{}, nil, fail(err)
	}
	defer w.Close()
	if err := os.WriteFile(result, nil, 0o600); err != nil {
		return removalEnd{}, nil, fail(err)
	}
	defer os.Remove(result)
	if handOff != nil {
		if err := handOff(holder); err != nil {
			return removalEnd{}, nil, fail(err)
		}
	}
	if _, err := b.startSession(holder, command, cwd, defaultCols, defaultRows, env, ""); err != nil {
		return removalEnd{}, nil, fail(fmt.Errorf("start the session: %w", err))
	}
	e, err := b.awaitRemoval(w, ends, holder)
	if err != nil {
		return e, nil, err
	}
	var r task.RemovalResult
	if data, err := os.ReadFile(result); err != nil || json.Unmarshal(data, &r) != nil {
		return e, nil, nil
	}
	return e, &r, nil
}

// removalEnd is how a removal in a session ended: removed (keeping the
// branch kept), failed (err), or the session ended (ended).
type removalEnd struct {
	removed, ended bool
	kept, err      string
}

// watchRemoval watches, on a daemon connection of its own, for the end of
// the removal running in session holder: the session's end, or what match
// (nil: nothing) finds in the daemon's other notifications. The first one
// counts.
func (b *Bridge) watchRemoval(holder string, match func(*wire.Msg) (removalEnd, bool)) (*rpc.Client, <-chan removalEnd, error) {
	conn, err := b.dialDaemon()
	if err != nil {
		return nil, nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	ends := make(chan removalEnd, 1)
	settle := func(e removalEnd) {
		select {
		case ends <- e:
		default: // the first one counts
		}
	}
	w := rpc.NewClient(conn, func(m *wire.Msg) {
		switch m.Method {
		case wire.NotifySessionUpdated:
			var s wire.Session
			if json.Unmarshal(m.Params, &s) == nil && s.ID == holder && ended(s) {
				settle(removalEnd{ended: true})
			}
		case wire.NotifySessionRemoved:
			var s wire.SessionRef
			if json.Unmarshal(m.Params, &s) == nil && s.ID == holder {
				settle(removalEnd{ended: true})
			}
		default:
			if match == nil {
				return
			}
			if e, ok := match(m); ok {
				settle(e)
			}
		}
	})
	ctx, cancel := context.WithTimeout(b.ctx, daemonStartWait)
	err = w.Call(ctx, wire.MethodWatch, wire.WatchParams{}, nil)
	cancel()
	if err != nil {
		w.Close()
		return nil, nil, err
	}
	return w, ends, nil
}

// awaitRemoval waits for the end of the removal in session holder (ends,
// see watchRemoval). The daemon connection w or the bridge going first is
// unavailable: the removal goes on in its session.
func (b *Bridge) awaitRemoval(w *rpc.Client, ends <-chan removalEnd, holder string) (removalEnd, error) {
	select {
	case e := <-ends:
		return e, nil
	case <-w.Done():
		return removalEnd{}, wire.Errorf(wire.ErrUnavailable, "daemon connection lost; the removal goes on in session %s", holder)
	case <-b.ctx.Done():
		return removalEnd{}, wire.Errorf(wire.ErrUnavailable, "bridge closing; the removal goes on in session %s", holder)
	}
}

// stopAll stops the sessions ids, then those of (see liveSessions) that
// came meanwhile, for at most stopRounds rounds, and returns the ones
// still live.
func (b *Bridge) stopAll(ctx context.Context, dc *daemonConn, of func(wire.Session) bool, ids []string) ([]string, error) {
	for round := 1; ; round++ {
		left, err := b.stopSessions(ctx, dc, of, ids)
		if err != nil || len(left) > 0 {
			return left, err
		}
		if ids, err = liveSessions(ctx, dc, of, nil); err != nil || len(ids) == 0 || round == stopRounds {
			return ids, err
		}
	}
}

// stopSessions hangs up the sessions ids, kills those still live after
// stopHangupWait and returns the ones still live stopKillWait after that.
func (b *Bridge) stopSessions(ctx context.Context, dc *daemonConn, of func(wire.Session) bool, ids []string) ([]string, error) {
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
			left, err := liveSessions(ctx, dc, of, ids)
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

// liveSessions lists the ids of the live sessions (agents and kept
// shells) of reports as its own — a task's (ofTask), a worktree's —, only
// among ids when given.
func liveSessions(ctx context.Context, dc *daemonConn, of func(wire.Session) bool, among []string) ([]string, error) {
	var list wire.SessionsListResult
	if err := dc.c.Call(ctx, wire.MethodSessionsList, struct{}{}, &list); err != nil {
		return nil, err
	}
	var ids []string
	for _, s := range list.Sessions {
		if of(s) && !ended(s) && (among == nil || slices.Contains(among, s.ID)) {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

// ofTask tells the sessions of task id (Session.task_id).
func ofTask(id string) func(wire.Session) bool {
	return func(s wire.Session) bool { return s.TaskID == id }
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

// refusedError is the refused answer of removing worktree for refusals.
func refusedError(method, worktree string, refusals []wire.RemoveRefusal) *wire.Error {
	return wire.Errorf(wire.ErrRefused, "%s: %s", method, (&task.RefusedError{Worktree: worktree, Refusals: refusals}).Error())
}
