//go:build !windows

package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// stubGH stands in for gh: every call is logged to $GH_LOG with its
// directory; `pr view <branch> --json number,state,url,headRefOid` prints
// $GH_PR_<branch> (- for /), or fails like gh when that is empty.
const stubGH = `#!/bin/sh
echo "$PWD: $*" >> "$GH_LOG"
[ "$1 $2 $4 $5" = "pr view --json number,state,url,headRefOid" ] || { echo "stub gh: unexpected $*" >&2; exit 1; }
v=$(printenv "GH_PR_$(echo "$3" | tr / _)")
if [ -z "$v" ]; then
  echo "no pull requests found for branch \"$3\"" >&2
  exit 1
fi
printf '%s\n' "$v"
`

// worktree.prs asks gh, in the source checkout, for the pull request of
// each worktree's branch of a repository with a github.com remote: a
// squash-merged one whose head is the worktree's commit is merged, one the
// branch has commits after is not, a branch without a pull request is left
// out, and a repository without a GitHub remote is not asked about.
func TestPRs(t *testing.T) {
	f := newFixture(t)
	app, lib := f.repo(t, "app"), f.repo(t, "lib")
	f.git(t, app, "remote", "add", "upstream", "https://github.com/o/app.git")
	squashed := filepath.Join(f.base, "wt", "squashed")
	more := filepath.Join(f.base, "wt", "more")
	nopr := filepath.Join(f.base, "wt", "nopr")
	f.worktree(t, app, squashed, "feat/squashed")
	f.worktree(t, app, more, "more")
	f.worktree(t, app, nopr, "nopr")
	f.worktree(t, lib, filepath.Join(f.base, "wt", "lib"), "lib")
	f.git(t, squashed, "commit", "-q", "--allow-empty", "-m", "work")
	f.git(t, more, "commit", "-q", "--allow-empty", "-m", "work")
	squashedHead := f.git(t, squashed, "rev-parse", "HEAD")
	moreMerged := f.git(t, more, "rev-parse", "HEAD")
	f.git(t, more, "commit", "-q", "--allow-empty", "-m", "after the merge")

	bin := filepath.Join(f.base, "bin")
	writeFile(t, filepath.Join(bin, "gh"), stubGH)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(f.base, "gh.log")
	env := append(f.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GH_LOG="+log,
		`GH_PR_feat_squashed={"number":12,"state":"MERGED","url":"https://github.com/o/app/pull/12","headRefOid":"`+squashedHead+`"}`,
		`GH_PR_more={"number":13,"state":"MERGED","url":"https://github.com/o/app/pull/13","headRefOid":"`+moreMerged+`"}`,
		`GH_PR_lib={"number":1,"state":"MERGED","url":"u","headRefOid":"x"}`,
	)
	inv, err := Discover(context.Background(), Sources{Env: env, Repos: []string{app, lib}})
	if err != nil {
		t.Fatal(err)
	}
	prs := PRs(context.Background(), env, inv, nil)
	if len(prs) != 2 {
		t.Fatalf("prs %+v", prs)
	}
	byPath := map[string]int{}
	for i, p := range prs {
		byPath[p.Path] = i
	}
	if p := prs[byPath[squashed]]; p.Path != squashed || p.Number != 12 || p.State != "MERGED" || p.URL != "https://github.com/o/app/pull/12" || !p.Merged {
		t.Fatalf("squashed %+v", p)
	}
	if p := prs[byPath[more]]; p.Path != more || p.Number != 13 || p.Merged {
		t.Fatalf("more %+v", p)
	}
	b, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(calls) != 3 {
		t.Fatalf("gh calls %q", calls)
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, app+": pr view ") {
			t.Fatalf("gh call %q, want one in %s", c, app)
		}
	}
}

// A PR task's worktree is asked about by the pull request's number, as
// task.remove does: gh finds a fork's pull request by number only, not by
// the local branch name.
func TestPRsOfPRTask(t *testing.T) {
	f := newFixture(t)
	app := f.repo(t, "app")
	f.git(t, app, "remote", "add", "upstream", "https://github.com/o/app.git")
	wt := filepath.Join(f.base, "wt", "pr-42")
	f.worktree(t, app, wt, "contributor-patch")
	head := f.git(t, wt, "rev-parse", "HEAD")
	bin := filepath.Join(f.base, "bin")
	writeFile(t, filepath.Join(bin, "gh"), stubGH)
	if err := os.Chmod(filepath.Join(bin, "gh"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := append(f.env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GH_LOG="+filepath.Join(f.base, "gh.log"),
		`GH_PR_42={"number":42,"state":"MERGED","url":"https://github.com/o/app/pull/42","headRefOid":"`+head+`"}`,
	)
	inv, err := Discover(context.Background(), Sources{Env: env, Repos: []string{app}})
	if err != nil {
		t.Fatal(err)
	}
	tasks := []wire.Task{{ID: "t", Repo: app, Kind: wire.TaskKindPR, Number: 42, Worktree: wt, Branch: "contributor-patch"}}
	prs := PRs(context.Background(), env, inv, tasks)
	if len(prs) != 1 || prs[0].Path != wt || prs[0].Number != 42 || !prs[0].Merged {
		t.Fatalf("prs %+v", prs)
	}
}
