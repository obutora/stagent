package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
	"github.com/obutora/stagent/internal/worktree"
)

// The worktree.* methods have queues of their own (ADR 0006): a slow
// inventory, disk walk or gh never delays the daemon, spawn or remove
// queues, nor one another. worktree.remove shares task.remove's queue: one
// removal at a time.
const (
	targetWorktree      = "worktree" // worktree.list, worktree.watch
	targetWorktreeSizes = "worktree.sizes"
	targetWorktreePRs   = "worktree.prs"
)

const (
	// worktreeListWait bounds worktree.list, worktree.prs and
	// worktree.watch: git for every worktree, gh for every branch.
	worktreeListWait = 2 * time.Minute
	// worktreeSizesWait bounds worktree.sizes: the inventory and the disk
	// walks (a walk cut short counts what it saw).
	worktreeSizesWait = 10 * time.Minute
)

// serveWorktree decodes m's params into P, runs do and answers.
func serveWorktree[P, R any](b *Bridge, do func(P) (R, error)) func(*wire.Msg) {
	return func(m *wire.Msg) {
		var p P
		if err := rpc.Decode(m, &p); err != nil {
			b.replyResult(m.ID, nil, err)
			return
		}
		res, err := do(p)
		b.replyResult(m.ID, res, err)
	}
}

// worktreeEnv is git's and gh's environment, as for task.remove.
func (b *Bridge) worktreeEnv() []string {
	env := b.sessionEnv()
	for _, name := range []string{"git", "gh"} {
		env = ensureOnPath(env, name, b.l.Home)
	}
	return env
}

// hostWorktrees is the inventory with what it was made from.
type hostWorktrees struct {
	inv      *worktree.Inventory
	cfg      wire.Config
	dc       *daemonConn
	tasks    []wire.Task
	sessions []wire.Session
}

// hostWorktrees makes the inventory from config.json (worktree_folder,
// worktree_watch, worktree_repos) and the daemon's tasks and sessions.
func (b *Bridge) hostWorktrees(ctx context.Context, env []string) (*hostWorktrees, error) {
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	var tasks wire.TaskListResult
	if err := dc.c.Call(ctx, wire.MethodTaskList, struct{}{}, &tasks); err != nil {
		return nil, err
	}
	var sessions wire.SessionsListResult
	if err := dc.c.Call(ctx, wire.MethodSessionsList, struct{}{}, &sessions); err != nil {
		return nil, err
	}
	cfg := task.SavedConfig(b.l.Config)
	inv, err := worktree.Discover(ctx, worktree.Sources{
		Env: env, Folder: cfg.WorktreeFolder, Watch: cfg.WorktreeWatch, Repos: cfg.WorktreeRepos,
		Tasks: tasks.Tasks, Sessions: sessions.Sessions,
	})
	if err != nil {
		return nil, wire.Errorf(wire.ErrUnavailable, "git: %v", err)
	}
	return &hostWorktrees{inv: inv, cfg: cfg, dc: dc, tasks: tasks.Tasks, sessions: sessions.Sessions}, nil
}

// setConfig has the daemon, config.json's only writer, merge patch.
func setConfig(ctx context.Context, dc *daemonConn, patch map[string]any) (wire.Config, error) {
	b, err := json.Marshal(patch)
	if err != nil {
		return wire.Config{}, err
	}
	var res wire.ConfigResult
	err = dc.c.Call(ctx, wire.MethodConfigSet, wire.ConfigSetParams{Config: b}, &res)
	return res.Config, err
}

