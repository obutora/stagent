//go:build !windows

package task

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/gittest"
	"github.com/obutora/stagent/internal/wire"
)

// stubGHPRView stands in for gh in removals: `pr view <branch|number>
// --json state,headRefOid` prints $GH_PR_VIEW, or fails like gh when there
// is no pull request: $GH_PR_VIEW is empty, or $GH_PR_FOR is set and names
// another branch or number (gh finds a fork's pull request by number only).
const stubGHPRView = `#!/bin/sh
[ "$1 $2" = "pr view" ] || { echo "stub gh: unexpected $*" >&2; exit 1; }
if [ -z "$GH_PR_VIEW" ] || { [ -n "$GH_PR_FOR" ] && [ "$3" != "$GH_PR_FOR" ]; }; then
  echo "no pull requests found for branch \"$3\"" >&2
  exit 1
fi
printf '%s\n' "$GH_PR_VIEW"
`

const sharedOrca = "worktree:\n  sharedDirectories:\n    - node_modules\n"

// newRemovable makes a fixture whose origin holds files (orca.yaml
// defaults to sharedOrca) plus a .gitignore of node_modules/ and .env, and
// a worktree task of its checkout on branch 5-fix (from origin/main, no
// upstream) with the shared directory node_modules linked to the
// checkout's. gh answers pr view with prView.
func newRemovable(t *testing.T, files map[string]string, prView string) (*fixture, wire.Task) {
	t.Helper()
	all := map[string]string{".gitignore": "node_modules/\n.env\n", "a.txt": "a\n", "orca.yaml": sharedOrca}
	for k, v := range files {
		all[k] = v
	}
	f := newFixture(t, all)
	if err := os.WriteFile(filepath.Join(f.base, "bin", "gh"), []byte(stubGHPRView), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "GH_PR_VIEW="+prView)
	tk := wire.Task{ID: "t", Repo: f.repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Base: "main", Worktree: filepath.Join(f.base, "repo-src-5")}
	git(t, f.repo, "worktree", "add", "-q", "--no-track", "-b", tk.Branch, tk.Worktree, "origin/main")
	shared := filepath.Join(f.repo, "node_modules")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "keep.js"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(tk.Worktree, "node_modules")); err != nil {
		t.Fatal(err)
	}
	return f, tk
}

func (f *fixture) remove(t *testing.T, r Removal) ([]string, string, string, error) {
	t.Helper()
	var stages []string
	var out bytes.Buffer
	r.Env, r.Out = f.env, &out
	r.Stage = func(s string) { stages = append(stages, s) }
	kept, err := Remove(context.Background(), r)
	return stages, kept, out.String(), err
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) listed(t *testing.T, worktree string) bool {
	t.Helper()
	w, err := WorktreeAt(context.Background(), f.env, f.repo, worktree)
	if err != nil {
		t.Fatal(err)
	}
	return w != nil
}

// Changes are tracked files changed, staged or not, and untracked files
// git does not ignore, as git status --porcelain prints them; an ignored
// .env and the shared directory's link are none.
func TestChanges(t *testing.T) {
	f, tk := newRemovable(t, map[string]string{"orca.yaml": sharedOrca, "b.txt": "b\n"}, "")
	wt := tk.Worktree
	write(t, filepath.Join(wt, "a.txt"), "changed\n")
	write(t, filepath.Join(wt, "staged.txt"), "s\n")
	git(t, wt, "add", "staged.txt")
	git(t, wt, "mv", "b.txt", "c.txt")
	write(t, filepath.Join(wt, "new.txt"), "n\n")
	write(t, filepath.Join(wt, ".env"), "SECRET=1\n")
	ctx := context.Background()
	links := ShareLinks(f.repo, wt)
	if !slices.Equal(links, []string{"node_modules"}) {
		t.Fatalf("ShareLinks = %q", links)
	}
	got, err := Changes(ctx, f.env, wt, links)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{" M a.txt", "R  b.txt -> c.txt", "A  staged.txt", "?? new.txt"}; !slices.Equal(got, want) {
		t.Fatalf("Changes = %q, want %q", got, want)
	}
	// Without its name the link would be a change.
	if got, _ := Changes(ctx, f.env, wt, nil); !slices.Contains(got, "?? node_modules") {
		t.Fatalf("Changes without links = %q", got)
	}
}

