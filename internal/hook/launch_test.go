package hook

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/obutora/stagent/internal/ptable"
)

// The harness is the first ancestor that is not a hook shell; only a shell
// above it counts as a terminal.
func TestWinAncestry(t *testing.T) {
	tree := func(names ...string) *ptable.Snapshot {
		// names[0] is the hook (pid 100), each next one its parent.
		var procs []ptable.Proc
		for i, n := range names {
			procs = append(procs, ptable.Proc{PID: 100 + i, PPID: 101 + i, Name: n})
		}
		return ptable.New(procs, nil)
	}
	for _, c := range []struct {
		name     string
		s        *ptable.Snapshot
		harness  int
		terminal bool
	}{
		{"claude in Windows Terminal (hook through Git Bash)", tree("stagent.exe", "bash.exe", "claude.exe", "pwsh.exe", "WindowsTerminal.exe", "explorer.exe"), 102, true},
		{"codex notify from the desktop app", tree("stagent.exe", "codex.exe", "Codex.exe", "explorer.exe"), 101, false},
		{"omp under bun in cmd", tree("stagent.exe", "bun.exe", "omp.exe", "CMD.EXE"), 101, true},
		{"claude started over WMI, hook through PowerShell", tree("stagent.exe", "powershell.exe", "claude.exe", "WmiPrvSE.exe"), 102, false},
		{"parent gone", tree("stagent.exe"), 0, false},
	} {
		h, term := winAncestry(c.s, 100)
		if h != c.harness || term != c.terminal {
			t.Errorf("%s: harness %d terminal %v, want %d %v", c.name, h, term, c.harness, c.terminal)
		}
	}
}

// Codex's originator comes from the rollout file: the hook payload's
// transcript_path, or the notify program's thread-id under CODEX_HOME.
func TestCodexOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	day := filepath.Join(home, "sessions", "2026", "10", "04")
	if err := os.MkdirAll(day, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, first string) string {
		p := filepath.Join(day, name)
		if err := os.WriteFile(p, []byte(first+"\n{\"type\":\"event_msg\"}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tui := write("rollout-2026-10-04T10-00-00-aaaa.jsonl", `{"timestamp":"t","type":"session_meta","payload":{"id":"aaaa","originator":"codex-tui","source":"cli"}}`)
	write("rollout-2026-10-04T11-00-00-bbbb.jsonl", `{"type":"session_meta","payload":{"id":"bbbb","originator":"codex-tui","source":{"subagent":{"other":"guardian"}}}}`)
	write("rollout-2026-10-04T12-00-00-cccc.jsonl", `{"type":"session_meta","payload":{"originator":"Codex Desktop","source":"vscode"}}`)

	for _, c := range []struct {
		name, payload, originator string
		subagent                  bool
	}{
		{"hook payload", `{"session_id":"aaaa","transcript_path":` + strconv.Quote(tui) + `}`, "codex-tui", false},
		{"notify thread-id", `{"type":"agent-turn-complete","thread-id":"cccc"}`, "Codex Desktop", false},
		{"subagent thread", `{"type":"agent-turn-complete","thread-id":"bbbb"}`, "codex-tui", true},
		{"no rollout", `{"type":"agent-turn-complete","thread-id":"dddd"}`, "", false},
		{"glob characters", `{"type":"agent-turn-complete","thread-id":"*"}`, "", false},
	} {
		o, sub := codexOrigin([]byte(c.payload))
		if o != c.originator || sub != c.subagent {
			t.Errorf("%s: originator %q subagent %v, want %q %v", c.name, o, sub, c.originator, c.subagent)
		}
	}
}

