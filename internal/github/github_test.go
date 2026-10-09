package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/github"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

func TestMain(m *testing.M) { stubcmd.Main(m) }

// runner returns a Runner whose PATH holds the given stub programs and the
// directories of the real programs (skipping the test when one is not
// installed), with this process's other variables (Windows' ssh exits 255
// with only PATH and HOME). Real programs stay where they are: Git for
// Windows' git.exe finds its installation from its own path.
func runner(t *testing.T, stubs map[string][]stubcmd.Rule, real ...string) github.Runner {
	t.Helper()
	bin := t.TempDir()
	path := []string{bin}
	for _, name := range real {
		p, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s not installed", name)
		}
		path = append(path, filepath.Dir(p))
	}
	for name, rules := range stubs {
		stubcmd.Install(t, bin, name, rules...)
	}
	return github.Runner{Env: append(os.Environ(), "PATH="+strings.Join(path, string(os.PathListSeparator)), "HOME="+t.TempDir())}
}

// git runs git in dir for test setup, isolated from the user's config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// realDir creates dir (and parents) and returns its path with symlinks
// resolved, as git reports top-level directories.
func realDir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func rfc3339(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }

func TestReposDescribesWorkingTreesNewestFirst(t *testing.T) {
	r := runner(t, nil, "git", "ssh")
	root := realDir(t, t.TempDir())

	// a: on main, one commit ahead of its upstream, a change and a new file,
	// an ssh remote through a host alias.
	origin := filepath.Join(root, "origin.git")
	git(t, root, "init", "--bare", "-b", "main", origin)
	a := filepath.Join(root, "a")
	git(t, root, "clone", "-q", origin, a)
	os.WriteFile(filepath.Join(a, "README"), []byte("one\n"), 0o644)
	git(t, a, "add", "README")
	git(t, a, "commit", "-q", "-m", "one")
	git(t, a, "push", "-q", "-u", "origin", "main")
	git(t, a, "remote", "set-head", "origin", "main")
	git(t, a, "commit", "-q", "--allow-empty", "-m", "two")
	git(t, a, "remote", "set-url", "origin", "git@stagent-test-alias:o/a.git")
	os.WriteFile(filepath.Join(a, "README"), []byte("changed\n"), 0o644)
	os.WriteFile(filepath.Join(a, "new.txt"), []byte("new\n"), 0o644)
	sub := realDir(t, filepath.Join(a, "sub"))

	// b: detached, its only remote is not named origin.
	b := realDir(t, filepath.Join(root, "b"))
	git(t, b, "init", "-q")
	git(t, b, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, b, "remote", "add", "upstream", "https://github.com/o/b.git")
	git(t, b, "checkout", "-q", "--detach")
	short := git(t, b, "rev-parse", "--short", "HEAD")

	// c: no remote. plain: not a working tree.
	c := realDir(t, filepath.Join(root, "c"))
	git(t, c, "init", "-q")
	plain := realDir(t, filepath.Join(root, "plain"))

	const t0 = 1_760_000_000_000
	got := github.Repos(context.Background(), r, []github.Candidate{
		{Dir: a, At: t0},
		{Dir: sub, At: t0 + 1000},
		{Dir: b, At: t0 + 2000},
		{Dir: c, At: t0 + 3000},
		{Dir: plain, At: t0 + 4000},
		{Dir: b, At: t0 - 5000},
		{Dir: "relative/dir", At: t0 + 9000},
	})

	want := wire.GitHubReposResult{Repos: []wire.GitHubRepo{
		{Path: b, RemoteURL: "https://github.com/o/b.git", Branch: short, Detached: true, LastActiveAt: rfc3339(t0 + 2000)},
		{Path: a, RemoteURL: "git@stagent-test-alias:o/a.git", SSHHost: "stagent-test-alias", Branch: "main",
			DefaultBranch: "main", Changes: 2, Ahead: new(1), Behind: new(0), LastActiveAt: rfc3339(t0 + 1000)},
	}}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		t.Fatalf("Repos =\n%s\nwant\n%s", g, w)
	}
}