// addSubmodule makes a repository with one file, sub.txt, and checks it
// out as submodule sub of worktree, committed; it returns its path there.
func addSubmodule(t *testing.T, worktree string) string {
	t.Helper()
	_, src := newRepo(t)
	write(t, filepath.Join(src, "sub.txt"), "s\n")
	git(t, src, "add", "sub.txt")
	git(t, src, "commit", "-q", "-m", "sub")
	git(t, worktree, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "sub")
	git(t, worktree, "commit", "-q", "-m", "submodule")
	return filepath.Join(worktree, "sub")
}

// Changes do not depend on the user's git settings: untracked files count
// with status.showUntrackedFiles=no, each one listed, and a submodule's
// changes count whatever submodule.<name>.ignore and diff.ignoreSubmodules
// say.
func TestChangesIgnoreUserConfig(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	wt := tk.Worktree
	sub := addSubmodule(t, wt)
	git(t, wt, "config", "status.showUntrackedFiles", "no")
	git(t, wt, "config", "submodule.sub.ignore", "all")
	git(t, wt, "config", "diff.ignoreSubmodules", "all")
	ctx := context.Background()
	links := ShareLinks(f.repo, wt)
	if got, err := Changes(ctx, f.env, wt, links); err != nil || len(got) != 0 {
		t.Fatalf("clean worktree: %q, %v", got, err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "dir", "new.txt"), "n\n")
	got, err := Changes(ctx, f.env, wt, links)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"?? dir/new.txt"}; !slices.Equal(got, want) {
		t.Fatalf("Changes with showUntrackedFiles=no = %q, want %q", got, want)
	}
	write(t, filepath.Join(sub, "sub.txt"), "changed\n")
	if got, _ := Changes(ctx, f.env, wt, links); !slices.Contains(got, " M sub") {
		t.Fatalf("Changes with a changed submodule = %q, want \" M sub\"", got)
	}
}

