package harness

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// The harness registry (ハーネスの登録表): the one list of the
// coding agents stagent knows. Every list of harnesses and every judgement
// of which harness a program is draws from All. What a harness does its own
// way (reading its transcripts, its hook config files, its approvals) lives
// in per-harness files of the packages that do it, looked up by ID; each of
// those packages has a test that fails for an entry of All it has no
// implementation for.

// Harness is one entry of the registry.
type Harness struct {
	// ID is the harness id on the wire (wire.Session.Harness) and the
	// integration target id of `stagent integrate --harness / --remove`.
	ID string
	// Command is the program the user runs: what the shell wrapper wraps,
	// what doctor looks for, and the process name the agent runs as.
	Command string
	// NativePrefixes are process names of native binaries the npm package
	// runs under another name (codex-<target triple>).
	NativePrefixes []string
	// Packages are the npm packages (current name first), for launchers
	// that run its script by path (Claude Code's cli.js).
	Packages []string
	// Batch is how a command line asks for a non-interactive run.
	Batch Batch
	// TranscriptRoot is the directory the harness keeps its conversations
	// in, for a home directory and the environment the harness runs with.
	TranscriptRoot func(home string, getenv func(string) string) string
	// HookHeldForPrompt: the harness shows its permission prompt while its
	// PermissionRequest hook runs, so the hook may wait until the prompt
	// is answered (Codex shows its prompt only once the hook returned).
	HookHeldForPrompt bool
}

// Batch describes the arguments that make a harness run without its
// terminal UI. The shell wrapper leaves those runs alone, and hooks from
// them are not counted as launches the app misses.
type Batch struct {
	// Subcommands: the first argument is one of them (codex exec).
	Subcommands []string
	// Flags: any argument is one of them (claude -p).
	Flags []string
	// Prefixes: any argument starts with one of them (copilot
	// --prompt=…).
	Prefixes []string
	// Mode is an option whose value other than ModeInteractive asks for a
	// non-interactive run, given as `Mode value` or `Mode=value` (omp
	// --mode json).
	Mode, ModeInteractive string
}

// All is the registry.
var All = []Harness{
	{
		ID:       wire.HarnessClaude,
		Command:  "claude",
		Packages: []string{"@anthropic-ai/claude-code"},
		Batch:    Batch{Flags: []string{"-p", "--print"}},
		TranscriptRoot: func(home string, getenv func(string) string) string {
			dir := getenv("CLAUDE_CONFIG_DIR")
			if dir == "" {
				dir = filepath.Join(home, ".claude")
			}
			return filepath.Join(dir, "projects")
		},
		HookHeldForPrompt: true,
	},
	{
		ID:      wire.HarnessCodex,
		Command: "codex",
		// Codex's npm package has shipped its native binary as
		// codex-<target triple>.
		NativePrefixes: []string{"codex-x86_64-", "codex-aarch64-"},
		Packages:       []string{"@openai/codex"},
		Batch:          Batch{Subcommands: []string{"exec", "e"}},
		// The codex home; rollouts are under its sessions directory.
		TranscriptRoot: func(home string, getenv func(string) string) string {
			if dir := getenv("CODEX_HOME"); dir != "" {
				return dir
			}
			return filepath.Join(home, ".codex")
		},
	},
	{
		ID:       wire.HarnessOmp,
		Command:  "omp",
		Packages: []string{"@oh-my-pi/pi-coding-agent"},
		Batch:    Batch{Flags: []string{"-p", "--print"}, Mode: "--mode", ModeInteractive: "text"},
		TranscriptRoot: func(home string, _ func(string) string) string {
			return filepath.Join(home, ".omp", "agent", "sessions")
		},
	},
	{
		ID:      wire.HarnessPi,
		Command: "pi",
		// Pi runs as node with its script and sets its process title to
		// "pi" (the name Linux shows; macOS shows it as argv[0]). It was
		// published as @mariozechner/pi-coding-agent before.
		Packages: []string{"@earendil-works/pi-coding-agent", "@mariozechner/pi-coding-agent"},
		// --no-session keeps nothing to come back to; the package and
		// account commands run no conversation.
		Batch: Batch{
			Subcommands:     []string{"install", "remove", "uninstall", "update", "list", "config", "auth", "mcp"},
			Flags:           []string{"-p", "--print", "--no-session"},
			Mode:            "--mode",
			ModeInteractive: "text",
		},
		TranscriptRoot: func(home string, getenv func(string) string) string {
			return filepath.Join(PiAgentDir(home, getenv), "sessions")
		},
	},
	{
		ID:      wire.HarnessCopilot,
		Command: "copilot",
		// The npm package's launcher (node npm-loader.js) runs the native
		// binary of @github/copilot-<platform>-<arch>, also called
		// copilot; brew, WinGet and the install script put that binary on
		// PATH.
		Packages: []string{"@github/copilot"},
		// --acp serves the Agent Client Protocol on stdio; the other
		// commands run no conversation.
		Batch: Batch{
			Subcommands: []string{"login", "help", "init", "config", "update", "version", "workflow", "sessions", "memories", "plugin", "mcp", "skill", "instruction", "lsp", "sandbox", "completion", "app"},
			Flags:       []string{"-p", "--prompt", "--acp"},
			Prefixes:    []string{"--prompt="},
		},
		TranscriptRoot: func(home string, getenv func(string) string) string {
			return filepath.Join(CopilotHome(home, getenv), "session-state")
		},
	},
}

