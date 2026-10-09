package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// Tasks (タスク) live in <root>/state/tasks.json, written by the daemon
// only. A task's key is its source checkout (元のチェックアウト, canonical)
// and number. The bridge registers one (task.register) before preparing
// it, `stagent task run` reports its stages (task.progress), and task.list,
// watch and task.updated / task.removed show them to the app.

const (
	// defaultPrepareGrace is how long a preparing task waits for its
	// prepared session to register (after task.register, or after a daemon
	// restart for its holder to come back) before it counts as interrupted.
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
		err = writeFileAtomic(d.layout.Tasks, b)
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
}

func (d *Daemon) snapshotTasksLocked() tasksSnapshot {
	return tasksSnapshot{tasks: d.taskListLocked(), prepAt: maps.Clone(d.prepAt), removingFrom: maps.Clone(d.removingFrom)}
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
	d.prepAt, d.removingFrom = snap.prepAt, snap.removingFrom
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
// Interrupted preparations

// preparingLocked puts t in preparing as of now and arranges the check
// that its prepared session registers.
func (d *Daemon) preparingLocked(t *wire.Task, now time.Time) {
	t.State, t.Stage, t.Error = wire.TaskPreparing, "", ""
	d.prepAt[t.ID] = now
	d.scheduleReviewLocked(now.Add(d.opts.PrepareGrace))
}

// reviewTasksLocked fails (error interrupted) every preparing task whose
// prepared session ended, or did not register within PrepareGrace of the
// task's preparation (or of the daemon's start). It is run by task.list,
// watch, task.register, at a session's end and by a timer.
func (d *Daemon) reviewTasksLocked(now time.Time) {
	for _, t := range d.tasks {
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
		delete(d.prepAt, t.ID)
		d.taskChangedLocked(t)
	}
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
	switch {
	case !filepath.IsAbs(p.Repo):
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: repo must be an absolute path")
	case p.Kind != wire.TaskKindIssue && p.Kind != wire.TaskKindPR:
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: kind must be issue or pr")
	case p.Number <= 0:
		return wire.TaskRegisterResult{}, wire.Errorf(wire.ErrBadRequest, "task.register: number must be positive")
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
		if t.Number != p.Number || !task.SamePath(t.Repo, repo) {
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
		ID: task.NewID(), Repo: repo, Kind: p.Kind, Number: p.Number, Title: p.Title, URL: p.URL,
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

func (d *Daemon) taskProgress(p wire.TaskProgressParams) error {
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
		if t.State != wire.TaskRemoving {
			d.removingFrom[t.ID] = t.State
		}
		if msg != "" {
			// Back to where removing started (ready after a restart),
			// naming the stage that failed.
			back := d.removingFrom[t.ID]
			if back == "" {
				back = wire.TaskReady
			}
			delete(d.removingFrom, t.ID)
			t.State, t.Stage, t.Error = back, p.Stage, msg
		} else {
			t.State, t.Stage, t.Error = wire.TaskRemoving, p.Stage, ""
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
	d.tasks = slices.Delete(d.tasks, i, i+1)
	delete(d.prepAt, id)
	delete(d.removingFrom, id)
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
// becomes missing.
func (d *Daemon) taskList() wire.TaskListResult {
	d.mu.Lock()
	d.reviewTasksLocked(time.Now())
	var check []wire.Task
	for _, t := range d.tasks {
		if t.State == wire.TaskReady {
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
	var missing []string
	for _, t := range check {
		if !task.IsDir(t.Worktree) {
			missing = append(missing, t.ID)
			continue
		}
		l, ok := listed[t.Repo]
		if !ok {
			l.paths, l.err = task.Worktrees(ctx, nil, t.Repo)
			listed[t.Repo] = l
		}
		var gerr *task.GitError
		switch {
		case l.err == nil:
			if !slices.ContainsFunc(l.paths, func(p string) bool { return task.SamePath(p, t.Worktree) }) {
				missing = append(missing, t.ID)
			}
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
	return wire.TaskListResult{Tasks: d.taskListLocked()}
}

// ---------------------------------------------------------------------------
// Session.task_id

// taskOfLocked decides the task of s: an in_place task it prepares or was
// started for (session.spawn task_id), else the worktree task whose
// worktree holds its cwd (the innermost). An in_place task never takes a
// session by its cwd.
func (d *Daemon) taskOfLocked(s *session) string {
	best, bestLen := "", -1
	for _, t := range d.tasks {
		if t.InPlace {
			if s.s.ID == t.SessionID || s.taskWant == t.ID {
				return t.ID
			}
			continue
		}
		if s.cwd != "" && task.Within(s.cwd, t.Worktree) && len(t.Worktree) > bestLen {
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

// taskNumberLocked is the number of s's task (0: none).
func (d *Daemon) taskNumberLocked(s *session) int {
	if s.s.TaskID == "" {
		return 0
	}
	if t := d.taskLocked(s.s.TaskID); t != nil {
		return t.Number
	}
	return 0
}