// Changes take none of git's optional locks: the index of a worktree an
// agent works in is not refreshed (written under index.lock) however
// stale its stat data is.
func TestChangesTakeNoLock(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	wt := tk.Worktree
	out, err := Git(context.Background(), f.env, wt, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	index := strings.TrimSpace(string(out))
	before := readFile(t, index)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(wt, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := Changes(context.Background(), f.env, wt, nil); err != nil {
			t.Fatal(err)
		}
	}
	if readFile(t, index) != before {
		t.Fatal("Changes rewrote the index")
	}
}

// A worktree with a submodule, which git worktree remove refuses, and a
// worktree the user locked are refused (submodules, locked); the lock of
// a killed git worktree add is no refusal for a task's worktree (ours),
// but is for another tool's, whose add may still be at work.
func TestRefusalsSubmodulesAndLock(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	ctx := context.Background()
	refusals := func(ours bool) ([]wire.RemoveRefusal, error) {
		ws, err := ListWorktrees(ctx, f.env, f.repo)
		if err != nil {
			t.Fatal(err)
		}
		return Refusals(ctx, f.env, ws, tk.Worktree, ours)
	}
	locked := []wire.RemoveRefusal{{Kind: wire.RefusalLocked}}
	git(t, f.repo, "worktree", "lock", "--reason", AddingLock, tk.Worktree)
	if got, err := refusals(true); err != nil || len(got) != 0 {
		t.Fatalf("half-made lock of a task's worktree: %+v, %v", got, err)
	}
	if got, err := refusals(false); err != nil || !reflect.DeepEqual(got, locked) {
		t.Fatalf("half-made lock of another tool's worktree: %+v, %v", got, err)
	}
	git(t, f.repo, "worktree", "unlock", tk.Worktree)
	sub := addSubmodule(t, tk.Worktree)
	git(t, f.repo, "worktree", "lock", "--reason", "mine", tk.Worktree)
	got, err := refusals(true)
	want := []wire.RemoveRefusal{{Kind: wire.RefusalSubmodules, Paths: []string{CanonicalPath(sub)}}, {Kind: wire.RefusalLocked}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Refusals = %+v, %v, want %+v", got, err, want)
	}
}

// Unpushed counts the branch's commits on no remote.
func TestUnpushed(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	ctx := context.Background()
	if n, err := Unpushed(ctx, f.env, f.repo, tk.Branch); err != nil || n != 0 {
		t.Fatalf("fresh branch: %d, %v", n, err)
	}
	git(t, tk.Worktree, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, tk.Worktree, "commit", "-q", "--allow-empty", "-m", "two")
	if n, err := Unpushed(ctx, f.env, f.repo, tk.Branch); err != nil || n != 2 {
		t.Fatalf("two commits: %d, %v", n, err)
	}
	if n, err := Unpushed(ctx, f.env, f.repo, "no-such-branch"); err != nil || n != 0 {
		t.Fatalf("no branch: %d, %v", n, err)
	}
}

// archive runs in the worktree with the source checkout's orca.yaml; then
// only the link of the shared directory goes (what it points to stays),
// the worktree is removed and its merged branch deleted.
func TestRemove(t *testing.T) {
	orca := sharedOrca + "scripts:\n  archive: |\n    pwd > \"$ORCA_ROOT_PATH/archive-ran\"\n"
	f, tk := newRemovable(t, map[string]string{"orca.yaml": orca}, "")
	write(t, filepath.Join(tk.Worktree, ".env"), "SECRET=1\n")
	stages, kept, out, err := f.remove(t, Removal{Task: tk, Archive: true})
	if err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if want := []string{"archive", "worktree", "branch"}; !slices.Equal(stages, want) {
		t.Fatalf("stages %q, want %q", stages, want)
	}
	if kept != "" {
		t.Fatalf("kept %q", kept)
	}
	if got := readFile(t, filepath.Join(f.repo, "archive-ran")); got != tk.Worktree+"\n" {
		t.Fatalf("archive ran in %q", got)
	}
	if got := readFile(t, filepath.Join(f.repo, "node_modules", "keep.js")); got != "kept" {
		t.Fatalf("shared directory content %q", got)
	}
	if _, err := os.Lstat(tk.Worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if f.listed(t, tk.Worktree) {
		t.Fatal("git still lists the worktree")
	}
	if HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch) {
		t.Fatal("merged branch kept")
	}
}

// A scripts.archive that removes the worktree itself (git worktree remove)
// leaves the worktree stage nothing to do: git is asked again after it, so
// the removal still succeeds, and the branch stage decides the branch as
// always — an unmerged one is kept.
func TestRemoveArchiveRemovesWorktree(t *testing.T) {
	orca := sharedOrca + "scripts:\n  archive: |\n    git -C \"$ORCA_ROOT_PATH\" worktree remove --force \"$PWD\"\n"
	f, tk := newRemovable(t, map[string]string{"orca.yaml": orca}, "")
	git(t, tk.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
	stages, kept, out, err := f.remove(t, Removal{Task: tk, Archive: true})
	if err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if want := []string{"archive", "worktree", "branch"}; !slices.Equal(stages, want) {
		t.Fatalf("stages %q, want %q", stages, want)
	}
	if kept != tk.Branch || !HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch) {
		t.Fatalf("kept %q, want the unmerged %s kept\n%s", kept, tk.Branch, out)
	}
	if f.listed(t, tk.Worktree) || IsDir(tk.Worktree) {
		t.Fatal("worktree left")
	}
}

// A failing archive stops the removal before anything is removed.
func TestRemoveArchiveFails(t *testing.T) {
	f, tk := newRemovable(t, map[string]string{"orca.yaml": "scripts:\n  archive: exit 3\n"}, "")
	_, _, out, err := f.remove(t, Removal{Task: tk, Archive: true})
	if stageOf(err) != wire.TaskStageArchive || !strings.Contains(err.Error(), "exited with code 3") {
		t.Fatalf("Remove = %v\n%s", err, out)
	}
	if !f.listed(t, tk.Worktree) || !IsDir(tk.Worktree) {
		t.Fatal("worktree removed after archive failed")
	}
}

// Changes stop git worktree remove (git's error) unless forced.
func TestRemoveForce(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	write(t, filepath.Join(tk.Worktree, "a.txt"), "changed\n")
	_, _, out, err := f.remove(t, Removal{Task: tk})
	if stageOf(err) != wire.TaskStageWorktree || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("Remove = %v\n%s", err, out)
	}
	if !f.listed(t, tk.Worktree) {
		t.Fatal("worktree removed without force")
	}
	if _, _, out, err := f.remove(t, Removal{Task: tk, Force: true}); err != nil {
		t.Fatalf("forced Remove: %v\n%s", err, out)
	}
	if f.listed(t, tk.Worktree) || IsDir(tk.Worktree) {
		t.Fatal("forced Remove left the worktree")
	}
}

