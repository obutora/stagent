package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// fixture is a directory of repositories: base/origin-<name>.git (bare,
// main holding a.txt) cloned to base/src/<name>, the source checkout.
type fixture struct {
	base string
	env  []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{base: base, env: append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")}
}

func (f *fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo makes the repository name and returns its source checkout.
func (f *fixture) repo(t *testing.T, name string) string {
	t.Helper()
	seed := filepath.Join(f.base, "seed-"+name)
	f.git(t, f.base, "init", "-q", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "a.txt"), "a\n")
	f.git(t, seed, "add", "a.txt")
	f.git(t, seed, "commit", "-q", "-m", "init")
	origin := filepath.Join(f.base, "origin-"+name+".git")
	f.git(t, f.base, "clone", "-q", "--bare", seed, origin)
	repo := filepath.Join(f.base, "src", name)
	f.git(t, f.base, "clone", "-q", origin, repo)
	return repo
}

// worktree adds a worktree of repo at path on a new branch from
// origin/main (no upstream).
func (f *fixture) worktree(t *testing.T, repo, path, branch string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f.git(t, repo, "worktree", "add", "-q", "--no-track", "-b", branch, path, "origin/main")
}

func writeFile(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func repoPaths(inv *Inventory) []string {
	var paths []string
	for _, r := range inv.Repos {
		paths = append(paths, r.Path)
	}
	return paths
}

func (inv *Inventory) repoAt(t *testing.T, path string) Repo {
	t.Helper()
	for _, r := range inv.Repos {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no repository %s in %q", path, repoPaths(inv))
	return Repo{}
}

func worktreesByPath(r Repo) map[string]Worktree {
	m := map[string]Worktree{}
	for _, w := range r.Worktrees {
		m[w.Path] = w
	}
	return m
}

// Every worktree git knows is found, tasks' or not: a repository through
// a task record, one only through a watch folder's worktree (Orca's
// ~/orca/*), one only through a session's cwd in its worktree, one only
// through worktree_repos; a remembered checkout that is gone is dropped
// silently. A missing worktree is listed and stays registered; a locked
// one keeps its reason. The worktree folder's other folders are listed by
// the <repo name> rule; folders outside it, hidden ones and files are not.
func TestDiscover(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	app, lib, tool, old := f.repo(t, "app"), f.repo(t, "lib"), f.repo(t, "tool"), f.repo(t, "old")
	folder := filepath.Join(f.base, "worktrees")
	taskWT := filepath.Join(folder, "app", "5-fix")
	locked := filepath.Join(folder, "app", "locked")
	gone := filepath.Join(folder, "app", "gone")
	f.worktree(t, app, taskWT, "5-fix")
	f.worktree(t, app, locked, "locked")
	f.git(t, app, "worktree", "lock", "--reason", "agent at work", locked)
	f.worktree(t, app, gone, "gone")
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	orca := filepath.Join(f.base, "orca", "wt-lib")
	f.worktree(t, lib, orca, "feat/lib")
	elsewhere := filepath.Join(f.base, "elsewhere", "wt-tool")
	f.worktree(t, tool, elsewhere, "tool-x")
	mkdir(t, filepath.Join(elsewhere, "sub"))
	oldWT := filepath.Join(f.base, "old-wt")
	f.worktree(t, old, oldWT, "o")
	f.git(t, oldWT, "checkout", "-q", "--detach")
	notes := filepath.Join(folder, "app", "1108-notes")
	writeFile(t, filepath.Join(notes, "notes.md"), "n")
	loose := filepath.Join(folder, "loose")
	mkdir(t, loose)
	mkdir(t, filepath.Join(folder, ".cache"))
	writeFile(t, filepath.Join(folder, "README"), "r")
	mkdir(t, filepath.Join(f.base, "orca", "notes"))

	inv, err := Discover(ctx, Sources{
		Env:      f.env,
		Folder:   folder,
		Watch:    []string{filepath.Join(f.base, "orca")},
		Repos:    []string{old, filepath.Join(f.base, "src", "vanished")},
		Tasks:    []wire.Task{{ID: "t5", Repo: app, Worktree: taskWT, Branch: "5-fix"}},
		Sessions: []wire.Session{{ID: "00000000000000a1", Cwd: filepath.Join(elsewhere, "sub")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{app, lib, old, tool}; !slices.Equal(repoPaths(inv), want) {
		t.Fatalf("repos %q, want %q", repoPaths(inv), want)
	}
	if inv.Folder != folder {
		t.Fatalf("folder %q", inv.Folder)
	}
	apps := worktreesByPath(inv.repoAt(t, app))
	if len(apps) != 3 {
		t.Fatalf("app worktrees %+v", apps)
	}
	if w := apps[taskWT]; w.Branch != "5-fix" || w.Missing || w.Locked || len(w.Head) != 40 {
		t.Fatalf("task worktree %+v", w)
	}
	if w := apps[locked]; !w.Locked || w.LockReason != "agent at work" || w.Missing {
		t.Fatalf("locked worktree %+v", w)
	}
	if w := apps[gone]; !w.Missing || w.Branch != "gone" {
		t.Fatalf("missing worktree %+v", w)
	}
	if ws := inv.repoAt(t, lib).Worktrees; len(ws) != 1 || ws[0].Path != orca || ws[0].Branch != "feat/lib" {
		t.Fatalf("lib worktrees %+v", ws)
	}
	if ws := inv.repoAt(t, tool).Worktrees; len(ws) != 1 || ws[0].Path != elsewhere {
		t.Fatalf("tool worktrees %+v", ws)
	}
	if ws := inv.repoAt(t, old).Worktrees; len(ws) != 1 || ws[0].Path != oldWT || ws[0].Branch != "" {
		t.Fatalf("old worktrees %+v", ws)
	}
	wantFolders := []wire.WorktreeFolder{{Path: notes, Repo: "app"}, {Path: loose, Repo: ""}}
	if !slices.Equal(inv.Folders, wantFolders) {
		t.Fatalf("folders %+v, want %+v", inv.Folders, wantFolders)
	}
	// Listing pruned nothing (git prints / on Windows too).
	if out := f.git(t, app, "worktree", "list", "--porcelain"); !strings.Contains(out, "worktree "+filepath.ToSlash(gone)+"\n") {
		t.Fatalf("the missing worktree was pruned:\n%s", out)
	}
}

// A folder of the worktree folder that does not have a repository's name
// still is <repo name> when a worktree is directly in it; without a
// worktree folder there are no folders.
func TestDiscoverFolderOfWorktrees(t *testing.T) {
	f := newFixture(t)
	app := f.repo(t, "app")
	folder := filepath.Join(f.base, "worktrees")
	wt := filepath.Join(folder, "renamed", "x")
	f.worktree(t, app, wt, "x")
	left := filepath.Join(folder, "renamed", "left")
	mkdir(t, left)
	inv, err := Discover(context.Background(), Sources{Env: f.env, Folder: folder, Repos: []string{app}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []wire.WorktreeFolder{{Path: left, Repo: "renamed"}}; !slices.Equal(inv.Folders, want) {
		t.Fatalf("folders %+v, want %+v", inv.Folders, want)
	}
	inv, err = Discover(context.Background(), Sources{Env: f.env, Repos: []string{app}})
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Folders) != 0 || len(inv.Repos) != 1 {
		t.Fatalf("without a worktree folder: %+v", inv)
	}
}

// A repository around the others (dotfiles kept in git at ~), found
// through a session's cwd and remembered, hides none of the repositories
// inside it. Without linked worktrees it is not listed, so its checkout is
// never measured.
func TestDiscoverEnclosingRepository(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	app := f.repo(t, "app")
	wt := filepath.Join(f.base, "orca", "wt-app")
	f.worktree(t, app, wt, "x")
	f.git(t, f.base, "init", "-q", "-b", "main")
	src := Sources{
		Env:      f.env,
		Repos:    []string{f.base},
		Sessions: []wire.Session{{ID: "00000000000000a1", Cwd: f.base}, {ID: "00000000000000a2", Cwd: filepath.Join(wt)}},
	}
	inv, err := Discover(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.base, app}; !slices.Equal(inv.RepoPaths(), want) {
		t.Fatalf("found %q, want %q", inv.RepoPaths(), want)
	}
	repos := List(ctx, f.env, inv, nil, nil)
	if len(repos) != 1 || repos[0].Path != app || len(repos[0].Worktrees) != 1 || repos[0].Worktrees[0].Path != wt {
		t.Fatalf("listed %+v", repos)
	}
	for _, e := range Sizes(ctx, inv).Entries {
		if e.Path == f.base {
			t.Fatalf("measured the enclosing repository: %+v", e)
		}
	}
}

// A repository with no remote still has git's merge check: its local main
// or master is the default branch.
func TestListMergedWithoutRemote(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	local := filepath.Join(f.base, "local")
	f.git(t, f.base, "init", "-q", "-b", "master", local)
	f.git(t, local, "commit", "-q", "--allow-empty", "-m", "init")
	done := filepath.Join(f.base, "wt", "done")
	open := filepath.Join(f.base, "wt", "open")
	f.git(t, local, "worktree", "add", "-q", "-b", "done", done, "master")
	f.git(t, local, "worktree", "add", "-q", "-b", "open", open, "master")
	f.git(t, open, "commit", "-q", "--allow-empty", "-m", "work")
	inv, err := Discover(ctx, Sources{Env: f.env, Repos: []string{local}})
	if err != nil {
		t.Fatal(err)
	}
	repos := List(ctx, f.env, inv, nil, nil)
	if len(repos) != 1 || repos[0].DefaultBranch != "master" || len(repos[0].Worktrees) != 2 {
		t.Fatalf("repos %+v", repos)
	}
	for _, w := range repos[0].Worktrees {
		if want := w.Path == done; w.Merged != want {
			t.Fatalf("%s merged %v, want %v", w.Path, w.Merged, want)
		}
	}
}

// List gives each worktree its task, live sessions (inside it, or its
// task's), uncommitted changes, unpushed commits and whether git sees it
// merged into the default branch; a repository with a github.com remote is
// marked. The source checkout is not one of the worktrees.
func TestList(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	app, lib := f.repo(t, "app"), f.repo(t, "lib")
	f.git(t, app, "remote", "add", "upstream", "https://github.com/o/app.git")
	clean := filepath.Join(f.base, "wt", "clean")
	busy := filepath.Join(f.base, "wt", "busy")
	gone := filepath.Join(f.base, "wt", "gone")
	f.worktree(t, app, clean, "clean")
	f.worktree(t, app, busy, "busy")
	f.worktree(t, app, gone, "gone")
	f.git(t, busy, "commit", "-q", "--allow-empty", "-m", "work")
	writeFile(t, filepath.Join(busy, "a.txt"), "changed\n")
	writeFile(t, filepath.Join(busy, "new.txt"), "new\n")
	mkdir(t, filepath.Join(busy, "sub"))
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	inv, err := Discover(ctx, Sources{Env: f.env, Repos: []string{app, lib}})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []wire.Task{
		{ID: "t1", Repo: app, Worktree: clean},
		{ID: "t2", Repo: app, Worktree: app, InPlace: true},
	}
	live := []wire.Session{
		{ID: "00000000000000a1", Cwd: filepath.Join(busy, "sub")},
		{ID: "00000000000000a2", Cwd: f.base, TaskID: "t1"},
		{ID: "00000000000000a3", Cwd: app, TaskID: "t2"},
	}
	repos := List(ctx, f.env, inv, tasks, live)
	if len(repos) != 1 {
		t.Fatalf("repos %+v (lib has no worktrees: not listed)", repos)
	}
	a := repos[0]
	if a.Path != app || a.Name != "app" || a.DefaultBranch != "main" || !a.GitHub {
		t.Fatalf("app %+v", a)
	}
	got := map[string]wire.WorktreeEntry{}
	for _, w := range a.Worktrees {
		got[w.Path] = w
	}
	if len(got) != 3 {
		t.Fatalf("app worktrees %+v", a.Worktrees)
	}
	if w := got[clean]; w.TaskID != "t1" || !slices.Equal(w.Sessions, []string{"00000000000000a2"}) ||
		w.Changes != 0 || w.Unpushed != 0 || !w.Merged || w.Branch != "clean" {
		t.Fatalf("clean %+v", w)
	}
	if w := got[busy]; w.TaskID != "" || !slices.Equal(w.Sessions, []string{"00000000000000a1"}) ||
		w.Changes != 2 || w.Unpushed != 1 || w.Merged {
		t.Fatalf("busy %+v", w)
	}
	if w := got[gone]; !w.Missing || w.Changes != 0 || len(w.Sessions) != 0 {
		t.Fatalf("gone %+v", w)
	}
}