// doWorktreeList answers worktree.list. It changes nothing on the host but
// worktree_repos, written when the repositories found differ from it (a
// source checkout that is gone is not found).
func (b *Bridge) doWorktreeList(struct{}) (*wire.WorktreeListResult, error) {
	env := b.worktreeEnv()
	ctx, cancel := context.WithTimeout(b.ctx, worktreeListWait)
	defer cancel()
	h, err := b.hostWorktrees(ctx, env)
	if err != nil {
		return nil, err
	}
	if found := h.inv.RepoPaths(); !slices.Equal(found, h.cfg.WorktreeRepos) {
		// From a coding agent's process tree the daemon refuses it; the
		// list does not depend on it.
		if _, err := setConfig(ctx, h.dc, map[string]any{"worktree_repos": found}); err != nil {
			fmt.Fprintln(os.Stderr, "stagent bridge: worktree_repos:", err)
		}
	}
	var live []wire.Session
	for _, s := range h.sessions {
		if !ended(s) {
			live = append(live, s)
		}
	}
	return &wire.WorktreeListResult{
		WorktreeFolder: h.inv.Folder,
		WatchFolders:   orEmpty(h.cfg.WorktreeWatch),
		Repos:          worktree.List(ctx, env, h.inv, h.tasks, live),
		Folders:        h.inv.Folders,
	}, nil
}

// doWorktreeSizes answers worktree.sizes.
func (b *Bridge) doWorktreeSizes(struct{}) (*wire.WorktreeSizesResult, error) {
	env := b.worktreeEnv()
	ctx, cancel := context.WithTimeout(b.ctx, worktreeSizesWait)
	defer cancel()
	h, err := b.hostWorktrees(ctx, env)
	if err != nil {
		return nil, err
	}
	res := worktree.Sizes(ctx, h.inv)
	return &res, nil
}

// doWorktreePRs answers worktree.prs.
func (b *Bridge) doWorktreePRs(struct{}) (*wire.WorktreePRsResult, error) {
	env := b.worktreeEnv()
	ctx, cancel := context.WithTimeout(b.ctx, worktreeListWait)
	defer cancel()
	h, err := b.hostWorktrees(ctx, env)
	if err != nil {
		return nil, err
	}
	return &wire.WorktreePRsResult{PRs: worktree.PRs(ctx, env, h.inv, h.tasks)}, nil
}

// doWorktreeWatch adds a watch folder (見るフォルダ: ~ expanded, an
// existing directory, canonical) or removes one (the same path), writing
// worktree_watch through the daemon. A coding agent's process tree changes
// no setting (ADR 0004).
func (b *Bridge) doWorktreeWatch(p wire.WorktreeWatchParams) (*wire.WorktreeWatchResult, error) {
	const method = wire.MethodWorktreeWatch
	if (p.Add == "") == (p.Remove == "") {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: give add or remove", method)
	}
	if werr := b.agentRefusal(); werr != nil {
		return nil, werr
	}
	watch := slices.Clone(task.SavedConfig(b.l.Config).WorktreeWatch)
	if p.Add != "" {
		dir := b.l.ExpandHome(p.Add)
		if !filepath.IsAbs(dir) {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s is not an absolute path", method, p.Add)
		}
		dir = task.CanonicalPath(dir)
		if !task.IsDir(dir) {
			return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s is not a directory", method, p.Add)
		}
		if !slices.ContainsFunc(watch, func(w string) bool { return task.SamePath(w, dir) }) {
			watch = append(watch, dir)
		}
	} else {
		dir := filepath.Clean(b.l.ExpandHome(p.Remove))
		canonical := task.CanonicalPath(dir)
		watch = slices.DeleteFunc(watch, func(w string) bool { return task.SamePath(w, dir) || task.SamePath(w, canonical) })
	}
	ctx, cancel := context.WithTimeout(b.ctx, daemonCallWait)
	defer cancel()
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		return nil, upstreamError(err, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
	}
	b.rewatch(dc)
	cfg, err := setConfig(ctx, dc, map[string]any{"worktree_watch": orEmpty(watch)})
	if err != nil {
		return nil, err
	}
	return &wire.WorktreeWatchResult{WatchFolders: orEmpty(cfg.WorktreeWatch)}, nil
}