// A worktree a killed `git worktree add` left (no files, locked
// "initializing") is removed without archive (nothing was checked out to
// archive) or force, and its branch goes as usual. A worktree the user
// locked is refused (locked) before git is asked, forced or not.
func TestRemoveKilledAdd(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "a\n", "orca.yaml": "scripts:\n  archive: exit 3\n"})
	tk := f.task(t, wire.TaskKindIssue, 5, "5-fix")
	git(t, f.repo, "branch", "-q", "--no-track", tk.Branch, "origin/main")
	gittest.KilledAdd(t, f.repo, tk.Worktree, tk.Branch)
	stages, kept, out, err := f.remove(t, Removal{Task: tk, Archive: true})
	if err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if want := []string{"worktree", "branch"}; !slices.Equal(stages, want) || kept != "" {
		t.Fatalf("stages %q, kept %q", stages, kept)
	}
	if f.listed(t, tk.Worktree) || IsDir(tk.Worktree) || HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch) {
		t.Fatalf("worktree or branch left:\n%s", out)
	}

	f, tk = newRemovable(t, nil, "")
	git(t, f.repo, "worktree", "lock", "--reason", "agent at work", tk.Worktree)
	var refused *RefusedError
	if _, _, out, err := f.remove(t, Removal{Task: tk, Force: true}); stageOf(err) != wire.TaskStageWorktree || !errors.As(err, &refused) ||
		!reflect.DeepEqual(refused.Refusals, []wire.RemoveRefusal{{Kind: wire.RefusalLocked}}) {
		t.Fatalf("Remove of a locked worktree = %v\n%s", err, out)
	}
	if !f.listed(t, tk.Worktree) || !IsDir(tk.Worktree) {
		t.Fatal("the locked worktree went")
	}
}

// A worktree whose directory is gone is unregistered alone: another
// worktree whose directory is gone too, and a locked one, stay. One git
// no longer lists unregisters nothing.
func TestRemoveGoneWorktreeUnregistersItAlone(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	other, locked := f.missingWorktrees(t)
	if err := os.RemoveAll(tk.Worktree); err != nil {
		t.Fatal(err)
	}
	if _, _, out, err := f.remove(t, Removal{Task: tk}); err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if f.listed(t, tk.Worktree) || !f.listed(t, other) || !f.listed(t, locked) {
		t.Fatalf("worktree list:\n%s", f.gitOut(t, f.repo, "worktree", "list"))
	}
	if _, _, out, err := f.remove(t, Removal{Task: tk}); err != nil {
		t.Fatalf("Remove again: %v\n%s", err, out)
	}
	if !f.listed(t, other) || !f.listed(t, locked) {
		t.Fatalf("worktree list:\n%s", f.gitOut(t, f.repo, "worktree", "list"))
	}
}

