package harness

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func TestCommand(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"claude", "fix it"}, wire.HarnessClaude},
		{[]string{`C:\Users\u\AppData\Roaming\npm\codex.cmd`}, wire.HarnessCodex},
		{[]string{"/bin/bash", "-l"}, wire.HarnessOther},
		{nil, wire.HarnessOther},
		// A task's prepared session is told by what `stagent task run`
		// ends in.
		{[]string{"/home/u/.local/bin/stagent", "task", "run", "0123456789abcdef", "--", "claude", "Issue #3"}, wire.HarnessClaude},
		{[]string{`C:\stagent\stagent.exe`, "task", "run", "--switch-branch", "0123456789abcdef", "--", "omp.cmd"}, wire.HarnessOmp},
		{[]string{"/usr/bin/stagent", "task", "run", "0123456789abcdef", "--", "/bin/bash", "-l"}, wire.HarnessOther},
		// An agent schedule's run: the agent its wrapper starts.
		{[]string{"/usr/bin/stagent", "schedule", "agent", "--result", "/tmp/r.json", "--", "codex", "exec", "--json", "--", "hi"}, wire.HarnessCodex},
		{[]string{"/usr/bin/stagent", "run", "--", "claude"}, wire.HarnessOther},
	} {
		if got := Command(c.argv); got != c.want {
			t.Errorf("Command(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// BatchArgs follows the shell wrapper's rule for non-interactive runs.
func TestBatchArgs(t *testing.T) {
	for _, c := range []struct {
		harness string
		argv    []string
		want    bool
	}{
		{"claude", []string{"claude", "-p", "hi"}, true},
		{"claude", []string{"node", "/lib/cli.js", "--model", "haiku", "--print", "hi"}, true},
		{"claude", []string{"claude", "fix the -p flag"}, false},
		{"claude", []string{"claude"}, false},
		{"codex", []string{"/vendor/codex", "exec", "hi"}, true},
		{"codex", []string{"/vendor/codex", "e", "hi"}, true},
		{"codex", []string{"/vendor/codex", "--model", "o3", "exec"}, false}, // only the first argument, like the wrapper
		{"codex", []string{"/vendor/codex", "resume", "-p", "x"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "-p", "hi"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode", "json"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode=rpc"}, true},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode", "text"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode=text", "--model", "x"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp", "--mode"}, false},
		{"pi", []string{"pi", "-p", "hi"}, true},
		{"pi", []string{"pi", "--print", "hi"}, true},
		{"pi", []string{"pi", "--mode", "json", "hi"}, true},
		{"pi", []string{"pi", "--mode=rpc"}, true},
		{"pi", []string{"pi", "--no-session"}, true},
		{"pi", []string{"pi", "--mode", "text"}, false},
		{"pi", []string{"pi", "--session", "0193"}, false},
		// Its package and account commands run no conversation either.
		{"pi", []string{"pi", "install", "npm:pi-foo"}, true},
		{"pi", []string{"pi", "update"}, true},
		{"pi", []string{"pi", "fix", "the", "update", "script"}, false},
		{"copilot", []string{"copilot", "-p", "hi"}, true},
		{"copilot", []string{"copilot", "--prompt", "hi"}, true},
		{"copilot", []string{"copilot", "--model", "x", "--prompt=hi"}, true},
		{"copilot", []string{"copilot", "--acp"}, true},
		{"copilot", []string{"/x/node_modules/@github/copilot-darwin-arm64/copilot", "login"}, true},
		{"copilot", []string{"copilot", "help", "config"}, true},
		{"copilot", []string{"copilot", "--resume=0193"}, false},
		{"copilot", []string{"copilot", "-i", "fix the -p flag"}, false},
		{"copilot", []string{"copilot", "--mode", "autopilot"}, false}, // its --mode is the agent's, still interactive
		{"claude", []string{"claude", "--mode", "json"}, false},        // --mode is omp's and pi's
		{"other", []string{"vim", "-p"}, false},
	} {
		if got := BatchArgs(c.harness, c.argv); got != c.want {
			t.Errorf("BatchArgs(%s, %q) = %v, want %v", c.harness, c.argv, got, c.want)
		}
	}
}

// Every entry is complete: what the other packages build from the
// registry needs these.
func TestRegistryEntriesComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, h := range All {
		if h.ID == "" || h.ID == wire.HarnessOther || seen[h.ID] {
			t.Errorf("entry %+v: id empty, other or duplicated", h)
		}
		seen[h.ID] = true
		if h.Command == "" || len(h.Packages) == 0 || h.TranscriptRoot == nil {
			t.Errorf("%s: command, package and transcript root are required", h.ID)
		}
		if Name(h.Command) != h.ID {
			t.Errorf("%s: its command %q is judged %q", h.ID, h.Command, Name(h.Command))
		}
	}
}

// Missing names the registry's harnesses a per-harness table has no entry
// for, in registry order; entries for other ids do not count.
func TestMissing(t *testing.T) {
	table := map[string]int{"not-a-harness": 0}
	if got := Missing(table); !slices.Equal(got, IDs()) {
		t.Errorf("Missing of a table without harnesses = %q, want %q", got, IDs())
	}
	for _, h := range All {
		table[h.ID] = 1
	}
	if got := Missing(table); len(got) != 0 {
		t.Errorf("Missing of a complete table = %q", got)
	}
	delete(table, wire.HarnessCodex)
	if got := Missing(table); !slices.Equal(got, []string{wire.HarnessCodex}) {
		t.Errorf("Missing without codex = %q", got)
	}
}

// Pi keeps its sessions in its agent directory, $PI_CODING_AGENT_DIR
// (with ~ expanded, as Pi does) or ~/.pi/agent.
func TestPiTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessPi)
	for _, c := range []struct {
		dir, want string
	}{
		{"", filepath.Join("/home/u", ".pi", "agent", "sessions")},
		{"/srv/pi", filepath.Join("/srv/pi", "sessions")},
		{"~/pi-agent", filepath.Join("/home/u", "pi-agent", "sessions")},
		{"~", filepath.Join("/home/u", "sessions")},
	} {
		got := h.TranscriptRoot("/home/u", func(k string) string {
			if k == "PI_CODING_AGENT_DIR" {
				return c.dir
			}
			return ""
		})
		if got != c.want {
			t.Errorf("PI_CODING_AGENT_DIR=%q: root %q, want %q", c.dir, got, c.want)
		}
	}
}

// Copilot CLI keeps its sessions under $COPILOT_HOME (else ~/.copilot) in
// session-state/<session id>/events.jsonl.
func TestCopilotTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessCopilot)
	for _, c := range []struct {
		dir, want string
	}{
		{"", filepath.Join("/home/u", ".copilot", "session-state")},
		{"/srv/copilot", filepath.Join("/srv/copilot", "session-state")},
	} {
		got := h.TranscriptRoot("/home/u", func(k string) string {
			if k == "COPILOT_HOME" {
				return c.dir
			}
			return ""
		})
		if got != c.want {
			t.Errorf("COPILOT_HOME=%q: root %q, want %q", c.dir, got, c.want)
		}
	}
}
