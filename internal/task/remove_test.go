//go:build !windows

package task

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
	ok, err := Listed(context.Background(), f.env, f.repo, worktree)
	if err != nil {
		t.Fatal(err)
	}
	return ok
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
