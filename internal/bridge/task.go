package bridge

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// taskCreateWait bounds task.create: git, gh pr view and the daemon.
const taskCreateWait = 60 * time.Second

func (b *Bridge) taskCreate(m *wire.Msg) {
	var p wire.TaskCreateParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	res, err := b.doTaskCreate(p)
	b.replyResult(m.ID, res, err)
}

// doTaskCreate registers the task with the daemon (task.register) and,
// unless the daemon says it exists, starts its prepared session: a holder
// running `stagent task run <id> -- <command…>` in the task's place — the
// worktree directory, made empty here first, or the source checkout when
// in_place — so the session's cwd is the task's place from the start (ADR
// 0006). It runs on the spawn queue.
func (b *Bridge) doTaskCreate(p wire.TaskCreateParams) (*wire.TaskCreateResult, error) {
	const method = wire.MethodTaskCreate
	named := p.Kind == wire.TaskKindName
	switch {
	case p.Kind != wire.TaskKindIssue && p.Kind != wire.TaskKindPR && !named:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: kind must be issue, pr or name", method)
	case named && p.Number != 0:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: a named task has no number", method)
	case named && p.InPlace:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: a named task cannot be in_place", method)
	case !named && p.Number <= 0:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: number must be positive", method)
	case len(p.Command) > 0 && p.Command[0] == "":
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: command is empty", method)
	case p.Repo == "":
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: repo is empty", method)
	}
	if named {
		p.Title, p.URL = "", ""
	} else {
		p.Name = ""
	}
	// Nothing starts for a coding agent's process tree (ADR 0004).
	if werr := b.agentRefusal(); werr != nil {
		return nil, werr
	}
	cols, rows, err := termSize(method, p.Cols, p.Rows)
	if err != nil {
		return nil, err
	}
	env := b.sessionEnv()
	for _, name := range []string{"git", "gh"} {
		env = ensureOnPath(env, name, b.l.Home)
	}
	command := p.Command
	if len(command) == 0 {
		command = terminalCommand(env)
	} else {
		env = ensureOnPath(env, command[0], b.l.Home)
	}
	if env, err = addEnv(method, env, p.Env); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(b.ctx, taskCreateWait)
	defer cancel()
	repo, err := task.Canonical(ctx, env, b.l.ExpandHome(p.Repo))
	if err != nil {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: repo %s: %v", method, p.Repo, err)
	}
	folder := task.SavedWorktreeFolder(b.l.Config)
	if named {
		if err := task.ValidName(ctx, env, repo, p.Name); err != nil {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
		}
		if folder == "" {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: worktree_folder is not set", method)
		}
	}
	place := taskPlace(repo, folder, p.Kind, p.Number, p.Title, p.Name)
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	var list wire.TaskListResult
	if err := dc.c.Call(ctx, wire.MethodTaskList, struct{}{}, &list); err != nil {
		return nil, err
	}
	var known *wire.Task
	for i := range list.Tasks {
		if task.SameKey(&list.Tasks[i], repo, p.Kind, p.Number, p.Name) {
			known = &list.Tasks[i]
		}
	}
	// A new named task's place is its own: one taken by something else
	// is not a worktree it may adopt.
	if named && known == nil {
		if ents, err := os.ReadDir(place); err == nil && len(ents) > 0 || err != nil && !os.IsNotExist(err) {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s exists and is not an empty directory", method, place)
		}
	}
	branch, base, err := taskBranch(ctx, env, repo, p, known)
	if err != nil {
		return nil, err
	}

	sessionID := wire.NewSessionID()
	var reg wire.TaskRegisterResult
	if err := dc.c.Call(ctx, wire.MethodTaskRegister, wire.TaskRegisterParams{
		Repo: repo, Kind: p.Kind, Number: p.Number, Name: p.Name, Title: p.Title, URL: p.URL,
		Branch: branch, Base: base, Worktree: place, InPlace: p.InPlace,
		SessionID: sessionID,
	}, &reg); err != nil {
		return nil, err
	}
	t := reg.Task
	if reg.Existed {
		return &wire.TaskCreateResult{Task: t, Session: reg.Session, Existed: true}, nil
	}
	// The task is preparing now: a failure from here on fails it rather
	// than leaving it to be found interrupted.
	failed := func(stage string, err error) error {
		var none struct{}
		dc.c.Call(ctx, wire.MethodTaskProgress, wire.TaskProgressParams{ID: t.ID, Stage: stage, Error: err.Error()}, &none)
		return err
	}
	cwd := t.Worktree
	if t.InPlace {
		cwd = t.Repo
	} else if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, failed(wire.TaskStageWorktree, err)
	}
	exe, err := daemonclient.Exe()
	if err != nil {
		return nil, failed(wire.TaskStageFetch, err)
	}
	run := []string{exe, "task", "run"}
	if t.InPlace && p.SwitchBranch {
		run = append(run, "--switch-branch")
	}
	if t.InPlace && p.Setup {
		run = append(run, "--setup")
	}
	run = append(append(run, t.ID, "--"), command...)
	s, err := b.startSession(sessionID, run, cwd, cols, rows, env, t.ID)
	if err != nil {
		if !t.InPlace {
			os.Remove(cwd) // only when still empty
		}
		return nil, failed(wire.TaskStageFetch, fmt.Errorf("start the session: %w", err))
	}
	return &wire.TaskCreateResult{Task: t, Session: s}, nil
}