// A git before 2.18 cannot remove one worktree whose directory is gone:
// it is pruned, as before, which still keeps a locked one.
func TestRemoveGoneWorktreeOldGitPrunes(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	f.oldGit(t)
	other, locked := f.missingWorktrees(t)
	if err := os.RemoveAll(tk.Worktree); err != nil {
		t.Fatal(err)
	}
	if _, _, out, err := f.remove(t, Removal{Task: tk}); err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if f.listed(t, tk.Worktree) || f.listed(t, other) || !f.listed(t, locked) {
		t.Fatalf("worktree list:\n%s", f.gitOut(t, f.repo, "worktree", "list"))
	}
}

// A branch git does not see merged (a squash merge) is deleted when gh says
// its pull request was merged at the branch's own commit; otherwise — more
// commits than the merged PR, or no PR — it is kept. A PR task asks gh by
// its number, which finds a fork's pull request; the branch name does not.
func TestRemoveBranch(t *testing.T) {
	for _, c := range []struct {
		name     string
		pr       bool   // a PR task (number 5)
		ghFor    string // the only branch or number gh answers for
		extra    bool   // a commit after the one the PR merged
		noPR     bool
		wantKept bool
	}{
		{name: "squash merged", ghFor: "5-fix"},
		{name: "fork pull request squash merged", pr: true, ghFor: "5"},
		{name: "commits after the merge", extra: true, wantKept: true},
		{name: "no pull request", noPR: true, wantKept: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, tk := newRemovable(t, nil, "")
			if c.pr {
				tk.Kind = wire.TaskKindPR
			}
			f.env = append(f.env, "GH_PR_FOR="+c.ghFor)
			git(t, tk.Worktree, "commit", "-q", "--allow-empty", "-m", "work")
			head := f.gitOut(t, tk.Worktree, "rev-parse", "HEAD")
			if c.extra {
				git(t, tk.Worktree, "commit", "-q", "--allow-empty", "-m", "more")
			}
			if !c.noPR {
				f.env = append(f.env, `GH_PR_VIEW={"state":"MERGED","headRefOid":"`+head+`"}`)
			}
			_, kept, out, err := f.remove(t, Removal{Task: tk})
			if err != nil {
				t.Fatalf("Remove: %v\n%s", err, out)
			}
			has := HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch)
			if c.wantKept {
				if kept != tk.Branch || !has {
					t.Fatalf("kept %q (branch there: %v), want %q\n%s", kept, has, tk.Branch, out)
				}
			} else if kept != "" || has {
				t.Fatalf("kept %q (branch there: %v), want deleted\n%s", kept, has, out)
			}
		})
	}
}

// wrapGit is a git first on PATH that runs $REAL_GIT. With GIT_WRAP_OLD
// set, merge-tree fails as a git before 2.38 does on --write-tree. With
// GIT_WRAP_MOVE="<ref> <commit>" set, the first merge-tree moves that ref
// there once it has run: a push landing between the check and the delete.
const wrapGit = `#!/bin/sh
if [ "$1" = merge-tree ]; then
  if [ -n "$GIT_WRAP_OLD" ]; then
    echo "fatal: unknown rev --write-tree" >&2
    exit 128
  fi
  "$REAL_GIT" "$@"; s=$?
  if [ -n "$GIT_WRAP_MOVE" ] && mkdir "$GIT_WRAP_MOVED" 2>/dev/null; then
    env -u GIT_OBJECT_DIRECTORY -u GIT_ALTERNATE_OBJECT_DIRECTORIES "$REAL_GIT" update-ref $GIT_WRAP_MOVE
  fi
  exit $s
fi
exec "$REAL_GIT" "$@"
`