func TestReposInspectsTheTwentyNewest(t *testing.T) {
	r := runner(t, nil, "git")
	root := realDir(t, t.TempDir())
	var cands []github.Candidate
	var want []string
	for i := range 25 {
		dir := realDir(t, filepath.Join(root, fmt.Sprintf("r%02d", i)))
		git(t, dir, "init", "-q")
		git(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
		cands = append(cands, github.Candidate{Dir: dir, At: int64(i+1) * 1000})
		if i >= 5 {
			want = append([]string{dir}, want...)
		}
	}
	got := github.Repos(context.Background(), r, cands)
	var paths []string
	for _, repo := range got.Repos {
		paths = append(paths, repo.Path)
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}
}

func TestReposReportsMissingGit(t *testing.T) {
	got := github.Repos(context.Background(), runner(t, nil), []github.Candidate{{Dir: t.TempDir(), At: 1}})
	b, _ := json.Marshal(got)
	if string(b) != `{"repos":[],"git_missing":true}` {
		t.Fatalf("Repos = %s", b)
	}
}

// ghStub answers the gh calls of one repository o/r on github.com; extra
// rules come first.
func ghStub(extra ...stubcmd.Rule) []stubcmd.Rule {
	return append(extra,
		stubcmd.Rule{Args: "auth status --hostname github.com"},
		stubcmd.Rule{Args: "repo view o/r --json description,isPrivate", Stdout: `{"description":"  A repo\n","isPrivate":true}` + "\n"},
		stubcmd.Rule{Args: "pr list -R o/r --state open --limit 30 --json *", Stdout: `[{"number":1,"title":"p"}]` + "\n"},
		stubcmd.Rule{Args: "issue list -R o/r --state open --limit 30 --json *blockedBy*", Stderr: `Unknown JSON field: "blockedBy"` + "\n", Exit: 1},
		stubcmd.Rule{Args: "issue list -R o/r --state open --limit 30 --json *", Stdout: `[{"number":2,"title":"i"}]` + "\n"},
		stubcmd.Rule{Args: "pr view feature -R o/r --json *", Stdout: `{"number":3,"state":"OPEN"}` + "\n"},
		stubcmd.Rule{Args: "pr view *", Stderr: "no pull requests found for branch\n", Exit: 1},
	)
}

func activity(t *testing.T, r github.Runner, p wire.GitHubActivityParams) wire.GitHubActivityResult {
	t.Helper()
	res, err := github.Activity(context.Background(), r, p)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestActivityReturnsGhJSON(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": ghStub()})
	got := activity(t, r, wire.GitHubActivityParams{Path: t.TempDir(), Host: "github.com", Repo: "o/r", Branch: "feature"})
	want := wire.GitHubActivityResult{
		GH: wire.GHReady, Description: "A repo", IsPrivate: true,
		Pulls:   json.RawMessage(`[{"number":1,"title":"p"}]` + "\n"),
		Issues:  json.RawMessage(`[{"number":2,"title":"i"}]` + "\n"),
		Current: json.RawMessage(`{"number":3,"state":"OPEN"}` + "\n"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Activity = %+v\nwant %+v", got, want)
	}
}

func TestActivityReportsListErrorsAndNoPull(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": ghStub(stubcmd.Rule{Args: "pr list *", Stderr: "HTTP 502\nBad gateway\n", Exit: 1})})
	got := activity(t, r, wire.GitHubActivityParams{Path: t.TempDir(), Host: "github.com", Repo: "o/r", Branch: "other"})
	if got.GH != wire.GHReady || got.Pulls != nil || got.PullsError != "HTTP 502 Bad gateway" || got.Issues == nil || got.Current != nil {
		t.Fatalf("Activity = %+v", got)
	}
}

func TestActivityReportsUnauthenticatedGh(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": {{Args: "*", Stderr: "You are not logged into any GitHub hosts.\nTo log in, run: gh auth login\n", Exit: 1}}})
	got := activity(t, r, wire.GitHubActivityParams{Host: "github.com", Repo: "o/r"})
	want := wire.GitHubActivityResult{GH: wire.GHUnauthenticated,
		AuthMessage: "You are not logged into any GitHub hosts. To log in, run: gh auth login"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Activity = %+v", got)
	}
}

func TestActivityReportsMissingGhAndWinget(t *testing.T) {
	got := activity(t, runner(t, nil), wire.GitHubActivityParams{Host: "github.com", Repo: "o/r"})
	if !reflect.DeepEqual(got, wire.GitHubActivityResult{GH: wire.GHMissing}) {
		t.Fatalf("without winget: %+v", got)
	}
	got = activity(t, runner(t, map[string][]stubcmd.Rule{"winget": nil}), wire.GitHubActivityParams{Host: "github.com", Repo: "o/r"})
	if !reflect.DeepEqual(got, wire.GitHubActivityResult{GH: wire.GHMissing, PackageManager: "winget"}) {
		t.Fatalf("with winget: %+v", got)
	}
}

func TestActivityRefusesOptionLikeArguments(t *testing.T) {
	r := runner(t, map[string][]stubcmd.Rule{"gh": ghStub()})
	for _, p := range []wire.GitHubActivityParams{
		{Host: "github.com", Repo: "--repo=x/y"},
		{Host: "github.com", Repo: "o/r/x/y"},
		{Host: "-h", Repo: "o/r"},
		{Host: "github.com", Repo: "o/r", Branch: "--web"},
	} {
		_, err := github.Activity(context.Background(), r, p)
		var we *wire.Error
		if !errors.As(err, &we) || we.Code != wire.ErrBadRequest {
			t.Errorf("Activity(%+v) error = %v, want bad_request", p, err)
		}
	}
}
