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

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// session.spawn {task_id} ties the new session to an in_place task; a
// session started in the same checkout without it is not tied.
func TestSpawnWithTaskID(t *testing.T) {
	a, l, _, home := startPersistBridge(t)
	conn, err := daemonclient.Dial(l, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	d := rpc.NewClient(conn, nil)
	t.Cleanup(func() { d.Close() })
	var reg wire.TaskRegisterResult
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.Call(ctx, wire.MethodTaskRegister, wire.TaskRegisterParams{
		Repo: home, Kind: wire.TaskKindIssue, Number: 12, Title: "t", InPlace: true, SessionID: wire.NewSessionID(),
	}, &reg); err != nil {
		t.Fatal(err)
	}

	var tied, untied wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cwd: home, TaskID: reg.Task.ID, Cols: 80, Rows: 24}, &tied)
	t.Cleanup(func() {
		a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: tied.Session.ID, Signal: wire.SignalKill}, nil)
	})
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cwd: home, Cols: 80, Rows: 24}, &untied)
	t.Cleanup(func() {
		a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: untied.Session.ID, Signal: wire.SignalKill}, nil)
	})

	// The holder registers on its own: wait for what the daemon decided.
	a.waitFor(t, "tied session", func(m *wire.Msg) bool {
		return sessionUpdated(m, tied.Session.ID, func(s wire.Session) bool { return s.TaskID == reg.Task.ID })
	})
	m := a.waitFor(t, "untied session", func(m *wire.Msg) bool {
		return sessionUpdated(m, untied.Session.ID, func(wire.Session) bool { return true })
	})
	if sessionUpdated(m, untied.Session.ID, func(s wire.Session) bool { return s.TaskID != "" }) {
		t.Fatalf("a session started in the checkout without task_id is tied: %s", m.Params)
	}
}

// stubGH links an issue's branch the way `gh issue develop` does: made on
// origin from base, then fetched.
const stubGH = `#!/bin/sh
[ "$1 $2" = "issue develop" ] || exit 1
shift 3
while [ $# -gt 0 ]; do
  case "$1" in --base) base=$2; shift 2;; --name) name=$2; shift 2;; *) shift;; esac
done
git push -q origin "refs/remotes/origin/$base:refs/heads/$name" &&
git fetch -q origin "+refs/heads/$name:refs/remotes/origin/$name"
`

func taskUpdated(m *wire.Msg, pred func(wire.Task) bool) bool {
	var tk wire.Task
	return m.Method == wire.NotifyTaskUpdated && json.Unmarshal(m.Params, &tk) == nil && pred(tk)
}

// task.create end to end: the prepared session starts in the empty task
// place, `stagent task run` makes the branch and the worktree there and
// runs setup; setup failing leaves the task failed with no agent, and
// task.create again carries on in the same worktree up to the agent: the
// task is ready.
func TestTaskCreateEndToEnd(t *testing.T) {
	a, _, _, home := startPersistBridge(t)
	gitIn := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed := filepath.Join(home, "seed")
	gitIn(home, "init", "-q", "-b", "main", seed)
	orca := "scripts:\n  setup: |\n    test -f \"$ORCA_ROOT_PATH/ok\"\n    echo set up > setup-ran\n"
	if err := os.WriteFile(filepath.Join(seed, "orca.yaml"), []byte(orca), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(seed, "add", "orca.yaml")
	gitIn(seed, "commit", "-q", "-m", "init")
	gitIn(home, "clone", "-q", "--bare", seed, "origin.git")
	gitIn(home, "clone", "-q", "origin.git", "app")
	repo := filepath.Join(home, "app")
	place := filepath.Join(home, "app-42")

	// The bridge takes its sessions' environment from the login shell
	// when it first starts one: put the stub gh first on that PATH.
	bin := filepath.Join(home, "stub-bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stubGH), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, err := os.OpenFile(filepath.Join(home, ".bash_profile"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(profile, "export PATH='%s':\"$PATH\"\n", bin)
	profile.Close()

	create := func() wire.TaskCreateResult {
		var res wire.TaskCreateResult
		a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{
			Repo: repo, Kind: wire.TaskKindIssue, Number: 42, Title: "Make it work", URL: "https://github.com/o/r/issues/42",
			Command: []string{"sh", "-c", "echo AGENT-RAN; exec sleep 60"}, Cols: 80, Rows: 24,
		}, &res)
		if res.Existed || res.Session == nil {
			t.Fatalf("task.create = %+v", res)
		}
		id := res.Session.ID
		// The session of a failed preparation has ended by then.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a.Call(ctx, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil)
		})
		if res.Session.Cwd != place {
			t.Fatalf("prepared session cwd %q, want %q", res.Session.Cwd, place)
		}
		return res
	}

	first := create()
	a.waitFor(t, "task failed at setup", func(m *wire.Msg) bool {
		return taskUpdated(m, func(tk wire.Task) bool {
			return tk.ID == first.Task.ID && tk.State == wire.TaskFailed && tk.Stage == wire.TaskStageSetup
		})
	})
	if got := gitIn(place, "symbolic-ref", "--short", "HEAD"); got != "42-make-it-work" {
		t.Fatalf("worktree on %q", got)
	}
	if _, err := os.Stat(filepath.Join(place, "setup-ran")); err == nil {
		t.Fatal("setup went on after failing")
	}

	if err := os.WriteFile(filepath.Join(repo, "ok"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	second := create()
	if second.Task.ID != first.Task.ID {
		t.Fatalf("retry made task %s, want %s", second.Task.ID, first.Task.ID)
	}
	a.waitFor(t, "task ready", func(m *wire.Msg) bool {
		return taskUpdated(m, func(tk wire.Task) bool { return tk.ID == first.Task.ID && tk.State == wire.TaskReady })
	})
	a.waitFor(t, "prepared session tied to the task", func(m *wire.Msg) bool {
		return sessionUpdated(m, second.Session.ID, func(s wire.Session) bool { return s.TaskID == first.Task.ID && s.Cwd == place })
	})
	if _, err := os.Stat(filepath.Join(place, "setup-ran")); err != nil {
		t.Fatal("setup did not run again")
	}
	var again wire.TaskCreateResult
	a.call(t, wire.MethodTaskCreate, wire.TaskCreateParams{Repo: repo, Kind: wire.TaskKindIssue, Number: 42, Command: []string{"claude"}}, &again)
	if !again.Existed || again.Task.State != wire.TaskReady {
		t.Fatalf("task.create of a ready task = %+v", again)
	}
}
