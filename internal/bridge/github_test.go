package bridge

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/wire"
)

// withGitHub turns the github.* methods on as on Windows; the user's
// processes are in dirs.
func withGitHub(dirs ...string) func(*Bridge) {
	return func(b *Bridge) {
		b.github = true
		b.liveDirs = func([]int) []string { return dirs }
	}
}

// gitRepo creates a working tree with an origin remote and returns its
// path as git reports it.
func gitRepo(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", dir},
		{"-C", dir, "remote", "add", "origin", "https://github.com/o/" + name + ".git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// stubGH puts a gh stub answering by rules first on PATH.
func stubGH(t *testing.T, rules ...stubcmd.Rule) {
	t.Helper()
	bin := t.TempDir()
	stubcmd.Install(t, bin, "gh", rules...)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func helloCaps(t *testing.T, a *app) []string {
	t.Helper()
	var res wire.HelloResult
	if e := a.call(t, wire.MethodHello, wire.HelloParams{Protocol: 1, Client: "test"}, &res); e != nil {
		t.Fatal(e)
	}
	return res.Capabilities
}

// The github.* methods are on by default on Windows only, where the app's
// sh scripts cannot run (ADR 0007).
func TestGitHubIsOnlyAnnouncedAndServedWhereEnabled(t *testing.T) {
	onWindows := runtime.GOOS == "windows"
	_, a := startBridge(t)
	if got := slices.Contains(helloCaps(t, a), wire.CapGitHub); got != onWindows {
		t.Fatalf("hello announces github on %s: %v, want %v", runtime.GOOS, got, onWindows)
	}
	if !onWindows {
		if e := a.call(t, wire.MethodGitHubRepos, nil, nil); e == nil || e.Code != wire.ErrUnknownMethod {
			t.Fatalf("github.repos off Windows: %v, want unknown_method", e)
		}
	}

	_, a = startBridge(t, withGitHub())
	if !slices.Contains(helloCaps(t, a), wire.CapGitHub) {
		t.Fatal("hello does not announce github")
	}
}

func TestGitHubReposMergesProcessesSessionsAndConversations(t *testing.T) {
	live, session, conv := gitRepo(t, "live"), gitRepo(t, "session"), gitRepo(t, "conv")
	var mu sync.Mutex
	var holders []int
	l, a := startBridge(t, func(b *Bridge) {
		b.github = true
		b.liveDirs = func(h []int) []string {
			mu.Lock()
			holders = h
			mu.Unlock()
			return []string{live}
		}
	})
	const t0 = 1_760_000_000_000
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		switch m.Method {
		case wire.MethodSessionsList:
			return wire.SessionsListResult{Sessions: []wire.Session{
				{ID: idA, Cwd: session, HolderPID: 4242, State: wire.StateIdle, StartedAt: t0 - 9000, LastActivityAt: t0},
				{ID: idB, Cwd: t.TempDir(), HolderPID: 4343, State: wire.StateExited, StartedAt: t0},
			}}, nil
		case wire.MethodConversationsList:
			return wire.ConversationsListResult{Conversations: []wire.Conversation{
				{Harness: "claude", Cwd: session, UpdatedAt: t0 - 5000}, // older than the session's activity
				{Harness: "codex", Cwd: conv, UpdatedAt: t0 + 1000},
			}}, nil
		}
		return struct{}{}, nil
	})

	before := time.Now().Add(-time.Second)
	var res wire.GitHubReposResult
	if e := a.call(t, wire.MethodGitHubRepos, nil, &res); e != nil {
		t.Fatal(e)
	}
	var paths []string
	for _, r := range res.Repos {
		paths = append(paths, r.Path)
	}
	if want := []string{live, conv, session}; res.GitMissing || !reflect.DeepEqual(paths, want) {
		t.Fatalf("repos = %v (git_missing %v), want %v", paths, res.GitMissing, want)
	}
	if at, err := time.Parse(time.RFC3339, res.Repos[0].LastActiveAt); err != nil || at.Before(before.Truncate(time.Second)) {
		t.Errorf("process repo last_active_at = %q, want now", res.Repos[0].LastActiveAt)
	}
	if got, want := res.Repos[1].LastActiveAt, time.UnixMilli(t0+1000).UTC().Format(time.RFC3339); got != want {
		t.Errorf("conversation repo last_active_at = %q, want %q", got, want)
	}
	if got, want := res.Repos[2].LastActiveAt, time.UnixMilli(t0).UTC().Format(time.RFC3339); got != want {
		t.Errorf("session repo last_active_at = %q, want %q", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(holders, []int{4242}) {
		t.Errorf("processes looked up under holders %v, want the live session's [4242]", holders)
	}
}

func TestGitHubMethodsServeAgentDescendants(t *testing.T) {
	stubGH(t,
		stubcmd.Rule{Args: "auth status*"},
		stubcmd.Rule{Args: "repo view*", Stdout: `{"description":"d","isPrivate":false}` + "\n"},
		stubcmd.Rule{Args: "*", Stdout: "[]\n"},
	)
	repo := gitRepo(t, "r")
	_, a := startBridge(t, withGitHub(repo), func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "agent") }
	})
	var repos wire.GitHubReposResult
	if e := a.call(t, wire.MethodGitHubRepos, nil, &repos); e != nil || len(repos.Repos) != 1 {
		t.Fatalf("github.repos from an agent: %v, %+v", e, repos)
	}
	var act wire.GitHubActivityResult
	if e := a.call(t, wire.MethodGitHubActivity, wire.GitHubActivityParams{Path: repo, Host: "github.com", Repo: "o/r"}, &act); e != nil {
		t.Fatalf("github.activity from an agent: %v", e)
	}
	if act.GH != wire.GHReady || act.Description != "d" || string(act.Pulls) != "[]" || string(act.Issues) != "[]" {
		t.Fatalf("github.activity = %+v", act)
	}
}

