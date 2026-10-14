//go:build !windows

package bridge

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// previewCheckout is a checkout tracking .gitignore, .worktreeinclude and
// orca.yaml (setup, shared node_modules), with ignored and unignored
// files beside.
func previewCheckout(t *testing.T, orca string) string {
	t.Helper()
	repo := checkout(t)
	writeTree(t, repo, map[string]string{
		".gitignore":       ".env\nnode_modules/\n*.local\ngen/\n",
		".worktreeinclude": ".env\nnode_modules/pkg\nREADME.md\n",
		"orca.yaml":        orca,
		"README.md":        "r",
	})
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "files")
	writeTree(t, repo, map[string]string{".env": "KEY=1\n", "node_modules/pkg/i.js": "js", "config/dev.local": "dev", "todo.txt": "t"})
	return repo
}

// task.preview resolves what a new worktree would get: the saved app
// patterns (config.json repos.<repo>.worktree_include) with the
// checkout's .worktreeinclude, shared directories as links, skips with
// their reasons, and the checkout's setup. Agent descendants may ask.
func TestTaskPreview(t *testing.T) {
	repo := previewCheckout(t, "scripts:\n  setup: npm ci\nworktree:\n  sharedDirectories: [node_modules]\n")
	lay, a := startBridge(t, func(b *Bridge) {
		b.agentRefusal = func() *wire.Error { return wire.Errorf(wire.ErrAgentRefused, "agent") }
	})
	cfg, _ := json.Marshal(wire.Config{Repos: map[string]wire.RepoConfig{repo: {WorktreeInclude: []string{"config/*.local", "todo.txt", "nothing/*"}}}})
	if err := os.WriteFile(lay.Config, cfg, 0o600); err != nil {
		t.Fatal(err)
	}

	var res wire.TaskPreviewResult
	if e := a.call(t, wire.MethodTaskPreview, map[string]any{"repo": repo + "/"}, &res); e != nil {
		t.Fatal(e)
	}
	want := wire.TaskPreviewResult{
		Repo: repo,
		Copies: []wire.TaskCopy{
			{Path: ".env", Source: "include", Bytes: 6},
			{Path: "config/dev.local", Source: "app", Bytes: 3},
		},
		Links: []wire.TaskLink{{Path: "node_modules"}},
		Skipped: []wire.TaskSkip{
			{Entry: "README.md", Source: "include", Reason: "tracked"},
			{Entry: "todo.txt", Source: "app", Reason: "not_ignored"},
			{Entry: "nothing/*", Source: "app", Reason: "no_match"},
			{Entry: "node_modules/pkg", Source: "include", Reason: "under_link"},
		},
		TotalBytes: 9,
		Setup:      "npm ci",
	}
	checkPreview(t, res, want)

	// An unsaved trial replaces the saved patterns; an empty one has none.
	res = wire.TaskPreviewResult{}
	if e := a.call(t, wire.MethodTaskPreview, wire.TaskPreviewParams{Repo: repo, WorktreeInclude: []string{}}, &res); e != nil {
		t.Fatal(e)
	}
	want.Copies, want.TotalBytes = want.Copies[:1], 6
	want.Skipped = []wire.TaskSkip{
		{Entry: "README.md", Source: "include", Reason: "tracked"},
		{Entry: "node_modules/pkg", Source: "include", Reason: "under_link"},
	}
	checkPreview(t, res, want)
}

func checkPreview(t *testing.T, got, want wire.TaskPreviewResult) {
	t.Helper()
	slices.SortFunc(got.Skipped, func(a, b wire.TaskSkip) int { return compareSkip(a, b) })
	slices.SortFunc(want.Skipped, func(a, b wire.TaskSkip) int { return compareSkip(a, b) })
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("task.preview\n%s\nwant\n%s", g, w)
	}
}

func compareSkip(a, b wire.TaskSkip) int {
	switch {
	case a.Entry < b.Entry:
		return -1
	case a.Entry > b.Entry:
		return 1
	}
	return 0
}

