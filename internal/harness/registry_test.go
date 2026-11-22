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
		// Claude Code and omp run directly or as their runtime's script
		// (the process the hook reads): only the arguments after the
		// script are the harness's, so a runtime flag before it that
		// reads like a batch flag is not one.
		{"claude", []string{"/usr/local/bin/claude", "--print", "hi"}, true},
		{"claude", []string{"node", "/u/lib/node_modules/@anthropic-ai/claude-code/cli.js", "-p", "hi"}, true},
		{"claude", []string{"node", "--max-old-space-size=4096", "/u/lib/node_modules/@anthropic-ai/claude-code/cli.js", "--print", "hi"}, true},
		{"claude", []string{"node", "/u/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, false},
		{"claude", []string{"node", "/u/lib/node_modules/@anthropic-ai/claude-code/cli.js", "--resume", "0193"}, false},
		{"claude", []string{"node", "--print", "/u/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, false},
		{"claude", []string{"node", "-p"}, false}, // no script: nothing is the harness's
		{"omp", []string{"/home/u/.bun/bin/omp", "-p", "hi"}, true},
		{"omp", []string{"/home/u/.bun/bin/omp", "--mode", "json"}, true},
		{"omp", []string{"/home/u/.bun/bin/omp"}, false},
		{"omp", []string{"/home/u/.bun/bin/omp", "--mode", "text"}, false},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp"}, false},
		{"omp", []string{"bun", "--smol", "/home/u/.bun/bin/omp", "--print", "hi"}, true},
		{"omp", []string{"bun", "-p", "/home/u/.bun/bin/omp"}, false},
		{"omp", []string{"bun", "--mode=json", "/home/u/.bun/bin/omp", "--model", "x"}, false},
		{"omp", []string{"bun", "--print"}, false},
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
		// A positional prompt runs qwen one-shot unless -i asks for the
		// interactive UI; option values are not prompts.
		{"qwen", []string{"qwen", "-p", "hi"}, true},
		{"qwen", []string{"qwen", "--prompt", "hi"}, true},
		{"qwen", []string{"qwen", "fix it"}, true},
		{"qwen", []string{"qwen", "--approval-mode", "default", "fix it"}, true},
		{"qwen", []string{"qwen", "--acp"}, true},
		{"qwen", []string{"qwen", "-o", "json"}, true},
		{"qwen", []string{"qwen", "--output-format=json"}, true},
		{"qwen", []string{"qwen", "mcp", "list"}, true},
		{"qwen", []string{"qwen", "serve"}, true},
		{"qwen", []string{"qwen", "--", "-x"}, true},
		{"qwen", []string{"qwen"}, false},
		{"qwen", []string{"qwen", "-i", "fix it"}, false},
		{"qwen", []string{"qwen", "--prompt-interactive=fix it"}, false},
		{"qwen", []string{"qwen", "fix it", "-i", "more"}, false},
		{"qwen", []string{"qwen", "--approval-mode", "default", "--resume", "a2730fef"}, false},
		{"qwen", []string{"qwen", "-r", "a2730fef", "-m", "qwen3", "-c"}, false},
		{"qwen", []string{"qwen", "--resume=a2730fef", "--yolo"}, false},
		{"qwen", []string{"qwen", "--input-format", "text"}, false},
		// The hook reads the harness's own process: node, its script, the
		// arguments.
		{"qwen", []string{"node", "/u/.npm/bin/qwen", "--approval-mode", "default", "--resume", "a2730fef"}, false},
		{"qwen", []string{"node", "/u/.npm/bin/qwen", "hi"}, true},
		{"qwen", []string{"node", "--expose-gc", "/u/lib/node_modules/@qwen-code/qwen-code/cli.js", "-i", "hi"}, false},
		// OpenCode's commands but `mini` run no conversation in its
		// terminal UI; a positional argument is the directory to start in.
		{"opencode", []string{"opencode", "run", "fix it"}, true},
		{"opencode", []string{"opencode", "session", "export", "ses_1"}, true},
		{"opencode", []string{"opencode", "serve", "--stdio", "--port", "0"}, true},
		{"opencode", []string{"opencode", "service", "stop"}, true},
		{"opencode", []string{"opencode", "mini"}, true},
		{"opencode", []string{"opencode", "--version"}, true},
		{"opencode", []string{"opencode", "--completions=zsh"}, true},
		{"opencode", []string{"opencode"}, false},
		{"opencode", []string{"opencode", "--standalone", "--session", "ses_1"}, false},
		{"opencode", []string{"opencode", "/home/u/proj", "--prompt", "fix it"}, false},
		{"opencode", []string{"/u/node_modules/.bin/opencode", "-c"}, false},
		// Grok Build: -p and the prompt options run one turn; its commands
		// but dashboard run no conversation in the terminal UI; a
		// positional argument is the terminal UI's first prompt.
		{"grok", []string{"grok", "-p", "hi"}, true},
		{"grok", []string{"grok", "--single", "hi"}, true},
		{"grok", []string{"grok", "--single=hi"}, true},
		{"grok", []string{"grok", "-m", "grok-4", "--prompt-file", "p.txt"}, true},
		{"grok", []string{"grok", "--prompt-json={}"}, true},
		{"grok", []string{"grok", "agent", "stdio"}, true},
		{"grok", []string{"grok", "sessions", "list"}, true},
		{"grok", []string{"grok", "doctor"}, true},
		{"grok", []string{"grok", "update"}, true},
		{"grok", []string{"grok", "--version"}, true},
		{"grok", []string{"grok"}, false},
		{"grok", []string{"grok", "dashboard"}, false},
		{"grok", []string{"grok", "fix the update script"}, false},
		{"grok", []string{"/home/u/.grok/bin/grok", "--resume", "01a1e529"}, false},
		{"grok", []string{"grok", "-c"}, false},
		// Antigravity CLI: -p / --print / --prompt run one turn; its
		// commands run no conversation in the terminal UI; -i runs its
		// prompt in the terminal UI (a positional argument is rejected).
		{"antigravity", []string{"agy", "-p", "hi"}, true},
		{"antigravity", []string{"agy", "--print", "hi"}, true},
		{"antigravity", []string{"agy", "--prompt=hi"}, true},
		{"antigravity", []string{"agy", "acp"}, true},
		{"antigravity", []string{"agy", "mcp", "list"}, true},
		{"antigravity", []string{"agy", "update"}, true},
		{"antigravity", []string{"agy", "--version"}, true},
		{"antigravity", []string{"agy"}, false},
		{"antigravity", []string{"agy", "-i", "fix the -p flag"}, false},
		{"antigravity", []string{"agy", "--settings", `{"agentMode":""}`, "--conversation", "215bbd83"}, false},
		{"antigravity", []string{"/home/u/.local/bin/agy", "-c"}, false},
		{"claude", []string{"claude", "fix it"}, false},         // a positional prompt is interactive in the others
		{"claude", []string{"claude", "--mode", "json"}, false}, // --mode is omp's and pi's
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
		if h.Command == "" || h.TranscriptRoot == nil || h.Resume == nil {
			t.Errorf("%s: command, transcript root and resume are required", h.ID)
		}
		if Name(h.Command) != h.ID {
			t.Errorf("%s: its command %q is judged %q", h.ID, h.Command, Name(h.Command))
		}
	}
}

// Each harness continues a conversation the way the app's resumeCommand
// does; an unknown harness has none.
func TestResumeArgv(t *testing.T) {
	want := map[string][]string{
		wire.HarnessClaude:      {"claude", "--resume", "c1"},
		wire.HarnessCodex:       {"codex", "resume", "c1"},
		wire.HarnessCopilot:     {"copilot", "--resume=c1"},
		wire.HarnessOmp:         {"omp", "--resume", "c1"},
		wire.HarnessPi:          {"pi", "--session", "c1"},
		wire.HarnessQwen:        {"qwen", "--resume", "c1"},
		wire.HarnessOpenCode:    {"opencode", "--standalone", "--session", "c1"},
		wire.HarnessGrok:        {"grok", "--resume", "c1"},
		wire.HarnessAntigravity: {"agy", "--conversation", "c1"},
	}
	if m := Missing(want); len(m) > 0 {
		t.Fatalf("no expected resume command for %v", m)
	}
	for id, argv := range want {
		if got, ok := ResumeArgv(id, "c1"); !ok || !slices.Equal(got, argv) {
			t.Errorf("%s: resume %q, %v; want %q", id, got, ok, argv)
		}
	}
	if got, ok := ResumeArgv(wire.HarnessOther, "c1"); ok {
		t.Errorf("other: resume %q", got)
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

// Qwen Code keeps its conversations under its runtime directory:
// $QWEN_RUNTIME_DIR, else $QWEN_HOME (both with ~ expanded, as Qwen Code
// does), else ~/.qwen, in projects/.
func TestQwenTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessQwen)
	for _, c := range []struct {
		runtime, home, want string
	}{
		{"", "", filepath.Join("/home/u", ".qwen", "projects")},
		{"", "/srv/qwen", filepath.Join("/srv/qwen", "projects")},
		{"/run/qwen", "/srv/qwen", filepath.Join("/run/qwen", "projects")},
		{"", "~/q", filepath.Join("/home/u", "q", "projects")},
	} {
		got := h.TranscriptRoot("/home/u", func(k string) string {
			switch k {
			case "QWEN_RUNTIME_DIR":
				return c.runtime
			case "QWEN_HOME":
				return c.home
			}
			return ""
		})
		if got != c.want {
			t.Errorf("QWEN_RUNTIME_DIR=%q QWEN_HOME=%q: root %q, want %q", c.runtime, c.home, got, c.want)
		}
	}
}

// OpenCode keeps its conversations in a database; stagent records each one
// it sees in a file of its own, under its state directory (in
// $STAGENT_HOME when that isolates the installation).
func TestOpenCodeTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessOpenCode)
	for stagentHome, want := range map[string]string{
		"":           filepath.Join("/home/u", ".ssh-term", "agent", "state", "opencode"),
		"/srv/agent": filepath.Join("/srv/agent", ".ssh-term", "agent", "state", "opencode"),
	} {
		got := h.TranscriptRoot("/home/u", func(k string) string {
			if k == "STAGENT_HOME" {
				return stagentHome
			}
			return ""
		})
		if got != want {
			t.Errorf("STAGENT_HOME=%q: root %q, want %q", stagentHome, got, want)
		}
	}
}