// taskPlace is where a worktree task of the source checkout repo works.
// With the worktree folder (worktree の置き場) set it is
// <folder>/<checkout name>/<name> for a named task and
// <folder>/<checkout name>/<number>-<slug> (task.IssueBranch, whatever the
// task's branch) for an issue or PR; without it next to the checkout,
// <parent>/<checkout name>-<number> (a named task needs the folder).
func taskPlace(repo, folder, kind string, number int, title, name string) string {
	switch {
	case folder == "":
		return filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-"+strconv.Itoa(number))
	case kind == wire.TaskKindName:
		return filepath.Join(folder, filepath.Base(repo), name)
	}
	return filepath.Join(folder, filepath.Base(repo), task.IssueBranch(number, title))
}

// taskBranch decides the task's branch and base. A known task keeps its
// own unless the app names others: its worktree is on that branch. An
// in_place task that does not switch records the checkout's current
// branch. Issue: the app's branch or <number>-<slug>. Named: the app's
// branch or the name. PR: gh's head and base branch. Base: the app's
// (issue, named), else the default branch.
func taskBranch(ctx context.Context, env []string, repo string, p wire.TaskCreateParams, known *wire.Task) (branch, base string, err error) {
	const method = wire.MethodTaskCreate
	if p.Kind == wire.TaskKindPR {
		p.Branch, p.Base = "", ""
	} else if p.Branch != "" {
		if err := task.CheckBranch(ctx, env, repo, p.Branch); err != nil {
			return "", "", wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
		}
	}
	if known != nil {
		return cmp.Or(p.Branch, known.Branch), cmp.Or(p.Base, known.Base), nil
	}
	defaultBase := func() (string, error) {
		if p.Base != "" {
			return p.Base, nil
		}
		b, err := task.DefaultBranch(ctx, env, repo)
		if err != nil {
			return "", wire.Errorf(wire.ErrBadRequest, "%s: %v; pass base", method, err)
		}
		return b, nil
	}
	switch {
	case p.InPlace && !p.SwitchBranch:
		if branch, err = task.CurrentBranch(ctx, env, repo); err != nil {
			return "", "", wire.Errorf(wire.ErrBadRequest, "%s: %v", method, err)
		}
		base, _ = defaultBase() // only recorded
		return branch, base, nil
	case p.Kind == wire.TaskKindPR:
		if branch, base, err = task.PullRequestRefs(ctx, env, repo, p.Number); err != nil {
			return "", "", wire.Errorf(wire.ErrUnavailable, "%s: %v", method, err)
		}
		return branch, base, nil
	}
	if base, err = defaultBase(); err != nil {
		return "", "", err
	}
	if p.Kind == wire.TaskKindName {
		return cmp.Or(p.Branch, p.Name), base, nil
	}
	return cmp.Or(p.Branch, task.IssueBranch(p.Number, p.Title)), base, nil
}
