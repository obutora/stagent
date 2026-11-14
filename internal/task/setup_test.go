//go:build !windows

package task

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const forkSetup = "echo fork > fork-ran\n"

// pushPR puts a fork's PR number n on origin (refs/pull/<n>/head): main
// with orca.yaml replaced by orcaYAML.
func (f *fixture) pushPR(t *testing.T, n int, orcaYAML string) {
	t.Helper()
	dir := filepath.Join(f.base, "pr-"+strconv.Itoa(n))
	git(t, f.base, "clone", "-q", filepath.Join(f.base, "origin.git"), dir)
	if err := os.WriteFile(filepath.Join(dir, OrcaFile), []byte(orcaYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", OrcaFile)
	git(t, dir, "commit", "-q", "-m", "fork setup")
	git(t, dir, "push", "-q", "origin", "HEAD:refs/pull/"+strconv.Itoa(n)+"/head")
}

// asker is an AskSetup that answers run and records what it was asked.
type asker struct {
	run   bool
	asked []string
}

func (a *asker) ask(_ context.Context, script string) (bool, error) {
	a.asked = append(a.asked, script)
	return a.run, nil
}

// approve writes approvals as the daemon keeps them.
func approve(t *testing.T, path string, approvals ...SetupApproval) {
	t.Helper()
	b, err := json.Marshal(SetupApprovals{Approved: approvals})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The source checkout's own scripts.setup runs without asking.
func TestPrepareSetupSameAsSourceRuns(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	tk := f.task(t, wire.TaskKindIssue, 3, "3-same")
	a := &asker{}
	if _, out, err := f.prepare(t, Prep{Task: tk, AskSetup: a.ask}); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if len(a.asked) != 0 {
		t.Fatalf("asked about the source checkout's setup: %q", a.asked)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "setup-ran")); err != nil {
		t.Fatal("setup did not run")
	}
}

// A fork's PR that changes scripts.setup stops before running it and asks
// with the branch's script: skipped, preparation goes on without it;
// approved, it runs.
func TestPrepareChangedSetupAsks(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	f.pushPR(t, 8, "scripts:\n  setup: |\n    "+forkSetup)
	tk := f.task(t, wire.TaskKindPR, 8, "fork-setup")
	skip := &asker{}
	stages, out, err := f.prepare(t, Prep{Task: tk, AskSetup: skip.ask})
	if err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if len(skip.asked) != 1 || skip.asked[0] != forkSetup {
		t.Fatalf("asked %q, want the branch's setup", skip.asked)
	}
	if stages[len(stages)-1] != wire.TaskStageSetup {
		t.Fatalf("stages %q", stages)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "fork-ran")); err == nil {
		t.Fatal("a skipped setup ran")
	}
	if !strings.Contains(out, "skipped") {
		t.Fatalf("the terminal is not told setup was skipped:\n%s", out)
	}

	run := &asker{run: true}
	if _, out, err := f.prepare(t, Prep{Task: tk, AskSetup: run.ask}); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if len(run.asked) != 1 {
		t.Fatalf("asked %q", run.asked)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "fork-ran")); err != nil {
		t.Fatal("an approved setup did not run")
	}
}

// Without anyone to ask, a changed scripts.setup is not run.
func TestPrepareChangedSetupNobodyToAsk(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	f.pushPR(t, 8, "scripts:\n  setup: |\n    "+forkSetup)
	tk := f.task(t, wire.TaskKindPR, 8, "fork-setup")
	if _, out, err := f.prepare(t, Prep{Task: tk}); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "fork-ran")); err == nil {
		t.Fatal("an unapproved setup ran")
	}
}

// A content approved for the source checkout runs without asking; for
// another checkout, or once the content changes, it asks again.
func TestPrepareApprovedSetup(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	f.pushPR(t, 8, "scripts:\n  setup: |\n    "+forkSetup)
	approvals := filepath.Join(f.base, "setup-approvals.json")
	approve(t, approvals,
		SetupApproval{Repo: f.repo, Hash: SetupHash(forkSetup), At: time.Now()},
		SetupApproval{Repo: "/elsewhere", Hash: SetupHash("echo other\n"), At: time.Now()})
	tk := f.task(t, wire.TaskKindPR, 8, "fork-setup")
	a := &asker{}
	if _, out, err := f.prepare(t, Prep{Task: tk, SetupApprovals: approvals, AskSetup: a.ask}); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if len(a.asked) != 0 {
		t.Fatalf("asked about an approved setup: %q", a.asked)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "fork-ran")); err != nil {
		t.Fatal("the approved setup did not run")
	}

	f.pushPR(t, 9, "scripts:\n  setup: |\n    echo other\n")
	other := f.task(t, wire.TaskKindPR, 9, "fork-other")
	if _, out, err := f.prepare(t, Prep{Task: other, SetupApprovals: approvals, AskSetup: a.ask}); err != nil {
		t.Fatalf("Prepare: %v\n%s", err, out)
	}
	if len(a.asked) != 1 || a.asked[0] != "echo other\n" {
		t.Fatalf("asked %q: an approval of another checkout counted", a.asked)
	}
}

// in_place: switching to a branch whose setup differs from the checkout's
// asks; preparing it again once the checkout is on that branch still
// asks (the checkout's file is then the branch's).
func TestPrepareInPlaceChangedSetupAsks(t *testing.T) {
	f := newFixture(t, map[string]string{"orca.yaml": setupWritesRoot})
	f.pushPR(t, 8, "scripts:\n  setup: |\n    "+forkSetup)
	tk := wire.Task{ID: "t", Repo: f.repo, Kind: wire.TaskKindPR, Number: 8, Branch: "fork-setup", Base: "main", Worktree: f.repo, InPlace: true}
	for i := range 2 {
		a := &asker{}
		if _, out, err := f.prepare(t, Prep{Task: tk, SwitchBranch: true, Setup: true, AskSetup: a.ask}); err != nil {
			t.Fatalf("Prepare: %v\n%s", err, out)
		}
		if len(a.asked) != 1 || a.asked[0] != forkSetup {
			t.Fatalf("attempt %d asked %q", i+1, a.asked)
		}
	}
	if _, err := os.Stat(filepath.Join(f.repo, "fork-ran")); err == nil {
		t.Fatal("a skipped setup ran")
	}
}