// Grok Build keeps its conversations under $GROK_HOME/sessions, ~/.grok
// by default.
func TestGrokTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessGrok)
	for env, want := range map[string]string{
		"":          filepath.Join("/home/u", ".grok", "sessions"),
		"/srv/grok": filepath.Join("/srv/grok", "sessions"),
	} {
		got := h.TranscriptRoot("/home/u", func(k string) string {
			if k == "GROK_HOME" {
				return env
			}
			return ""
		})
		if got != want {
			t.Errorf("GROK_HOME=%q: root %q, want %q", env, got, want)
		}
	}
}

// Antigravity CLI keeps its conversations in ~/.gemini/antigravity-cli,
// which no environment variable moves.
func TestAntigravityTranscriptRoot(t *testing.T) {
	h, _ := Lookup(wire.HarnessAntigravity)
	got := h.TranscriptRoot("/home/u", func(string) string { return "/srv/elsewhere" })
	if want := filepath.Join("/home/u", ".gemini", "antigravity-cli"); got != want {
		t.Errorf("root %q, want %q", got, want)
	}
}

// Claude's configuration directory is CLAUDE_CONFIG_DIR as a clean path
// (the variable as set kept apart), else ~/.claude.
func TestClaudeConfigDir(t *testing.T) {
	for _, c := range []struct{ env, dir string }{
		{"", "/home/u/.claude"},
		{"/home/u/work/", "/home/u/work"},
		{"/home/u//work/.", "/home/u/work"},
	} {
		dir, env := ClaudeConfigDir("/home/u", func(k string) string {
			if k == "CLAUDE_CONFIG_DIR" {
				return c.env
			}
			return ""
		})
		if dir != filepath.FromSlash(c.dir) || env != c.env {
			t.Errorf("ClaudeConfigDir with %q = %q, %q; want %q, %q", c.env, dir, env, c.dir, c.env)
		}
	}
}
