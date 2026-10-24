// Package worktree is the host's inventory (目録) of worktrees: every
// repository stagent can find, each with every linked worktree git knows
// (who ever made it), and the other folders left in the worktree folder
// (置き場のほかのフォルダ). The bridge's worktree.* methods list, measure
// and remove them (ADR 0016).
package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// parallel bounds the git, gh and disk walks run at once.
const parallel = 4

// Sources are where Discover looks for repositories.
type Sources struct {
	// Env is git's environment (nil: this process's).
	Env []string
	// Folder is the worktree folder (worktree の置き場); "": none.
	Folder string
	// Watch are the watch folders (見るフォルダ).
	Watch []string
	// Repos are the source checkouts found before (worktree_repos).
	Repos []string
	// Tasks are the task records: their source checkouts and worktrees.
	Tasks []wire.Task
	// Sessions are sessions, ended ones too: their cwds.
	Sessions []wire.Session
}

// Inventory is what Discover found.
type Inventory struct {
	// Folder is the worktree folder, canonical; "": none.
	Folder string
	// Repos are in the order of their path.
	Repos []Repo
	// Folders are in path order.
	Folders []wire.WorktreeFolder
}

// Repo is a repository. Only one with linked worktrees is listed (List,
// Sizes): the source checkout alone has nothing to remove, and one around
// everything (dotfiles kept in git at ~) is not measured. All are
// remembered: a repository's name still makes <repo name> of the worktree
// folder after its last worktree went.
type Repo struct {
	// Path is its source checkout (main worktree), canonical.
	Path string
	// DefaultBranch is "" when it cannot be told.
	DefaultBranch string
	// GitHub: a remote's URL is on github.com.
	GitHub bool
	// Worktrees are its linked worktrees in git's order.
	Worktrees []Worktree
}

func (r Repo) listed() bool { return len(r.Worktrees) > 0 }

// Worktree is a linked worktree as `git worktree list` reports it.
type Worktree struct {
	Path       string // canonical
	Branch     string // short name; "" when detached
	Head       string
	Missing    bool // its directory is gone (prunable)
	Locked     bool
	LockReason string
}

// Discover finds the repositories of src and their worktrees, without
// changing anything. Each candidate directory — worktree_repos, the tasks'
// source checkouts and worktrees, the worktree folder's <x> and <x>/<y>
// and the watch folders' <x> that have a .git, the sessions' existing cwds
// — is taken to the nearest directory with a .git at or above it; unless
// that is a source checkout or worktree found already, `git worktree list`
// runs there and its first entry is the source checkout. A repository
// around another (dotfiles at ~) so hides none inside it. A candidate git
// fails for is skipped silently; git not found at all is the error.
func Discover(ctx context.Context, src Sources) (*Inventory, error) {
	env := noLocks(src.Env)
	inv := &Inventory{}
	if src.Folder != "" {
		inv.Folder = task.CanonicalPath(src.Folder)
	}
	var candidates []string
	candidates = append(candidates, src.Repos...)
	for _, t := range src.Tasks {
		candidates = append(candidates, t.Repo)
		if !t.InPlace {
			candidates = append(candidates, t.Worktree)
		}
	}
	if inv.Folder != "" {
		for _, x := range subdirs(inv.Folder) {
			if hasGit(x) {
				candidates = append(candidates, x)
			}
			for _, y := range subdirs(x) {
				if hasGit(y) {
					candidates = append(candidates, y)
				}
			}
		}
	}
	for _, w := range src.Watch {
		for _, x := range subdirs(w) {
			if hasGit(x) {
				candidates = append(candidates, x)
			}
		}
	}
	for _, s := range src.Sessions {
		if s.Cwd != "" && task.IsDir(s.Cwd) {
			candidates = append(candidates, s.Cwd)
		}
	}

	for _, c := range candidates {
		if c == "" || !filepath.IsAbs(c) {
			continue
		}
		c = task.CanonicalPath(c)
		if !task.IsDir(c) {
			continue
		}
		top := gitTop(c)
		if top == "" || inv.known(top) {
			continue
		}
		out, err := task.Git(ctx, env, top, "worktree", "list", "--porcelain")
		if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		if err != nil {
			continue
		}
		ws := parsePorcelain(string(out))
		if len(ws) == 0 || inv.repo(ws[0].Path) != nil {
			continue
		}
		inv.Repos = append(inv.Repos, Repo{Path: ws[0].Path, Worktrees: ws[1:]})
	}
	slices.SortFunc(inv.Repos, func(a, b Repo) int { return strings.Compare(a.Path, b.Path) })
	each(len(inv.Repos), func(i int) {
		r := &inv.Repos[i]
		r.DefaultBranch = defaultBranch(ctx, env, r.Path)
		r.GitHub = onGitHub(ctx, env, r.Path)
	})
	inv.Folders = inv.otherFolders()
	return inv, nil
}

