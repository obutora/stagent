package bridge

import (
	"cmp"
	"context"
	"os"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// taskPreviewWait bounds one task.preview (git included).
const taskPreviewWait = 60 * time.Second

// taskPreview answers task.preview: what a new worktree of the checkout
// would get copied and linked, resolved by the same code as the copy stage
// of `stagent task run`, and whether a named task could take a name. It
// only reads, so agent descendants may call it (ADR 0004). Its own queue
// keeps a slow git (a large ignored tree) from delaying the daemon and
// spawn queues.
func (b *Bridge) taskPreview(m *wire.Msg) {
	var p wire.TaskPreviewParams
	if err := rpc.Decode(m, &p); err != nil {
		b.replyResult(m.ID, nil, err)
		return
	}
	res, err := b.doTaskPreview(p)
	b.replyResult(m.ID, res, err)
}

func (b *Bridge) doTaskPreview(p wire.TaskPreviewParams) (*wire.TaskPreviewResult, error) {
	const method = wire.MethodTaskPreview
	if p.Repo == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: repo is empty", method)
	}
	// git runs with the environment sessions start from, as for spawn.
	env := ensureOnPath(b.sessionEnv(), "git", b.l.Home)
	ctx, cancel := context.WithTimeout(b.ctx, taskPreviewWait)
	defer cancel()
	repo, err := task.Canonical(ctx, env, b.l.ExpandHome(p.Repo))
	if err != nil {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: repo %s: %v", method, p.Repo, err)
	}
	// Absent worktree_include: the saved patterns, read from config.json
	// as `stagent task run` reads them.
	patterns := p.WorktreeInclude
	if patterns == nil {
		patterns = task.SavedPatterns(b.l.Config, repo)
	}
	res, err := task.Preview(ctx, env, repo, patterns)
	if err != nil {
		return nil, wire.Errorf(wire.ErrUnavailable, "%s: %v", method, err)
	}
	if p.Name != "" {
		if res.NameProblem, err = b.nameProblem(ctx, env, repo, p.Name, cmp.Or(p.Branch, p.Name)); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// nameProblem is the first reason a new named task of repo cannot take
// name with branch (wire.TaskName*), "" when it can: what task.create
// refuses, and a branch that exists (task.create would take it as it is).
func (b *Bridge) nameProblem(ctx context.Context, env []string, repo, name, branch string) (string, error) {
	if task.ValidName(ctx, env, repo, name) != nil {
		return wire.TaskNameInvalid, nil
	}
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return "", upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	var list wire.TaskListResult
	if err := dc.c.Call(ctx, wire.MethodTaskList, struct{}{}, &list); err != nil {
		return "", err
	}
	for i := range list.Tasks {
		if task.SameKey(&list.Tasks[i], repo, wire.TaskKindName, 0, name) {
			return wire.TaskNameTask, nil
		}
	}
	folder := task.SavedWorktreeFolder(b.l.Config)
	if folder != "" {
		if _, err := os.Lstat(taskPlace(repo, folder, wire.TaskKindName, 0, "", name)); err == nil {
			return wire.TaskNameFolder, nil
		}
	}
	if task.HasRef(ctx, env, repo, "refs/heads/"+branch) {
		return wire.TaskNameBranch, nil
	}
	if folder == "" {
		return wire.TaskNameNoFolder, nil
	}
	return "", nil
}
