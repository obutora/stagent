//go:build linux

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// stubGHFetch links an issue's branch that is on origin already: it only
// fetches it, so the task works on that branch as pushed (its own
// orca.yaml).
const stubGHFetch = `#!/bin/sh
[ "$1 $2" = "issue develop" ] || exit 1
shift 3
while [ $# -gt 0 ]; do
  case "$1" in --name) name=$2; shift 2;; *) shift;; esac
done
git fetch -q origin "+refs/heads/$name:refs/remotes/origin/$name"
`

// setup の承認 end to end (ADR 0023): a branch whose scripts.setup differs
// from the source checkout's stops before setup and the app sees the
// script; task.setup run runs it and approves the content, so the next
// task with that content runs it without asking; another content asks
// again, and skipping it starts the agent without setup.
func TestTaskSetupApprovalEndToEnd(t *testing.T) {
	a, _, _, home := startPersistBridge(t)
	gitIn := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	seed := filepath.Join(home, "seed")
	gitIn(home, "init", "-q", "-b", "main", seed)
	commitOrca := func(setup string) {
		t.Helper()
		orca := "scripts:\n  setup: |\n    " + setup
		if err := os.WriteFile(filepath.Join(seed, "orca.yaml"), []byte(orca), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(seed, "add", "orca.yaml")
		gitIn(seed, "commit", "-q", "-m", setup)
	}
	const changed, other = "echo branch > setup-ran\n", "echo other > setup-ran\n"
	commitOrca("echo main > setup-ran\n")
	for branch, setup := range map[string]string{"changed-1": changed, "changed-2": changed, "other-3": other} {
		gitIn(seed, "switch", "-q", "-c", branch, "main")
		commitOrca(setup)
	}
	gitIn(seed, "switch", "-q", "main")
	gitIn(home, "clone", "-q", "--bare", seed, "origin.git")
	gitIn(home, "clone", "-q", "origin.git", "app")
	repo := filepath.Join(home, "app")

	bin := filepath.Join(home, "stub-bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stubGHFetch), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := os.OpenFile(filepath.Join(home, ".bash_profile"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(profile, "export PATH='%s':\"$PATH\"\n", bin)
	profile.Close()

	create := func(number int, branch string) wire.Task {
		t.Helper()
		var res wire.TaskCreateResult
		a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
			Repo: repo, Kind: wire.TaskKindIssue, Number: number, Title: "t", Branch: branch,
			Command: []string{"sh", "-c", "echo AGENT-RAN; exec sleep 60"}, Cols: 80, Rows: 24,
		}, &res)
		if res.Existed || res.Session == nil {
			t.Fatalf("task.create = %+v", res)
		}
		id := res.Session.ID
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil)
		})
		return res.Task
	}
	pending := func(id string) wire.TaskSetup {
		t.Helper()
		m := a.waitFor(t, "setup pending", func(m *wire.Msg) bool {
			return taskUpdated(m, func(tk wire.Task) bool { return tk.ID == id && tk.SetupPending != nil })
		})
		var tk wire.Task
		json.Unmarshal(m.Params, &tk)
		if tk.State != wire.TaskPreparing || tk.Stage != wire.TaskStageSetup {
			t.Fatalf("pending task %+v", tk)
		}
		return *tk.SetupPending
	}
	ready := func(id string) {
		t.Helper()
		a.waitFor(t, "task ready", func(m *wire.Msg) bool {
			return taskUpdated(m, func(tk wire.Task) bool { return tk.ID == id && tk.State == wire.TaskReady })
		})
	}
	setupRan := func(tk wire.Task) string {
		b, _ := os.ReadFile(filepath.Join(tk.Worktree, "setup-ran"))
		return strings.TrimSpace(string(b))
	}

	first := create(1, "changed-1")
	p := pending(first.ID)
	if p.Script != changed {
		t.Fatalf("pending script %q, want the branch's", p.Script)
	}
	if got := setupRan(first); got != "" {
		t.Fatalf("setup ran before it was approved: %q", got)
	}
	a.call(t, wire.MethodTaskSetup, wire.TaskSetupParams{ID: first.ID, Hash: p.Hash, Run: true}, nil)
	ready(first.ID)
	if got := setupRan(first); got != "branch" {
		t.Fatalf("approved setup: setup-ran %q", got)
	}

	second := create(2, "changed-2")
	ready(second.ID)
	if got := setupRan(second); got != "branch" {
		t.Fatalf("setup with an approved content: setup-ran %q", got)
	}
	a.mu.Lock()
	for _, m := range a.notifs {
		if taskUpdated(m, func(tk wire.Task) bool { return tk.ID == second.ID && tk.SetupPending != nil }) {
			t.Error("an approved content was asked again")
		}
	}
	a.mu.Unlock()

	third := create(3, "other-3")
	if p := pending(third.ID); p.Script != other {
		t.Fatalf("pending script %q", p.Script)
	} else {
		a.call(t, wire.MethodTaskSetup, wire.TaskSetupParams{ID: third.ID, Hash: p.Hash}, nil)
	}
	ready(third.ID)
	if got := setupRan(third); got != "" {
		t.Fatalf("skipped setup ran: %q", got)
	}
}
