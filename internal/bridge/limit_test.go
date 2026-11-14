//go:build !windows

package bridge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/stubcmd"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// A git status before the removal that does not answer fails task.remove
// at its limit (the real git otherwise, every limit a hundredth): the dry
// run and the removal, forced or not, return an error that it timed out,
// and nothing is removed or forgotten.
func TestTaskRemoveCheckTimeout(t *testing.T) {
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	repo := checkout(t)
	wt := filepath.Join(filepath.Dir(repo), "app-5")
	addWorktree(t, repo, wt, "5-fix")
	bin := t.TempDir()
	stubcmd.Install(t, bin, "git",
		stubcmd.Rule{Args: "status *", Await: filepath.Join(t.TempDir(), "never")},
		stubcmd.Rule{Args: "*", Exec: real})
	d, a := startWorktreeBridge(t, wire.Config{}, func(b *Bridge) {
		b.loginEnv = func() ([]string, error) {
			return []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), task.EnvGitTimeoutScale + "=0.01"}, nil
		}
	})
	d.tasks = []wire.Task{{ID: "t5", Repo: repo, Kind: wire.TaskKindIssue, Number: 5, Branch: "5-fix", Worktree: wt, State: wire.TaskReady}}

	for _, p := range []wire.TaskRemoveParams{{ID: "t5", DryRun: true}, {ID: "t5"}, {ID: "t5", Force: true, StopSessions: true}} {
		e := a.call(t, wire.MethodTaskRemove, p, nil)
		if e == nil || !strings.Contains(e.Message, "git status --porcelain -z --untracked-files=all --ignore-submodules=none: timed out after 300ms") {
			t.Fatalf("task.remove %+v: %v", p, e)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.forgot) != 0 || !listed(t, repo, wt) || gone(t, wt) || gitOut(t, repo, "branch", "--list", "5-fix") == "" {
		t.Fatalf("forgot %v; worktree listed %v", d.forgot, listed(t, repo, wt))
	}
}
