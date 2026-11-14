//go:build !windows

package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// KilledAdd runs `git worktree add <args>` in repo and kills it (SIGKILL
// to its process group) in the middle of the checkout, as a session
// killed while it prepares a task does: the new worktree's directory keeps
// only its .git file and git lists it locked, reason "initializing". A
// smudge filter that waits holds the checkout (the branch needs a file);
// the attribute naming it is gone afterwards.
func KilledAdd(t testing.TB, repo string, args ...string) {
	t.Helper()
	common := gitOut(t, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	attrs := filepath.Join(common, "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(attrs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attrs, []byte("* filter=stall\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(attrs)
	started := filepath.Join(t.TempDir(), "smudging")
	cmd := exec.Command("git", append([]string{
		"-c", "filter.stall.smudge=touch '" + started + "'; sleep 60",
		"-c", "filter.stall.required=true",
		"worktree", "add",
	}, args...)...)
	cmd.Dir = repo
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			cmd.Wait()
			t.Fatal("git worktree add never reached the checkout")
		}
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait()
	if !strings.Contains(gitOut(t, repo, "worktree", "list", "--porcelain")+"\n", "\nlocked initializing\n") {
		t.Fatalf("no worktree left locked initializing:\n%s", gitOut(t, repo, "worktree", "list", "--porcelain"))
	}
}

func gitOut(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
