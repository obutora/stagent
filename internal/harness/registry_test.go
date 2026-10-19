package harness

import (
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
		{"claude", []string{"claude", "--mode", "json"}, false}, // --mode is omp's
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
		if h.Command == "" || h.Package == "" || h.TranscriptRoot == nil {
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