// defaultBranch is the repository's default branch as task.DefaultBranch
// tells it from origin, else — a repository without origin's — its local
// main or master; "" when none.
func defaultBranch(ctx context.Context, env []string, repo string) string {
	if name, err := task.DefaultBranch(ctx, env, repo); err == nil {
		return name
	}
	for _, name := range []string{"main", "master"} {
		if task.HasRef(ctx, env, repo, "refs/heads/"+name) {
			return name
		}
	}
	return ""
}

// parsePorcelain reads `git worktree list --porcelain` (not -z: git 2.36):
// blocks of "worktree <path>", "HEAD <sha>", "branch <ref>" or "detached",
// "bare", "locked [reason]", "prunable [reason]". A worktree whose
// directory is gone is missing even when an older git does not say
// prunable.
func parsePorcelain(out string) []Worktree {
	var ws []Worktree
	for line := range strings.Lines(out) {
		line = strings.TrimRight(line, "\r\n")
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			ws = append(ws, Worktree{Path: task.CanonicalPath(filepath.FromSlash(p))})
			continue
		}
		if len(ws) == 0 {
			continue
		}
		w := &ws[len(ws)-1]
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "HEAD":
			w.Head = val
		case "branch":
			w.Branch = strings.TrimPrefix(val, "refs/heads/")
		case "locked":
			w.Locked, w.LockReason = true, val
		case "prunable":
			w.Missing = true
		}
	}
	for i := range ws {
		if !ws[i].Missing {
			if _, err := os.Lstat(ws[i].Path); os.IsNotExist(err) {
				ws[i].Missing = true
			}
		}
	}
	return ws
}

// onGitHub reports whether a remote of repo has a github.com URL.
func onGitHub(ctx context.Context, env []string, repo string) bool {
	out, err := task.Git(ctx, env, repo, "config", "--get-regexp", `^remote\..*\.url$`)
	return err == nil && strings.Contains(strings.ToLower(string(out)), "github.com")
}

// repo is the repository whose source checkout is path, or nil.
func (inv *Inventory) repo(path string) *Repo {
	for i := range inv.Repos {
		if task.SamePath(inv.Repos[i].Path, path) {
			return &inv.Repos[i]
		}
	}
	return nil
}

// gitTop is the nearest of dir and the directories above it that has a
// .git, "" when none has.
func gitTop(dir string) string {
	for {
		if hasGit(dir) {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			return ""
		}
		dir = up
	}
}

// known reports whether dir is a source checkout or worktree found.
func (inv *Inventory) known(dir string) bool {
	for _, r := range inv.Repos {
		if task.SamePath(dir, r.Path) {
			return true
		}
		for _, w := range r.Worktrees {
			if task.SamePath(dir, w.Path) {
				return true
			}
		}
	}
	return false
}

// otherFolders are the worktree folder's folders that are no worktree: a
// directory <x> of it that is neither a worktree nor a source checkout is
// <repo name> when a repository found has that name or a worktree or
// source checkout is directly in it — then its directories <y> that are
// neither are listed with that name — else <x> itself is listed.
func (inv *Inventory) otherFolders() []wire.WorktreeFolder {
	folders := []wire.WorktreeFolder{}
	if inv.Folder == "" {
		return folders
	}
	for _, x := range subdirs(inv.Folder) {
		if inv.known(x) {
			continue
		}
		name := filepath.Base(x)
		ys := subdirs(x)
		isRepoDir := slices.ContainsFunc(ys, inv.known) || slices.ContainsFunc(inv.Repos, func(r Repo) bool {
			return filepath.Base(r.Path) == name
		})
		if !isRepoDir {
			folders = append(folders, wire.WorktreeFolder{Path: x})
			continue
		}
		for _, y := range ys {
			if !inv.known(y) {
				folders = append(folders, wire.WorktreeFolder{Path: y, Repo: name})
			}
		}
	}
	slices.SortFunc(folders, func(a, b wire.WorktreeFolder) int { return strings.Compare(a.Path, b.Path) })
	return folders
}

// subdirs lists the directories directly in dir (not links, not names
// starting with "."), as paths.
func subdirs(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if fi, err := e.Info(); err == nil && !task.IsLink(fi) {
				dirs = append(dirs, filepath.Join(dir, e.Name()))
			}
		}
	}
	return dirs
}