// wrapGit puts wrapGit first on the fixture's PATH.
func (f *fixture) wrapGit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.base, "bin", "git"), []byte(wrapGit), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = append(f.env, "REAL_GIT="+real, "GIT_WRAP_MOVED="+filepath.Join(f.base, "moved"))
}

// work commits two changes to the task's branch — b.txt added, a.txt
// changed — and pushes it to origin (no upstream), returning its head.
func (f *fixture) work(t *testing.T, tk wire.Task) string {
	t.Helper()
	write(t, filepath.Join(tk.Worktree, "b.txt"), "b\n")
	git(t, tk.Worktree, "add", "b.txt")
	git(t, tk.Worktree, "commit", "-q", "-m", "work 1")
	write(t, filepath.Join(tk.Worktree, "a.txt"), "a\nfrom the branch\n")
	git(t, tk.Worktree, "commit", "-q", "-am", "work 2")
	git(t, tk.Worktree, "push", "-q", "origin", tk.Branch)
	return f.gitOut(t, tk.Worktree, "rev-parse", "HEAD")
}

// mergeOnOrigin merges origin's branch into origin's base from another
// clone, as a forge's merge button does — "squash": one new commit with
// the branch's changes; "rebase": the branch's commits replayed on base,
// which moved on first — then, with later, changes the line the branch
// changed in a.txt on base again. The source checkout does not fetch.
func (f *fixture) mergeOnOrigin(t *testing.T, branch, base, how string, later bool) {
	t.Helper()
	other := filepath.Join(f.base, "merger")
	git(t, f.base, "clone", "-q", filepath.Join(f.base, "origin.git"), other)
	if base != "main" {
		git(t, other, "switch", "-q", "-c", base)
	}
	switch how {
	case "squash":
		git(t, other, "merge", "-q", "--squash", "origin/"+branch)
		git(t, other, "commit", "-q", "-m", "squash")
	case "rebase":
		write(t, filepath.Join(other, "other.txt"), "other\n")
		git(t, other, "add", "other.txt")
		git(t, other, "commit", "-q", "-m", "base moves on")
		git(t, other, "switch", "-q", "-c", "replay", "origin/"+branch)
		git(t, other, "rebase", "-q", base)
		git(t, other, "switch", "-q", base)
		git(t, other, "merge", "-q", "--ff-only", "replay")
	}
	if later {
		write(t, filepath.Join(other, "a.txt"), "a\nchanged on base later\n")
		git(t, other, "commit", "-q", "-am", "later")
	}
	git(t, other, "push", "-q", "origin", base)
}

