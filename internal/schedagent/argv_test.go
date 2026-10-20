package schedagent

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func agentSchedule(h, permission, cwd string) wire.Schedule {
	return wire.Schedule{Kind: wire.ScheduleKindAgent, Harness: h, Prompt: "-check the README", Permission: permission, Cwd: cwd}
}

// Every harness an agent schedule can run (wire.ScheduleHarnesses) has the
// flags of every permission and a command line of its own; there are no
// others.
func TestEveryScheduleHarnessIsKnown(t *testing.T) {
	if len(permissionFlags) != len(wire.ScheduleHarnesses) {
		t.Errorf("permissionFlags has %d harnesses, wire.ScheduleHarnesses %d", len(permissionFlags), len(wire.ScheduleHarnesses))
	}
	for _, h := range wire.ScheduleHarnesses {
		for _, p := range []string{wire.PermissionRead, wire.PermissionWrite, wire.PermissionAll} {
			if len(permissionFlags[h][p]) == 0 {
				t.Errorf("%s has no flags for %s", h, p)
			}
		}
		if argv := Argv(agentSchedule(h, "", t.TempDir()), ""); argv[0] != h {
			t.Errorf("%s runs %q", h, argv)
		}
	}
	if got := wire.ScheduleHarnessNames(); got != "claude, codex or omp" {
		t.Errorf("names %q", got)
	}
}

// Each permission maps to the harness's own flags (#1083); the prompt
// comes after "--" so one starting with "-" stays the prompt.
func TestArgv(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "src")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	home := t.TempDir()
	for _, c := range []struct {
		h, permission, cwd string
		want               []string
	}{
		{wire.HarnessClaude, "", sub, []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "manual", "--", "-check the README"}},
		{wire.HarnessClaude, wire.PermissionRead, sub, []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "manual", "--", "-check the README"}},
		{wire.HarnessClaude, wire.PermissionWrite, sub, []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "acceptEdits", "--", "-check the README"}},
		{wire.HarnessClaude, wire.PermissionAll, sub, []string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none", "--permission-mode", "bypassPermissions", "--", "-check the README"}},
		{wire.HarnessCodex, wire.PermissionRead, sub, []string{"codex", "exec", "--json", "--sandbox", "read-only", "--", "-check the README"}},
		{wire.HarnessCodex, wire.PermissionWrite, repo, []string{"codex", "exec", "--json", "--sandbox", "workspace-write", "--", "-check the README"}},
		{wire.HarnessCodex, wire.PermissionAll, repo, []string{"codex", "exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "--", "-check the README"}},
		{wire.HarnessCodex, wire.PermissionRead, outside, []string{"codex", "exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check", "--", "-check the README"}},
		{wire.HarnessOmp, wire.PermissionRead, sub, []string{"omp", "-p", "--mode", "json", "--approval-mode=always-ask", "--", "-check the README"}},
		{wire.HarnessOmp, wire.PermissionWrite, sub, []string{"omp", "-p", "--mode", "json", "--approval-mode=write", "--", "-check the README"}},
		{wire.HarnessOmp, wire.PermissionAll, sub, []string{"omp", "-p", "--mode", "json", "--approval-mode=yolo", "--", "-check the README"}},
		{wire.HarnessOmp, wire.PermissionRead, home, []string{"omp", "-p", "--mode", "json", "--approval-mode=always-ask", "--allow-home", "--", "-check the README"}},
	} {
		if got := Argv(agentSchedule(c.h, c.permission, c.cwd), home); !slices.Equal(got, c.want) {
			t.Errorf("%s %q in %s:\n got %q\nwant %q", c.h, c.permission, c.cwd, got, c.want)
		}
	}
}

func TestInGitWorkTree(t *testing.T) {
	repo := t.TempDir()
	// A worktree's .git is a file.
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if !InGitWorkTree(repo) || !InGitWorkTree(deep) {
		t.Error("inside a work tree: false")
	}
	if InGitWorkTree(t.TempDir()) {
		t.Error("outside: true")
	}
}