// copies holds 500 rows at most (truncated), total_bytes counts them all;
// a broken orca.yaml is reported in one line and its shared directories
// are not linked.
func TestTaskPreviewTruncatedAndBrokenOrca(t *testing.T) {
	repo := previewCheckout(t, "a: 1\na: 2\n")
	files := map[string]string{}
	for i := range 501 {
		files[fmt.Sprintf("gen/f%03d", i)] = "xy"
	}
	writeTree(t, repo, files)
	_, a := startBridge(t)

	var res wire.TaskPreviewResult
	if e := a.call(t, wire.MethodTaskPreview, wire.TaskPreviewParams{Repo: repo, WorktreeInclude: []string{"gen"}}, &res); e != nil {
		t.Fatal(e)
	}
	if len(res.Copies) != 500 || !res.Truncated || res.TotalBytes != 6+2+501*2 {
		t.Fatalf("%d copies, truncated %v, total %d", len(res.Copies), res.Truncated, res.TotalBytes)
	}
	if res.OrcaError == "" || res.Setup != "" || len(res.Links) != 0 {
		t.Fatalf("orca_error %q, setup %q, links %v", res.OrcaError, res.Setup, res.Links)
	}
	if !slices.Contains(res.Copies, wire.TaskCopy{Path: "node_modules/pkg", Source: "include", Bytes: 2}) {
		t.Fatalf("node_modules/pkg not copied without its link: %v", res.Copies[:3])
	}
}

// A repo that is no checkout is bad_request.
func TestTaskPreviewNoCheckout(t *testing.T) {
	_, a := startBridge(t)
	if e := a.call(t, wire.MethodTaskPreview, wire.TaskPreviewParams{Repo: t.TempDir()}, nil); e == nil || e.Code != wire.ErrBadRequest {
		t.Fatalf("task.preview of no checkout: %v", e)
	}
}

// With a name, task.preview says why a named task could not take it, the
// first of invalid, task, folder, branch, no_folder; nothing when it is
// free. Without one the daemon is not asked.
func TestTaskPreviewNameProblem(t *testing.T) {
	repo := checkout(t)
	gitRun(t, repo, "branch", "taken-branch")
	gitRun(t, repo, "branch", "taken-dir")
	folder := filepath.Join(filepath.Dir(repo), "worktrees")
	writeTree(t, filepath.Join(folder, "app"), map[string]string{"taken-dir/x": "x", "taken-task/x": "x"})
	other := wire.Task{ID: "o", Repo: filepath.Join(filepath.Dir(repo), "other"), Kind: wire.TaskKindName, Name: "elsewhere"}
	issue := wire.Task{ID: "i", Repo: repo, Kind: wire.TaskKindIssue, Number: 5}
	named := wire.Task{ID: "n", Repo: repo, Kind: wire.TaskKindName, Name: "taken-task"}
	d := &taskDaemon{tasks: []wire.Task{other, issue, named}}
	lay, a := startBridge(t)
	serveFake(t, lay.DaemonAddr, d.handle)
	problem := func(name, branch string) string {
		t.Helper()
		var res wire.TaskPreviewResult
		if e := a.call(t, wire.MethodTaskPreview, wire.TaskPreviewParams{Repo: repo, Name: name, Branch: branch}, &res); e != nil {
			t.Fatal(e)
		}
		return res.NameProblem
	}

	if got := problem("", ""); got != "" || len(d.calls) != 0 {
		t.Fatalf("no name: %q, daemon calls %v", got, d.calls)
	}
	for _, folderSet := range []bool{false, true} {
		if folderSet {
			cfg, _ := json.Marshal(wire.Config{WorktreeFolder: folder})
			if err := os.WriteFile(lay.Config, cfg, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		free, dir := wire.TaskNameNoFolder, wire.TaskNameBranch // without a folder no place is taken
		if folderSet {
			free, dir = "", wire.TaskNameFolder
		}
		for _, c := range []struct{ name, branch, want string }{
			{"a/b", "", wire.TaskNameInvalid},
			{"-x", "taken-branch", wire.TaskNameInvalid},
			{"taken-task", "taken-branch", wire.TaskNameTask},
			{"taken-dir", "", dir},
			{"taken-branch", "", wire.TaskNameBranch},
			{"free", "taken-branch", wire.TaskNameBranch},
			{"free", "", free},
			{"elsewhere", "", free},
			{"5", "", free},
		} {
			if got := problem(c.name, c.branch); got != c.want {
				t.Errorf("folder %v, name %q, branch %q: %q, want %q", folderSet, c.name, c.branch, got, c.want)
			}
		}
	}
}