// doWorktreeRemove is worktree を消す, on task.remove's queue: path is
// looked up in the inventory. A task's worktree is 片付け (doTaskRemove);
// another worktree is removed by the same rules without archive; a folder
// of the worktree folder is deleted. Then an empty <worktree folder>/<repo
// name> it was in goes too — for a worktree in the removal's session, so
// that it goes after the bridge has. The source checkout is never removed
// (bad_request); a worktree's refusals (task.Refusals: a locked one
// included, but for a task's worktree a killed `git worktree add` left
// half made) refuse it (refused). A coding agent's process tree removes
// nothing, not even a dry run (ADR 0004).
func (b *Bridge) doWorktreeRemove(p wire.WorktreeRemoveParams) (*wire.WorktreeRemoveResult, error) {
	const method = wire.MethodWorktreeRemove
	if p.Path == "" {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: path is empty", method)
	}
	if werr := b.agentRefusal(); werr != nil {
		return nil, werr
	}
	path := b.l.ExpandHome(p.Path)
	if !filepath.IsAbs(path) {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s is not an absolute path", method, p.Path)
	}
	path = task.CanonicalPath(path)
	env := b.worktreeEnv()
	ctx, cancel := context.WithTimeout(b.ctx, taskRemoveWait)
	defer cancel()
	h, err := b.hostWorktrees(ctx, env)
	if err != nil {
		return nil, err
	}
	hit, ok := h.inv.Find(path)
	var res *wire.WorktreeRemoveResult
	switch {
	case !ok:
		return nil, wire.Errorf(wire.ErrNotFound, "%s: %s is no worktree or folder this host knows", method, path)
	case hit.Folder != nil:
		res, err = removeFolder(ctx, h.inv, hit.Folder.Path, p.DryRun)
		if err == nil && !p.DryRun {
			res.RemovedParent = task.RemoveEmptyParent(h.inv.Folder, path)
		}
	case hit.Worktree == nil:
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s is a source checkout", method, path)
	default:
		if t := worktree.TaskOf(h.tasks, hit.Worktree.Path); t != nil {
			var tr *wire.TaskRemoveResult
			var parent string
			tr, parent, err = b.doTaskRemove(wire.TaskRemoveParams{ID: t.ID, Force: p.Force, StopSessions: p.StopSessions, DryRun: p.DryRun}, h.inv.Folder)
			if err == nil {
				res = &wire.WorktreeRemoveResult{
					Refusals: tr.Refusals, Changes: tr.Changes, ChangesTruncated: tr.ChangesTruncated, Unpushed: tr.Unpushed,
					Sessions: tr.Sessions, KeptBranch: tr.KeptBranch, RemovedParent: parent,
				}
			}
		} else {
			res, err = b.removeWorktree(ctx, h, env, *hit.Repo, *hit.Worktree, p)
		}
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// removeWorktree removes worktree w of repository r, which no task has: a
// dry run reports what is in the way (as task.remove's); otherwise its
// refusals refuse it whatever the flags (refused), changes need force
// (not_clean) and live sessions inside it stop_sessions (busy), then
// those are stopped as task.remove stops a task's and the removal runs
// without archive in a session (removeWorktreeInSession) — the shared
// directories' links, git worktree remove, the branch only when nothing is
// lost; never the remote.
func (b *Bridge) removeWorktree(ctx context.Context, h *hostWorktrees, env []string, r worktree.Repo, w task.Worktree, p wire.WorktreeRemoveParams) (*wire.WorktreeRemoveResult, error) {
	const method = wire.MethodWorktreeRemove
	inside := func(s wire.Session) bool { return worktree.Inside(s, w.Path) }
	live, err := liveSessions(ctx, h.dc, inside, nil)
	if err != nil {
		return nil, err
	}
	refusals, err := task.Refusals(ctx, env, r.Worktrees, w.Path, false)
	if err != nil {
		return nil, wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
	}
	var changes []string
	if !w.Missing {
		if changes, err = task.Changes(ctx, env, w.Path, task.ShareLinks(r.Path, w.Path)); err != nil {
			return nil, wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
		}
	}
	if p.DryRun {
		res := &wire.WorktreeRemoveResult{Refusals: refusals, Changes: changes, Sessions: live}
		if len(changes) > wire.TaskRemoveChangesMax {
			res.Changes, res.ChangesTruncated = changes[:wire.TaskRemoveChangesMax], true
		}
		if res.Unpushed, err = task.Unpushed(ctx, env, r.Path, w.Branch); err != nil {
			return nil, wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
		}
		return res, nil
	}
	switch {
	case len(refusals) > 0:
		return nil, refusedError(method, w.Path, refusals)
	case len(changes) > 0 && !p.Force:
		return nil, wire.Errorf(wire.ErrNotClean, "%s: %d uncommitted change(s) in %s", method, len(changes), w.Path)
	case len(live) > 0 && !p.StopSessions:
		return nil, wire.Errorf(wire.ErrBusy, "%s: %d live session(s)", method, len(live))
	}
	left, err := b.stopAll(ctx, h.dc, inside, live)
	if err != nil {
		return nil, err
	}
	if len(left) > 0 {
		return nil, wire.Errorf(wire.ErrBusy, "%s: %d session(s) did not stop", method, len(left))
	}
	return b.removeWorktreeInSession(r.Path, w, p.Force, h.inv.Folder, env)
}

// removeWorktreeInSession runs `stagent task remove-worktree` for w in a
// new session whose cwd is repo (runRemoval), as removeInSession runs a
// task's removal: it goes on when the bridge goes, the empty parent in
// the worktree folder included.
func (b *Bridge) removeWorktreeInSession(repo string, w task.Worktree, force bool, folder string, env []string) (*wire.WorktreeRemoveResult, error) {
	const method = wire.MethodWorktreeRemove
	var flags []string
	if force {
		flags = append(flags, "--force")
	}
	if folder != "" {
		flags = append(flags, "--folder", folder)
	}
	operands := []string{repo, w.Path}
	if w.Branch != "" {
		operands = append(operands, w.Branch)
	}
	_, r, err := b.runRemoval("remove-worktree", flags, operands, repo, env, nil, nil, func(err error) error {
		var we *wire.Error
		if errors.As(err, &we) {
			return we
		}
		return wire.Errorf(wire.ErrInternal, "%s: %s", method, oneLine(err.Error()))
	})
	switch {
	case err != nil:
		return nil, err
	case r == nil:
		return nil, wire.Errorf(wire.ErrInternal, "%s: stagent task remove-worktree ended before finishing (see its session)", method)
	case len(r.Refusals) > 0:
		return nil, refusedError(method, w.Path, r.Refusals)
	case r.Error != "":
		return nil, wire.Errorf(wire.ErrInternal, "%s", oneLine(r.Error))
	}
	return &wire.WorktreeRemoveResult{KeptBranch: r.KeptBranch, RemovedParent: r.RemovedParent}, nil
}

// removeFolder removes a folder of the worktree folder that is no
// worktree; a dry run gives its size and largest entries instead. One
// with a source checkout or worktree somewhere in it is not removed
// (bad_request): that would go with it.
func removeFolder(ctx context.Context, inv *worktree.Inventory, path string, dryRun bool) (*wire.WorktreeRemoveResult, error) {
	if inv.Holds(path) {
		return nil, wire.Errorf(wire.ErrBadRequest, "%s: %s holds a worktree or source checkout", wire.MethodWorktreeRemove, path)
	}
	if dryRun {
		exclude := inv.Exclude()
		bytes, _ := worktree.Measure(ctx, path, exclude)
		return &wire.WorktreeRemoveResult{Bytes: bytes, Entries: worktree.Top(ctx, path, wire.WorktreeRemoveTop, exclude)}, nil
	}
	if err := worktree.RemoveFolder(path); err != nil {
		return nil, wire.Errorf(wire.ErrInternal, "%s: %s", wire.MethodWorktreeRemove, oneLine(err.Error()))
	}
	return &wire.WorktreeRemoveResult{}, nil
}

// orEmpty is s, or an empty list for nil (JSON [] rather than null).
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
