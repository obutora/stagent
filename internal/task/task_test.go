package task

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo makes a repository with one commit under a fresh directory,
// returned with symlinks resolved.
func newRepo(t *testing.T) (base, repo string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo = filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	return base, repo
}

// The canonical path of a checkout is its top level with symlinks
// resolved, from any directory inside it; a linked worktree stays itself.
func TestCanonical(t *testing.T) {
	base, repo := newRepo(t)
	ctx := context.Background()
	sub := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	wt := filepath.Join(base, "repo-7")
	git(t, repo, "worktree", "add", "-q", "-b", "7-x", wt)
	for dir, want := range map[string]string{
		repo:                          repo,
		sub:                           repo,
		link:                          repo,
		filepath.Join(link, "a", "b"): repo,
		wt:                            wt,
	} {
		got, err := Canonical(ctx, nil, dir)
		if err != nil || got != want {
			t.Errorf("Canonical(%s) = %q, %v; want %q", dir, got, err, want)
		}
	}
	if _, err := Canonical(ctx, nil, base); err == nil {
		t.Errorf("Canonical of a directory outside any checkout succeeded")
	} else if ge := (*GitError)(nil); !errors.As(err, &ge) {
		t.Errorf("Canonical outside a checkout: %v, want a *GitError", err)
	}
}

// A git missing from the given environment's PATH is exec.ErrNotFound.
func TestGitNotFound(t *testing.T) {
	_, err := Git(context.Background(), []string{"PATH=" + t.TempDir()}, t.TempDir(), "version")
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("git outside PATH: %v", err)
	}
}

// Listed knows the worktrees git lists for a checkout, the checkout
// itself included, and none that was removed.
func TestListed(t *testing.T) {
	base, repo := newRepo(t)
	ctx := context.Background()
	kept, removed := filepath.Join(base, "repo-1"), filepath.Join(base, "repo-2")
	git(t, repo, "worktree", "add", "-q", "-b", "b1", kept)
	git(t, repo, "worktree", "add", "-q", "-b", "b2", removed)
	git(t, repo, "worktree", "remove", removed)
	for wt, want := range map[string]bool{repo: true, kept: true, removed: false, filepath.Join(base, "other"): false} {
		got, err := Listed(ctx, nil, repo, wt)
		if err != nil || got != want {
			t.Errorf("Listed(%s) = %v, %v; want %v", wt, got, err, want)
		}
	}
	if _, err := Listed(ctx, nil, filepath.Join(base, "gone"), kept); err == nil {
		t.Errorf("Listed of a missing checkout succeeded")
	}
}

func TestWithin(t *testing.T) {
	dir := filepath.FromSlash("/w/repo-5")
	for p, want := range map[string]bool{
		"/w/repo-5":       true,
		"/w/repo-5/src/a": true,
		"/w/repo-50":      false,
		"/w/repo":         false,
		"/w":              false,
	} {
		if got := Within(filepath.FromSlash(p), dir); got != want {
			t.Errorf("Within(%s, %s) = %v, want %v", p, dir, got, want)
		}
	}
}

// A task's key is its checkout and number, or name for a named task: an
// issue never matches a name, nor one name another.
func TestSameKey(t *testing.T) {
	repo := filepath.FromSlash("/w/repo")
	issue := &wire.Task{Repo: repo, Kind: wire.TaskKindIssue, Number: 5}
	pr := &wire.Task{Repo: repo, Kind: wire.TaskKindPR, Number: 5}
	named := &wire.Task{Repo: repo, Kind: wire.TaskKindName, Name: "brave-otter"}
	for _, c := range []struct {
		t      *wire.Task
		repo   string
		kind   string
		number int
		name   string
		want   bool
	}{
		{issue, repo, wire.TaskKindIssue, 5, "", true},
		{pr, repo, wire.TaskKindIssue, 5, "", true}, // number keys issues and PRs alike
		{issue, repo, wire.TaskKindIssue, 6, "", false},
		{issue, filepath.FromSlash("/w/other"), wire.TaskKindIssue, 5, "", false},
		{issue, repo, wire.TaskKindName, 0, "5", false},
		{named, repo, wire.TaskKindName, 0, "brave-otter", true},
		{named, repo, wire.TaskKindName, 0, "Brave-otter", false},
		{named, repo, wire.TaskKindName, 0, "calm-fox", false},
		{named, repo, wire.TaskKindIssue, 0, "", false},
		{named, filepath.FromSlash("/w/other"), wire.TaskKindName, 0, "brave-otter", false},
	} {
		if got := SameKey(c.t, c.repo, c.kind, c.number, c.name); got != c.want {
			t.Errorf("SameKey(%+v, %s, %s, %d, %q) = %v", *c.t, c.repo, c.kind, c.number, c.name, got)
		}
	}
}

// A name is 1–64 characters, no whitespace, control characters or
// / \ : * ? " < > |, not starting with . or -, and a branch name git takes.
func TestValidName(t *testing.T) {
	_, repo := newRepo(t)
	ctx := context.Background()
	for _, ok := range []string{"brave-otter", "a", "Issue_42", "v1.2", "名前", strings.Repeat("a", 64), strings.Repeat("あ", 64)} {
		if err := ValidName(ctx, nil, repo, ok); err != nil {
			t.Errorf("ValidName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", strings.Repeat("a", 65), ".", "..", ".hidden", "-x", "two words", "tab\tx", "nl\nx", "nul\x00x",
		"a/b", `a\b`, "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b",
		"a..b", "a.lock", "a@{b", "a~b", "a^b", "a[b", "x.", "@{-1}", "a\xffb",
	} {
		if err := ValidName(ctx, nil, repo, bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
}