// A branch squash- or rebase-merged on origin is deleted without gh (none
// answers): git finds its changes in the task's base once fetched from
// origin, the base having changed the same line since included. One with a
// commit after the merge is kept and named. Merged into the local base,
// nothing needs fetching (offline). Remote branches stay.
func TestRemoveBranchMergedByGit(t *testing.T) {
	for _, c := range []struct {
		name     string
		how      string // "squash", "rebase"
		base     string // the task's base ("": main)
		later    bool   // base changed the branch's line after the merge
		extra    bool   // a commit on the branch after the merge
		local    bool   // merged into the local base, origin gone
		wantKept bool
	}{
		{name: "squash merged", how: "squash"},
		{name: "rebase merged", how: "rebase"},
		{name: "squash merged into the task's base", how: "squash", base: "release"},
		{name: "squash merged then changed on base", how: "squash", later: true},
		{name: "rebase merged then changed on base", how: "rebase", later: true},
		{name: "squash merged locally, offline", how: "squash", local: true},
		{name: "commits after the squash merge", how: "squash", extra: true, wantKept: true},
		{name: "commits after the rebase merge", how: "rebase", extra: true, wantKept: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, tk := newRemovable(t, nil, "")
			tk.Base = cmp.Or(c.base, "main")
			pushed := f.work(t, tk)
			origin := filepath.Join(f.base, "origin.git")
			if c.local {
				git(t, f.repo, "merge", "-q", "--squash", tk.Branch)
				git(t, f.repo, "commit", "-q", "-m", "squash")
				if err := os.Rename(origin, origin+".away"); err != nil {
					t.Fatal(err)
				}
			} else {
				f.mergeOnOrigin(t, tk.Branch, tk.Base, c.how, c.later)
			}
			if c.extra {
				write(t, filepath.Join(tk.Worktree, "b.txt"), "b\nmore\n")
				git(t, tk.Worktree, "commit", "-q", "-am", "more")
			}
			_, kept, out, err := f.remove(t, Removal{Task: tk})
			if err != nil {
				t.Fatalf("Remove: %v\n%s", err, out)
			}
			has := HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch)
			if c.wantKept {
				if kept != tk.Branch || !has {
					t.Fatalf("kept %q (branch there: %v), want %q\n%s", kept, has, tk.Branch, out)
				}
			} else if kept != "" || has {
				t.Fatalf("kept %q (branch there: %v), want deleted\n%s", kept, has, out)
			}
			if got := f.gitOut(t, f.repo, "rev-parse", "refs/remotes/origin/"+tk.Branch); got != pushed {
				t.Fatalf("origin/%s is %s, want %s", tk.Branch, got, pushed)
			}
			if !c.local {
				if got := f.gitOut(t, origin, "rev-parse", "refs/heads/"+tk.Branch); got != pushed {
					t.Fatalf("origin's %s is %s, want %s", tk.Branch, got, pushed)
				}
			}
		})
	}
}

// A branch that moves after git or gh found it merged — a commit landing
// between the check and the delete — is kept at its new commit and named.
func TestRemoveBranchMovedAfterCheck(t *testing.T) {
	for _, by := range []string{"git", "gh"} {
		t.Run(by, func(t *testing.T) {
			f, tk := newRemovable(t, nil, "")
			f.env = append(f.env, "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t")
			head := f.work(t, tk)
			late := f.gitOut(t, f.repo, "commit-tree", "-p", head, "-m", "late", head+"^{tree}")
			ref := "refs/heads/" + tk.Branch
			if by == "git" {
				f.mergeOnOrigin(t, tk.Branch, "main", "squash", false)
				git(t, f.repo, "fetch", "-q", "origin")
				f.wrapGit(t)
				f.env = append(f.env, "GIT_WRAP_MOVE="+ref+" "+late)
			} else {
				// gh answers after the branch moved: merged at the old head.
				f.env = append(f.env, `GH_PR_VIEW={"state":"MERGED","headRefOid":"`+head+`"}`)
				stub := strings.Replace(stubGHPRView, "printf", `git update-ref `+ref+` `+late+`; printf`, 1)
				if err := os.WriteFile(filepath.Join(f.base, "bin", "gh"), []byte(stub), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, kept, out, err := f.remove(t, Removal{Task: tk})
			if err != nil {
				t.Fatalf("Remove: %v\n%s", err, out)
			}
			if kept != tk.Branch {
				t.Fatalf("kept %q, want %q\n%s", kept, tk.Branch, out)
			}
			if got := f.gitOut(t, f.repo, "rev-parse", ref); got != late {
				t.Fatalf("%s at %s, want %s (the late commit)\n%s", ref, got, late, out)
			}
		})
	}
}

// A git without merge-tree --write-tree (before 2.38) cannot tell a
// squash merge: the branch goes as before only by git branch -d or gh (a
// pull request merged at its commit), else it is kept and named.
func TestRemoveBranchOldGit(t *testing.T) {
	for _, c := range []struct {
		name     string
		pr       bool
		wantKept bool
	}{
		{name: "pull request merged", pr: true},
		{name: "no pull request", wantKept: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, tk := newRemovable(t, nil, "")
			head := f.work(t, tk)
			f.mergeOnOrigin(t, tk.Branch, "main", "squash", false)
			git(t, f.repo, "fetch", "-q", "origin")
			f.wrapGit(t)
			f.env = append(f.env, "GIT_WRAP_OLD=1")
			if c.pr {
				f.env = append(f.env, `GH_PR_VIEW={"state":"MERGED","headRefOid":"`+head+`"}`)
			}
			_, kept, out, err := f.remove(t, Removal{Task: tk})
			if err != nil {
				t.Fatalf("Remove: %v\n%s", err, out)
			}
			has := HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch)
			if c.wantKept {
				if kept != tk.Branch || !has {
					t.Fatalf("kept %q (branch there: %v), want %q\n%s", kept, has, tk.Branch, out)
				}
			} else if kept != "" || has {
				t.Fatalf("kept %q (branch there: %v), want deleted\n%s", kept, has, out)
			}
		})
	}
}