func TestGitHubRequestsDoNotStallTheDaemonQueue(t *testing.T) {
	release := filepath.Join(t.TempDir(), "release")
	stubGH(t, stubcmd.Rule{Args: "*", Await: release, Stdout: "[]\n"})
	l, a := startBridge(t, withGitHub())
	serveFake(t, l.DaemonAddr, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		return wire.SessionsListResult{Sessions: []wire.Session{}}, nil
	})

	slow := a.goCall(wire.MethodGitHubActivity, "activity", wire.GitHubActivityParams{Host: "github.com", Repo: "o/r"})
	if e := a.call(t, wire.MethodSessionsList, nil, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case <-slow:
		t.Fatal("github.activity answered before gh did")
	default:
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if m := wait(t, slow); m.Error != nil || !strings.Contains(string(m.Result), `"gh":"ready"`) {
		t.Fatalf("github.activity = %s %v", m.Result, m.Error)
	}
}

func TestGitHubStatusReportsClosedIssuesAndMergedPulls(t *testing.T) {
	stubGH(t,
		stubcmd.Rule{Args: "issue view 12 -R o/r --json state", Stdout: `{"state":"CLOSED"}` + "\n"},
		stubcmd.Rule{Args: "pr view 12-fix -R o/r --json state", Stdout: `{"state":"MERGED"}` + "\n"},
		stubcmd.Rule{Args: "pr view 13 -R o/r --json state", Stdout: `{"state":"MERGED"}` + "\n"},
	)
	_, a := startBridge(t, withGitHub())
	var res wire.GitHubStatusResult
	if e := a.call(t, wire.MethodGitHubStatus, wire.GitHubStatusParams{Items: []wire.GitHubStatusQuery{
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 12, Branch: "12-fix"},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 13},
	}}, &res); e != nil {
		t.Fatal(e)
	}
	want := []wire.GitHubStatusItem{
		{Repo: "o/r", Kind: wire.GitHubKindIssue, Number: 12, Branch: "12-fix", State: "CLOSED", PRState: "MERGED"},
		{Repo: "o/r", Kind: wire.GitHubKindPR, Number: 13, State: "MERGED"},
	}
	if !reflect.DeepEqual(res.Items, want) {
		t.Fatalf("github.status = %+v, want %+v", res.Items, want)
	}
}
