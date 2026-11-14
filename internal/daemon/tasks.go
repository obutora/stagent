package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// Tasks (タスク) live in <root>/state/tasks.json, written by the daemon
// only. A task's key is its source checkout (元のチェックアウト, canonical)
// and number, or name for a named task (task.SameKey). The bridge
// registers one (task.register) before preparing it, `stagent task run`
// reports its stages (task.progress), and task.list, watch and
// task.updated / task.removed show them to the app.

const (
	// defaultPrepareGrace is how long a preparing task waits for its
	// prepared session to register (after task.register, or after a daemon
	// restart for its holder to come back) before it counts as interrupted,
	// and a removal handed to a session for that session to register.
	defaultPrepareGrace = 30 * time.Second
	// worktreeCheckTimeout bounds the git calls of one task.list.
	worktreeCheckTimeout = 10 * time.Second
	// taskErrorMax bounds Task.Error (one line).
	taskErrorMax = 300
)

// tasksFile is the document stored in tasks.json.
type tasksFile struct {
	Tasks []*wire.Task `json:"tasks"`
}

// loadTasks reads tasks.json (none: no tasks).
func loadTasks(path string) ([]*wire.Task, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f tasksFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(f.Tasks, func(t *wire.Task) bool { return t == nil || t.ID == "" }), nil
}

// saveTasksLocked writes tasks.json (0600, atomically).
func (d *Daemon) saveTasksLocked() error {
	b, err := json.MarshalIndent(tasksFile{Tasks: d.tasks}, "", "  ")
	if err == nil {
		err = fsutil.WriteFileAtomic(d.layout.Tasks, b)
	}
	return err
}

// taskChangedLocked stores the tasks and pushes t to watchers: a change
// the daemon makes itself (an interrupted preparation or removal), kept
// in memory even when tasks.json cannot be written.
func (d *Daemon) taskChangedLocked(t *wire.Task) {
	if err := d.saveTasksLocked(); err != nil {
		d.logf("daemon: tasks: %v", err)
	}
	d.broadcastLocked(wire.NotifyTaskUpdated, *t)
}

// tasksSnapshot is the task state an RPC may change, taken before it does
// so that a change tasks.json does not take can be undone.
type tasksSnapshot struct {
	tasks        []wire.Task
	prepAt       map[string]time.Time
	removingFrom map[string]string
	removers     map[string]remover
}

func (d *Daemon) snapshotTasksLocked() tasksSnapshot {
	return tasksSnapshot{
		tasks: d.taskListLocked(), prepAt: maps.Clone(d.prepAt),
		removingFrom: maps.Clone(d.removingFrom), removers: maps.Clone(d.removers),
	}
}

// commitTasksLocked stores the tasks an RPC changed. When tasks.json
// cannot be written the change is undone (snap) and the RPC fails
// (internal) without telling watchers.
func (d *Daemon) commitTasksLocked(snap tasksSnapshot) error {
	err := d.saveTasksLocked()
	if err == nil {
		return nil
	}
	d.tasks = make([]*wire.Task, len(snap.tasks))
	for i := range snap.tasks {
		d.tasks[i] = &snap.tasks[i]
	}
	d.prepAt, d.removingFrom, d.removers = snap.prepAt, snap.removingFrom, snap.removers
	return wire.Errorf(wire.ErrInternal, "tasks: %v", err)
}

// taskCommittedLocked stores the tasks an RPC changed (commitTasksLocked)
// and pushes t to watchers.
func (d *Daemon) taskCommittedLocked(snap tasksSnapshot, t *wire.Task) error {
	if err := d.commitTasksLocked(snap); err != nil {
		return err
	}
	d.broadcastLocked(wire.NotifyTaskUpdated, *t)
	return nil
}