// hasGit reports whether dir has a .git (a file in a linked worktree).
func hasGit(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// Hit is what Find found at a path.
type Hit struct {
	Repo *Repo
	// Worktree is nil for the source checkout itself and for a folder.
	Worktree *Worktree
	Folder   *wire.WorktreeFolder
}

// Find looks path (canonical) up: a source checkout, a worktree or a
// folder; false when it is none.
func (inv *Inventory) Find(path string) (Hit, bool) {
	for i := range inv.Repos {
		r := &inv.Repos[i]
		if task.SamePath(r.Path, path) {
			return Hit{Repo: r}, true
		}
		for j := range r.Worktrees {
			if task.SamePath(r.Worktrees[j].Path, path) {
				return Hit{Repo: r, Worktree: &r.Worktrees[j]}, true
			}
		}
	}
	for i := range inv.Folders {
		if task.SamePath(inv.Folders[i].Path, path) {
			return Hit{Folder: &inv.Folders[i]}, true
		}
	}
	return Hit{}, false
}

// RepoPaths are the source checkouts found (worktree_repos), sorted.
func (inv *Inventory) RepoPaths() []string {
	paths := make([]string, 0, len(inv.Repos))
	for _, r := range inv.Repos {
		paths = append(paths, r.Path)
	}
	return paths
}

// Holds reports whether a source checkout or worktree found lies in dir.
func (inv *Inventory) Holds(dir string) bool {
	for _, r := range inv.Repos {
		if task.Within(r.Path, dir) || slices.ContainsFunc(r.Worktrees, func(w Worktree) bool { return task.Within(w.Path, dir) }) {
			return true
		}
	}
	return false
}

// TaskOf is the task (not in_place) whose worktree is path, or nil.
func TaskOf(tasks []wire.Task, path string) *wire.Task {
	for i := range tasks {
		if !tasks[i].InPlace && tasks[i].Worktree != "" && task.SamePath(tasks[i].Worktree, path) {
			return &tasks[i]
		}
	}
	return nil
}

// Inside reports whether session s works inside dir (its cwd).
func Inside(s wire.Session, dir string) bool {
	return s.Cwd != "" && task.Within(task.CanonicalPath(s.Cwd), dir)
}

// List describes inv's repositories for worktree.list: each worktree with
// its task, live sessions (live: cwd inside it, or its task's), changes
// (task.Changes, the shared directories' links not counted), unpushed
// commits and whether git sees its head merged into the default branch.
// Nothing takes git's optional locks.
func List(ctx context.Context, env []string, inv *Inventory, tasks []wire.Task, live []wire.Session) []wire.WorktreeRepo {
	env = noLocks(env)
	var shown []Repo
	for _, r := range inv.Repos {
		if r.listed() {
			shown = append(shown, r)
		}
	}
	repos := make([]wire.WorktreeRepo, len(shown))
	type ref struct{ r, w int }
	var refs []ref
	for i, r := range shown {
		repos[i] = wire.WorktreeRepo{
			Path: r.Path, Name: filepath.Base(r.Path), DefaultBranch: r.DefaultBranch, GitHub: r.GitHub,
			Worktrees: make([]wire.WorktreeEntry, len(r.Worktrees)),
		}
		for j := range r.Worktrees {
			refs = append(refs, ref{i, j})
		}
	}
	cwds := make([]string, len(live))
	for i, s := range live {
		if s.Cwd != "" {
			cwds[i] = task.CanonicalPath(s.Cwd)
		}
	}
	each(len(refs), func(k int) {
		r := &shown[refs[k].r]
		w := r.Worktrees[refs[k].w]
		e := wire.WorktreeEntry{
			Path: w.Path, Branch: w.Branch, Head: w.Head,
			Missing: w.Missing, Locked: w.Locked, LockReason: w.LockReason,
		}
		if t := TaskOf(tasks, w.Path); t != nil {
			e.TaskID = t.ID
		}
		for i, s := range live {
			if cwds[i] != "" && task.Within(cwds[i], w.Path) || e.TaskID != "" && s.TaskID == e.TaskID {
				e.Sessions = append(e.Sessions, s.ID)
			}
		}
		if !w.Missing {
			if changes, err := task.Changes(ctx, env, w.Path, task.ShareLinks(r.Path, w.Path)); err == nil {
				e.Changes = len(changes)
			}
		}
		e.Unpushed, _ = task.Unpushed(ctx, env, r.Path, w.Branch)
		e.Merged = merged(ctx, env, r.Path, r.DefaultBranch, w.Head)
		repos[refs[k].r].Worktrees[refs[k].w] = e
	})
	return repos
}

// merged reports whether head is an ancestor of the default branch:
// origin's, else the local branch of that name.
func merged(ctx context.Context, env []string, repo, def, head string) bool {
	if def == "" || head == "" {
		return false
	}
	ref := "refs/remotes/origin/" + def
	if !task.HasRef(ctx, env, repo, ref) {
		ref = "refs/heads/" + def
	}
	_, err := task.Git(ctx, env, repo, "merge-base", "--is-ancestor", head, ref)
	return err == nil
}

// noLocks is env (nil: this process's) with git's optional locks off, so
// git status never writes the index of a worktree an agent works in.
func noLocks(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	return append(slices.Clip(env), "GIT_OPTIONAL_LOCKS=0")
}

// each runs fn(0..n-1), at most parallel at once.
func each(n int, fn func(i int)) {
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range n {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i)
		})
	}
	wg.Wait()
}

// keyOf is the comparable form of a canonical path (case-insensitive on
// Windows), as task.SamePath compares.
func keyOf(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}