// A worktree git no longer lists (a failed task's) is pruned, and the
// branch handled as usual.
func TestRemoveUnlisted(t *testing.T) {
	f, tk := newRemovable(t, nil, "")
	if err := os.RemoveAll(tk.Worktree); err != nil {
		t.Fatal(err)
	}
	if _, _, out, err := f.remove(t, Removal{Task: tk}); err != nil {
		t.Fatalf("Remove: %v\n%s", err, out)
	}
	if f.listed(t, tk.Worktree) {
		t.Fatal("not pruned")
	}
	if HasRef(context.Background(), f.env, f.repo, "refs/heads/"+tk.Branch) {
		t.Fatal("merged branch kept")
	}
}

// A worktree holding another registered worktree — `claude --worktree`'s
// in an ignored .claude/worktrees — is refused, forced or not, before
// anything runs: git worktree remove would take the inner one, its
// changes included. Once the inner one is removed the outer goes as usual.
func TestRemoveRefusesNestedWorktree(t *testing.T) {
	orca := sharedOrca + "scripts:\n  archive: touch \"$ORCA_ROOT_PATH/archive-ran\"\n"
	f, tk := newRemovable(t, map[string]string{".gitignore": "node_modules/\n.env\n.claude/\n", "orca.yaml": orca}, "")
	inner := filepath.Join(tk.Worktree, ".claude", "worktrees", "x")
	git(t, tk.Worktree, "worktree", "add", "-q", "-b", "x", inner)
	write(t, filepath.Join(inner, "wip.txt"), "unsaved\n")
	for _, force := range []bool{false, true} {
		_, _, out, err := f.remove(t, Removal{Task: tk, Archive: true, Force: force})
		var refused *RefusedError
		want := []wire.RemoveRefusal{{Kind: wire.RefusalNestedWorktrees, Paths: []string{CanonicalPath(inner)}}}
		if !errors.As(err, &refused) || stageOf(err) != wire.TaskStageWorktree || !reflect.DeepEqual(refused.Refusals, want) {
			t.Fatalf("Remove (force %v) = %v, want refused for %v\n%s", force, err, want, out)
		}
		if !f.listed(t, inner) || !IsDir(inner) {
			t.Fatalf("Remove (force %v) took the inner worktree: %v\n%s", force, err, out)
		}
	}
	if got := readFile(t, filepath.Join(inner, "wip.txt")); got != "unsaved\n" {
		t.Fatalf("inner change %q", got)
	}
	if !f.listed(t, tk.Worktree) {
		t.Fatal("outer worktree unregistered")
	}
	if _, err := os.Stat(filepath.Join(f.repo, "archive-ran")); !os.IsNotExist(err) {
		t.Fatalf("archive ran for a refused removal: %v", err)
	}

	git(t, f.repo, "worktree", "remove", "--force", inner)
	if _, _, out, err := f.remove(t, Removal{Task: tk}); err != nil {
		t.Fatalf("Remove once the inner worktree is gone: %v\n%s", err, out)
	}
	if f.listed(t, tk.Worktree) || IsDir(tk.Worktree) {
		t.Fatal("outer worktree left")
	}
}
