package bridge

import (
	"context"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// taskPreviewWait bounds one task.preview (git included).
const taskPreviewWait = 60 * time.Second

// taskPreview answers task.preview: what a new worktree of the checkout
// would get copied and linked, resolved by the same code as the copy stage
// of `stagent task run`. It only reads, so agent descendants may call it
// (ADR 0004). Its own queue keeps a slow git (a large ignored tree) from
// delaying the daemon and spawn queues.
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
	repo, err := task.Canonical(ctx, env, b.expandHome(p.Repo))
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
	return res, nil
}
