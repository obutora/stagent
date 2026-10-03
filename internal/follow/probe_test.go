package follow

import (
	"errors"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/ptable"
)

// The probe finds an agent in a session as `follow --session` does — in
// the session program's tree and in the pane a multiplexer client there
// shows — and counts it only in front of the user, except on platforms
// without a foreground process group.
func TestAgentProbeRunning(t *testing.T) {
	tr := newTree()
	tr.add(500, 1, "stagent", "stagent", "run", "--detached", "--", "bash", "-l")
	tr.add(501, 500, "bash", "bash", "-l")
	claude := tr.add(502, 501, "claude", "claude").fg()
	// A second session whose shell shows a tmux pane running codex.
	tr.add(600, 1, "stagent", "stagent", "run", "--detached", "--", "bash", "-l")
	tr.add(601, 600, "bash", "bash", "-l")
	tr.add(602, 601, "tmux: client", "tmux", "attach").fg()
	tr.add(700, 1, "tmux: server", "tmux", "new")
	tr.add(701, 700, "bash", "-bash")
	tr.add(702, 701, "node", "node", "/usr/lib/node_modules/@openai/codex/bin/codex.js").fg()
	mux := &fakeMux{tmux: map[string][]tmuxClient{"": {{clientPID: 602, panePID: 701, paneID: "%1", session: "main"}}}}

	var takeErr error
	take := func() (*ptable.Snapshot, error) {
		if takeErr != nil {
			return nil, takeErr
		}
		return tr.snapshot(), nil
	}
	p := newAgentProbe(take, testLocator(mux), false)
	checks := []SessionAgent{
		{PID: 501, Harness: "claude"},
		{PID: 501, Harness: "codex"},
		{PID: 601, Harness: "codex"},
		{PID: 0, Harness: "claude"},
	}
	run := func() []bool {
		t.Helper()
		got, err := p.Running(checks)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := run(); !slices.Equal(got, []bool{true, false, true, false}) {
		t.Fatalf("running = %v", got)
	}

	// Suspended (the shell is in the foreground again): not running,
	// unless the platform has no foreground to tell.
	claude.bg()
	if got := run(); got[0] {
		t.Fatalf("background claude counted as running: %v", got)
	}
	p.noForeground = true
	if got := run(); !got[0] {
		t.Fatalf("without foreground info, claude in the tree not counted: %v", got)
	}

	// Gone.
	tr.remove(502)
	if got := run(); got[0] {
		t.Fatalf("exited claude counted as running: %v", got)
	}

	takeErr = errors.New("no process table")
	if _, err := p.Running(checks); err == nil {
		t.Fatal("snapshot error not reported")
	}
}