func (d *Daemon) taskLocked(id string) *wire.Task {
	for _, t := range d.tasks {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// taskListLocked lists the tasks in the order they were created.
func (d *Daemon) taskListLocked() []wire.Task {
	out := make([]wire.Task, 0, len(d.tasks))
	for _, t := range d.tasks {
		out = append(out, *t)
	}
	return out
}

// ---------------------------------------------------------------------------
// Interrupted preparations and removals

// preparingLocked puts t in preparing as of now and arranges the check
// that its prepared session registers.
func (d *Daemon) preparingLocked(t *wire.Task, now time.Time) {
	t.State, t.Stage, t.Error = wire.TaskPreparing, "", ""
	d.dropSetupLocked(t)
	d.prepAt[t.ID] = now
	d.scheduleReviewLocked(now.Add(d.opts.PrepareGrace))
}

// remover is who carries a removal on: the connection that started it
// (the bridge stopping the task's sessions, a `stagent task remove` run by
// hand) until it is handed to a session (task.progress with session_id:
// the session `stagent task remove` runs in, as of at).
type remover struct {
	conn    *connState
	session string
	at      time.Time
}

// reviewTasksLocked fails (error interrupted) every preparing task whose
// prepared session ended, or did not register within PrepareGrace of the
// task's preparation (or of the daemon's start), and ends every removal
// whose remover went (removerGoneLocked). It is run by task.list, watch,
// task.register, at a session's end, when a connection closes and by a
// timer.
func (d *Daemon) reviewTasksLocked(now time.Time) {
	for _, t := range d.tasks {
		if t.State == wire.TaskRemoving {
			if d.removerGoneLocked(t.ID, now) {
				d.interruptRemovalLocked(t)
			}
			continue
		}
		if t.State != wire.TaskPreparing {
			continue
		}
		s := d.sessions[t.SessionID]
		if s != nil && !s.ended {
			continue
		}
		if s == nil {
			if due := d.prepAt[t.ID].Add(d.opts.PrepareGrace); now.Before(due) {
				d.scheduleReviewLocked(due)
				continue
			}
		}
		t.State, t.Error = wire.TaskFailed, wire.TaskErrInterrupted
		d.dropSetupLocked(t)
		delete(d.prepAt, t.ID)
		d.taskChangedLocked(t)
	}
}

// removerGoneLocked tells whether the remover of removing task id went:
// its connection closed, or its session ended or did not register within
// PrepareGrace of the hand-off.
func (d *Daemon) removerGoneLocked(id string, now time.Time) bool {
	r, ok := d.removers[id]
	switch {
	case !ok:
		return true
	case r.session == "":
		_, open := d.conns[r.conn]
		return !open
	}
	if s := d.sessions[r.session]; s != nil {
		return s.ended
	}
	if due := r.at.Add(d.opts.PrepareGrace); now.Before(due) {
		d.scheduleReviewLocked(due)
		return false
	}
	return true
}

// interruptRemovalLocked ends removing task t whose remover went: it is
// back where the removal started (ready), with error interrupted, keeping
// the stage it got to.
func (d *Daemon) interruptRemovalLocked(t *wire.Task) {
	d.endRemovalLocked(t, wire.TaskErrInterrupted)
	d.taskChangedLocked(t)
}

// endRemovalLocked takes removing task t back where its removal started
// (ready after a restart), with error msg, forgetting who carried it on.
func (d *Daemon) endRemovalLocked(t *wire.Task, msg string) {
	t.State, t.Error = cmp.Or(d.removingFrom[t.ID], wire.TaskReady), msg
	delete(d.removingFrom, t.ID)
	delete(d.removers, t.ID)
}

// scheduleReviewLocked runs reviewTasksLocked at (or before) at.
func (d *Daemon) scheduleReviewLocked(at time.Time) {
	if d.closed || d.review != nil && !d.reviewAt.After(at) {
		return
	}
	if d.review != nil {
		d.review.Stop()
	}
	d.reviewAt = at
	d.review = time.AfterFunc(time.Until(at), func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.closed {
			return
		}
		d.review = nil
		d.reviewTasksLocked(time.Now())
	})
}

// ---------------------------------------------------------------------------
// task.register

func (d *Daemon) taskRegister(p wire.TaskRegisterParams) (wire.TaskRegisterResult, error) {
	named := p.Kind == wire.TaskKindName
	switch {
	case !filepath.IsAbs(p.Repo):
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: repo must be an absolute path")
	case p.Kind != wire.TaskKindIssue && p.Kind != wire.TaskKindPR && !named:
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: kind must be issue, pr or name")
	case named && (p.Name == "" || p.Number != 0 || p.InPlace):
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: a named task has a name, no number and is not in_place")
	case !named && (p.Number <= 0 || p.Name != ""):
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: number must be positive, without a name")
	case p.SessionID == "":
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: session_id required")
	}
	repo, worktree := filepath.Clean(p.Repo), filepath.Clean(p.Worktree)
	if p.InPlace {
		worktree = repo
	} else if !filepath.IsAbs(p.Worktree) || task.SamePath(worktree, repo) {
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: worktree must be an absolute path other than repo")
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reviewTasksLocked(now)
	snap := d.snapshotTasksLocked()
	for _, t := range d.tasks {
		if !task.SameKey(t, repo, p.Kind, p.Number, p.Name) {
			continue
		}
		switch t.State {
		case wire.TaskPreparing:
			res := wire.TaskRegisterResult{Task: *t, Existed: true}
			if s := d.sessions[t.SessionID]; s != nil && !s.ended {
				ss := s.s
				res.Session = &ss
			}
			return res, nil
		case wire.TaskFailed:
			// Prepared again: the stages are safe to run twice.
			t.SessionID, t.Title, t.URL = p.SessionID, p.Title, p.URL
			if p.Branch != "" {
				t.Branch = p.Branch
			}
			if p.Base != "" {
				t.Base = p.Base
			}
			d.preparingLocked(t, now)
			if err := d.taskCommittedLocked(snap, t); err != nil {
				return wire.TaskRegisterResult{}, err
			}
			d.rebindLocked()
			return wire.TaskRegisterResult{Task: *t}, nil
		}
		return wire.TaskRegisterResult{Task: *t, Existed: true}, nil
	}
	if p.InPlace {
		for _, t := range d.tasks {
			if t.InPlace && task.SamePath(t.Repo, repo) {
				return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrInUse, "the checkout is used by the task of #%d", t.Number)
			}
		}
	}
	t := &wire.Task{
		ID: task.NewID(), Repo: repo, Kind: p.Kind, Number: p.Number, Name: p.Name, Title: p.Title, URL: p.URL,
		Branch: p.Branch, Base: p.Base, Worktree: worktree, InPlace: p.InPlace,
		CreatedAt: now.UTC().Truncate(time.Second), SessionID: p.SessionID,
	}
	d.tasks = append(d.tasks, t)
	d.preparingLocked(t, now)
	if err := d.taskCommittedLocked(snap, t); err != nil {
		return wire.TaskRegisterResult{}, err
	}
	d.rebindLocked()
	return wire.TaskRegisterResult{Task: *t}, nil
}

// ---------------------------------------------------------------------------
// task.progress

// taskProgress reports a stage of task p.ID from connection cs. A removal
// is carried on by cs when cs starts it, and by session p.SessionID once a
// removal stage names one (see remover).
func (d *Daemon) taskProgress(cs *connState, p wire.TaskProgressParams) error {
	creating, removing := false, false
	switch p.Stage {
	case wire.TaskStageFetch, wire.TaskStageCopy, wire.TaskStageSetup, wire.TaskStageAgent:
		creating = true
	case wire.TaskStageStop, wire.TaskStageArchive:
		removing = true
	case wire.TaskStageBranch, wire.TaskStageWorktree:
		creating, removing = true, true
	default:
		return wire.Errorf(wire.ErrBadRequest, "task.progress: unknown stage %q", p.Stage)
	}
	msg := clipLine(p.Error, taskErrorMax)
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.taskLocked(p.ID)
	if t == nil {
		return wire.Errorf(wire.ErrNotFound, "task %s", p.ID)
	}
	snap := d.snapshotTasksLocked()
	switch {
	case t.State == wire.TaskPreparing && creating:
		// Reported again: whatever setup it waited on is behind it.
		d.dropSetupLocked(t)
		switch {
		case msg != "":
			t.State, t.Stage, t.Error = wire.TaskFailed, p.Stage, msg
			delete(d.prepAt, t.ID)
		case p.Stage == wire.TaskStageAgent:
			t.State, t.Stage, t.Error = wire.TaskReady, "", ""
			delete(d.prepAt, t.ID)
		default:
			t.Stage = p.Stage
		}
	case t.State == wire.TaskReady && p.Stage == wire.TaskStageAgent && msg != "" &&
		p.SessionID != "" && p.SessionID == t.SessionID:
		// The prepared session could not become the agent after reporting
		// stage agent (exec failed).
		t.State, t.Stage, t.Error = wire.TaskFailed, p.Stage, msg
	case t.State == wire.TaskRemoving && p.Stage == wire.TaskStageStop && msg == "":
		// Another removal got there first.
		return wire.Errorf(wire.ErrBusy, "task %s is being removed", p.ID)
	case t.State == wire.TaskRemoving && removing,
		t.State != wire.TaskRemoving && t.State != wire.TaskPreparing && (p.Stage == wire.TaskStageStop || p.Stage == wire.TaskStageArchive):
		starting := t.State != wire.TaskRemoving
		if starting {
			d.removingFrom[t.ID] = t.State
		}
		switch {
		case msg != "":
			// Back to where removing started, naming the stage that
			// failed.
			d.endRemovalLocked(t, msg)
			t.Stage = p.Stage
		case p.SessionID != "":
			// Handed to the session it goes on in.
			t.State, t.Stage, t.Error = wire.TaskRemoving, p.Stage, ""
			now := time.Now()
			d.removers[t.ID] = remover{session: p.SessionID, at: now}
			d.scheduleReviewLocked(now.Add(d.opts.PrepareGrace))
		default:
			t.State, t.Stage, t.Error = wire.TaskRemoving, p.Stage, ""
			if starting {
				d.removers[t.ID] = remover{conn: cs}
			}
		}
	default:
		return wire.Errorf(wire.ErrBadRequest, "task.progress: task %s is %s, stage %s does not apply", p.ID, t.State, p.Stage)
	}
	return d.taskCommittedLocked(snap, t)
}

// ---------------------------------------------------------------------------
// task.forget

// taskForget deletes the task's record, telling watchers the branch
// task.remove kept; its sessions, the ended ones still listed included,
// lose their task_id.
func (d *Daemon) taskForget(p wire.TaskRemoved) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	id := p.ID
	i := slices.IndexFunc(d.tasks, func(t *wire.Task) bool { return t.ID == id })
	if i < 0 {
		return wire.Errorf(wire.ErrNotFound, "task %s", id)
	}
	snap := d.snapshotTasksLocked()
	d.dropSetupLocked(d.tasks[i])
	d.tasks = slices.Delete(d.tasks, i, i+1)
	delete(d.prepAt, id)
	delete(d.removingFrom, id)
	delete(d.removers, id)
	if err := d.commitTasksLocked(snap); err != nil {
		return err
	}
	d.broadcastLocked(wire.NotifyTaskRemoved, p)
	for _, s := range d.sessions {
		if s.s.TaskID == id {
			s.s.TaskID = ""
			d.broadcastLocked(wire.NotifySessionUpdated, s.s)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// task.list

// taskList answers task.list. A ready task whose worktree directory is
// gone, or that `git worktree list` of its checkout no longer lists,
// becomes missing. A missing task whose worktree is back — the directory
// with its .git there, and git still listing it — is ready again (a
// volume reconnected); one git no longer lists stays missing.
func (d *Daemon) taskList() wire.TaskListResult {
	d.mu.Lock()
	d.reviewTasksLocked(time.Now())
	var check []wire.Task
	for _, t := range d.tasks {
		if t.State == wire.TaskReady || t.State == wire.TaskMissing {
			check = append(check, *t)
		}
	}
	d.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), worktreeCheckTimeout)
	defer cancel()
	type listing struct {
		paths []string
		err   error
	}
	listed := map[string]listing{}
	var missing, back []string
	for _, t := range check {
		wasMissing := t.State == wire.TaskMissing
		if !task.IsDir(t.Worktree) {
			if !wasMissing {
				missing = append(missing, t.ID)
			}
			continue
		}
		if wasMissing {
			if _, err := os.Lstat(filepath.Join(t.Worktree, ".git")); err != nil {
				continue
			}
		}
		l, ok := listed[t.Repo]
		if !ok {
			l.paths, l.err = task.Worktrees(ctx, nil, t.Repo)
			listed[t.Repo] = l
		}
		var gerr *task.GitError
		switch {
		case l.err == nil:
			has := slices.ContainsFunc(l.paths, func(p string) bool { return task.SamePath(p, t.Worktree) })
			switch {
			case wasMissing && has:
				back = append(back, t.ID)
			case !wasMissing && !has:
				missing = append(missing, t.ID)
			}
		case wasMissing:
			// Not known to be back: the task stays missing.
		case errors.As(l.err, &gerr) && ctx.Err() == nil:
			// git answered that the checkout is no repository (any more).
			missing = append(missing, t.ID)
		case errors.Is(l.err, exec.ErrNotFound):
			// No git to ask: the task stays as it is.
		default:
			d.logf("daemon: task %s: %v", t.ID, l.err)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range missing {
		if t := d.taskLocked(id); t != nil && t.State == wire.TaskReady {
			t.State = wire.TaskMissing
			d.taskChangedLocked(t)
		}
	}
	for _, id := range back {
		if t := d.taskLocked(id); t != nil && t.State == wire.TaskMissing {
			t.State = wire.TaskReady
			d.taskChangedLocked(t)
		}
	}
	return wire.TaskListResult{Tasks: d.taskListLocked()}
}

// ---------------------------------------------------------------------------
// Session.task_id

// taskOfLocked decides the task of s: an in_place task it prepares or was
// started for (session.spawn task_id), else the worktree task whose
// worktree holds its cwd (the innermost). An in_place task never takes a
// session by its cwd, and no task a schedule's run.
func (d *Daemon) taskOfLocked(s *session) string {
	if s.s.ScheduleID != "" {
		return ""
	}
	for _, t := range d.tasks {
		if t.InPlace && (s.s.ID == t.SessionID || s.taskWant == t.ID) {
			return t.ID
		}
	}
	if s.cwd == "" {
		return ""
	}
	return d.worktreeTaskLocked(s.cwd)
}

// worktreeTaskLocked is the task whose worktree holds canonical path p,
// the innermost when worktrees nest ("": none; an in_place task has none,
// its directory is the source checkout).
func (d *Daemon) worktreeTaskLocked(p string) string {
	best, bestLen := "", -1
	for _, t := range d.tasks {
		if !t.InPlace && t.Worktree != "" && task.Within(p, t.Worktree) && len(t.Worktree) > bestLen {
			best, bestLen = t.ID, len(t.Worktree)
		}
	}
	return best
}

// rebindLocked decides the task of every live session again after a task
// was registered, pushing the sessions that changed.
func (d *Daemon) rebindLocked() {
	for _, s := range d.sessions {
		if s.ended {
			continue
		}
		if id := d.taskOfLocked(s); id != s.s.TaskID {
			s.s.TaskID = id
			d.broadcastLocked(wire.NotifySessionUpdated, s.s)
		}
	}
}

// taskLabelLocked names s's task in a push's title: "#N", or a named
// task's name ("": none).
func (d *Daemon) taskLabelLocked(s *session) string {
	if s.s.TaskID == "" {
		return ""
	}
	t := d.taskLocked(s.s.TaskID)
	switch {
	case t == nil:
		return ""
	case t.Kind == wire.TaskKindName:
		return t.Name
	}
	return "#" + strconv.Itoa(t.Number)
}