// CopilotHome is Copilot CLI's configuration directory (hooks, sessions):
// $COPILOT_HOME, else ~/.copilot.
func CopilotHome(home string, getenv func(string) string) string {
	if dir := getenv("COPILOT_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".copilot")
}

// PiAgentDir is Pi's agent directory (extensions, sessions):
// $PI_CODING_AGENT_DIR, with a leading ~ expanded as Pi does, else
// ~/.pi/agent.
func PiAgentDir(home string, getenv func(string) string) string {
	dir := getenv("PI_CODING_AGENT_DIR")
	switch {
	case dir == "":
		return filepath.Join(home, ".pi", "agent")
	case dir == "~":
		return home
	case strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`):
		return filepath.Join(home, dir[2:])
	}
	return dir
}

// IDs lists the ids of All, in its order.
func IDs() []string {
	ids := make([]string, len(All))
	for i, h := range All {
		ids[i] = h.ID
	}
	return ids
}

// Lookup returns the entry of id.
func Lookup(id string) (Harness, bool) {
	i := slices.IndexFunc(All, func(h Harness) bool { return h.ID == id })
	if i < 0 {
		return Harness{}, false
	}
	return All[i], true
}

// Known reports whether id is a harness of the registry.
func Known(id string) bool {
	_, ok := Lookup(id)
	return ok
}

// Missing lists, in the order of All, the ids of the harnesses a
// per-harness table keyed by harness id has no entry for. Each package's
// test that its table covers the registry checks it is empty.
func Missing[V any](table map[string]V) []string {
	var ids []string
	for _, h := range All {
		if _, ok := table[h.ID]; !ok {
			ids = append(ids, h.ID)
		}
	}
	return ids
}

// Name returns the harness a program called n (a ptable.BaseName) is, ""
// for other programs. It is the one judgement of which harness a program
// is: Of and Command build on it.
func Name(n string) string {
	for _, h := range All {
		if n == h.Command {
			return h.ID
		}
		for _, p := range h.NativePrefixes {
			if strings.HasPrefix(n, p) {
				return h.ID
			}
		}
	}
	return ""
}

// Command maps a command line to the harness its program is,
// wire.HarnessOther for other programs. For a task's prepared session it
// is the command `stagent task run` ends in (wire.AgentArgv).
func Command(argv []string) string {
	argv = wire.AgentArgv(argv)
	if len(argv) == 0 {
		return wire.HarnessOther
	}
	if h := Name(wire.ProgramName(argv[0])); h != "" {
		return h
	}
	return wire.HarnessOther
}

// BatchArgs reports whether a command line of harness id (argv[0] first)
// asks for a non-interactive run, by the rule the shell wrapper applies.
// Every argument is looked at for flags and the mode: omp runs under its
// JavaScript runtime, its script first.
func BatchArgs(id string, argv []string) bool {
	h, ok := Lookup(id)
	if !ok || len(argv) < 2 {
		return false
	}
	return h.Batch.args(argv[1:])
}

func (b Batch) args(args []string) bool {
	if len(b.Subcommands) > 0 && slices.Contains(b.Subcommands, args[0]) {
		return true
	}
	for i, a := range args {
		switch {
		case slices.Contains(b.Flags, a):
			return true
		case slices.ContainsFunc(b.Prefixes, func(p string) bool { return strings.HasPrefix(a, p) }):
			return true
		case b.Mode == "":
		case a == b.Mode && i+1 < len(args) && args[i+1] != b.ModeInteractive:
			return true
		case strings.HasPrefix(a, b.Mode+"=") && a != b.Mode+"="+b.ModeInteractive:
			return true
		}
	}
	return false
}
