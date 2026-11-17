package harness

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/obutora/stagent/internal/ptable"
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
	// runs under another name (codex-<target triple>), or the program is
	// a link to (grok-<os>-<arch>).
	NativePrefixes []string
	// Packages are the npm packages (current name first), for launchers
	// that run its script by path (Claude Code's cli.js); none for a
	// harness that ships a native binary only.
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
	// QuestionTool is the tool through which the harness asks the user a
	// choice question ("" when it has none): its hooks and transcript rows
	// naming it are a question (PROTOCOL.md "Questions").
	QuestionTool string
	// Resume is the arguments after Command that continue conversation id
	// in the terminal UI: what the app's resumeCommand runs and what
	// stopped.resume starts (ResumeArgv).
	Resume func(id string) []string
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
	// Prompt: a positional argument — neither an option nor the value of
	// one of Values or Interactive — is a prompt the harness runs once
	// without its terminal UI (qwen "fix it"), unless one of Interactive
	// is given too. After `--` every argument is positional.
	Prompt bool
	// Values are the options that take the next argument as their value.
	Values []string
	// Interactive are the options that take a prompt (the next argument,
	// or after `=`) and run it in the terminal UI (qwen -i "fix it").
	Interactive []string
}

// All is the registry.
var All = []Harness{
	{
		ID:       wire.HarnessClaude,
		Command:  "claude",
		Packages: []string{"@anthropic-ai/claude-code"},
		Batch:    Batch{Flags: []string{"-p", "--print"}},
		TranscriptRoot: func(home string, getenv func(string) string) string {
			dir, _ := ClaudeConfigDir(home, getenv)
			return filepath.Join(dir, "projects")
		},
		HookHeldForPrompt: true,
		QuestionTool:      "AskUserQuestion",
		Resume:            func(id string) []string { return []string{"--resume", id} },
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
		QuestionTool: "request_user_input",
		Resume:       func(id string) []string { return []string{"resume", id} },
	},
	{
		ID:       wire.HarnessOmp,
		Command:  "omp",
		Packages: []string{"@oh-my-pi/pi-coding-agent"},
		Batch:    Batch{Flags: []string{"-p", "--print"}, Mode: "--mode", ModeInteractive: "text"},
		TranscriptRoot: func(home string, _ func(string) string) string {
			return filepath.Join(home, ".omp", "agent", "sessions")
		},
		QuestionTool: "ask",
		Resume:       func(id string) []string { return []string{"--resume", id} },
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
		Resume: func(id string) []string { return []string{"--session", id} },
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
		Resume: func(id string) []string { return []string{"--resume=" + id} },
	},
	{
		ID:      wire.HarnessQwen,
		Command: "qwen",
		// node runs the package's cli-entry.js by the path it was invoked
		// as (the bin link called qwen), in one process.
		Packages: []string{"@qwen-code/qwen-code"},
		// A positional prompt runs once and exits (-p is its deprecated
		// form); -i runs it in the terminal UI. --acp, the output formats
		// and the commands run no conversation in the terminal UI
		// (--input-format stream-json reads a stdin that is no terminal,
		// which the wrapper leaves alone anyway).
		Batch: Batch{
			Subcommands: []string{"agents", "attach", "autofix-pr", "auth", "batch", "board", "channel", "extensions", "hooks", "mcp", "memory", "review", "sandbox", "serve", "daemon", "sessions", "update"},
			Flags:       []string{"-p", "--prompt", "--acp", "-o", "--output-format"},
			Prefixes:    []string{"--prompt=", "--output-format="},
			Prompt:      true,
			// Its options that take a value (qwen --help, 0.27.4).
			Values: []string{
				"-m", "--model", "--advisor", "--workflow-mode", "--fallback-model",
				"--system-prompt", "--append-system-prompt", "--output-style",
				"-s", "--sandbox", "--sandbox-image", "--approval-mode", "--channel",
				"--allowed-mcp-server-names", "--disable-mcp-server", "--mcp-config",
				"--allowed-tools", "-e", "--extensions", "--include-directories", "--add-dir",
				"--openai-logging-dir", "--openai-api-key", "--openai-base-url", "--input-format",
				"--json-fd", "--json-file", "--json-schema", "--input-file",
				"-r", "--resume", "--session-id", "--worktree",
				"--max-session-turns", "--max-wall-time", "--max-tool-calls", "--max-subagent-depth",
				"--core-tools", "--exclude-tools", "--disabled-slash-commands", "--auth-type",
				"--proxy", "--telemetry-target", "--telemetry-otlp-endpoint",
				"--telemetry-otlp-protocol", "--telemetry-outfile",
			},
			Interactive: []string{"-i", "--prompt-interactive"},
		},
		TranscriptRoot: func(home string, getenv func(string) string) string {
			return filepath.Join(QwenRuntimeDir(home, getenv), "projects")
		},
		QuestionTool: "ask_user_question",
		Resume:       func(id string) []string { return []string{"--resume", id} },
	},
	{
		ID:      wire.HarnessOpenCode,
		Command: "opencode",
		// OpenCode V2's npm package links its native (Bun-compiled) binary,
		// opencode.exe, as opencode; its terminal UI runs its private server
		// (--standalone, which stagent adds: holder/opencode.go) as a child
		// process of the same binary, `serve --stdio`, where plugins run.
		Packages: []string{"@opencode/cli"},
		// Its commands run no conversation in the terminal UI (`mini` runs
		// one in an interface of its own, which the app does not read); a
		// positional argument is the directory to start in. --completions
		// prints a script.
		Batch: Batch{
			Subcommands: []string{"run", "serve", "service", "session", "api", "debug", "auth", "mcp", "plugin", "models", "stats", "upgrade", "update", "uninstall", "acp", "reload", "pair", "mini"},
			Flags:       []string{"-h", "--help", "-v", "--version", "--completions"},
			Prefixes:    []string{"--completions="},
		},
		// OpenCode keeps its conversations in a database (no file to
		// read): stagent records each conversation its plugin reports, read
		// with `opencode session export`, in a file of its own under its
		// state directory (paths.Layout.StateDir, in $STAGENT_HOME when that
		// is set): transcript/opencode.go.
		TranscriptRoot: func(home string, getenv func(string) string) string {
			if dir := getenv("STAGENT_HOME"); dir != "" {
				home = dir
			}
			return filepath.Join(home, ".ssh-term", "agent", "state", "opencode")
		},
		QuestionTool: "question",
		// --session also creates a session of that id; --standalone keeps
		// the conversation in a server of its own, whose plugin events are
		// that terminal's.
		Resume: func(id string) []string { return []string{"--standalone", "--session", id} },
	},
	{
		ID:      wire.HarnessGrok,
		Command: "grok",
		// Grok Build is one native binary, installed as
		// ~/.grok/downloads/grok-<os>-<arch> and linked as ~/.grok/bin/grok
		// (also as agent, Cursor CLI's name, which is not taken for it):
		// macOS names the process after the link's target, Linux after the
		// link; Windows copies it as grok.exe.
		NativePrefixes: []string{"grok-macos-", "grok-linux-", "grok-windows-"},
		// -p and the prompt options run one turn and print it; the commands
		// run no conversation in the terminal UI (`agent` runs one without
		// it), but `dashboard`, a view of the terminal UI. A positional
		// argument is the first prompt of the terminal UI.
		Batch: Batch{
			Subcommands: []string{
				"agent", "clone", "completions", "doctor", "du", "disk-usage", "export", "help",
				"inspect", "leader", "login", "logout", "mcp", "memory", "models", "plugin",
				"sessions", "setup", "trace", "update", "usage", "version", "v", "worker",
				"worktree", "wrap",
			},
			Flags:    []string{"-p", "--single", "--prompt-file", "--prompt-json", "-h", "--help", "-v", "--version"},
			Prefixes: []string{"--single=", "--prompt-file=", "--prompt-json="},
		},
		// sessions/<cwd, percent-encoded>/<session id>/updates.jsonl.
		TranscriptRoot: func(home string, getenv func(string) string) string {
			return filepath.Join(GrokHome(home, getenv), "sessions")
		},
		QuestionTool: "ask_user_question",
		Resume:       func(id string) []string { return []string{"--resume", id} },
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
	if dir := getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return expandHome(home, dir)
	}
	return filepath.Join(home, ".pi", "agent")
}

// QwenHome is Qwen Code's directory (settings, extensions): $QWEN_HOME,
// with a leading ~ expanded as Qwen Code does, else ~/.qwen.
func QwenHome(home string, getenv func(string) string) string {
	if dir := getenv("QWEN_HOME"); dir != "" {
		return expandHome(home, dir)
	}
	return filepath.Join(home, ".qwen")
}

// QwenRuntimeDir is where Qwen Code records its conversations
// (projects/<cwd>/chats/<session id>.jsonl): $QWEN_RUNTIME_DIR, with ~
// expanded, else QwenHome. Its setting advanced.runtimeOutputDir moves it
// too; that is not read here (the hooks name the conversation's file).
func QwenRuntimeDir(home string, getenv func(string) string) string {
	if dir := getenv("QWEN_RUNTIME_DIR"); dir != "" {
		return expandHome(home, dir)
	}
	return QwenHome(home, getenv)
}

// GrokHome is Grok Build's directory (hooks, sessions): $GROK_HOME, else
// ~/.grok.
func GrokHome(home string, getenv func(string) string) string {
	if dir := getenv("GROK_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".grok")
}

// expandHome expands a leading ~ of dir as Pi and Qwen Code do.
func expandHome(home, dir string) string {
	switch {
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

// QuestionToolOf is the question tool of harness id ("" when it has none).
func QuestionToolOf(id string) string {
	h, _ := Lookup(id)
	return h.QuestionTool
}

// QuestionToolHarness returns the harness whose question tool is tool:
// harness id when tool is its question tool (Qwen Code and Grok Build both
// call theirs ask_user_question), else the first of All whose it is.
func QuestionToolHarness(tool, id string) (Harness, bool) {
	if h, ok := Lookup(id); ok && h.QuestionTool != "" && h.QuestionTool == tool {
		return h, true
	}
	i := slices.IndexFunc(All, func(h Harness) bool { return h.QuestionTool != "" && h.QuestionTool == tool })
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

// ResumeArgv is the command line that continues conversation conv of
// harness id (its Command first); false for an unknown harness.
func ResumeArgv(id, conv string) ([]string, bool) {
	h, ok := Lookup(id)
	if !ok {
		return nil, false
	}
	return append([]string{h.Command}, h.Resume(conv)...), true
}

// ClaudeConfigDir is the directory Claude Code keeps its files in, the
// account its limit windows are kept under: $CLAUDE_CONFIG_DIR as a clean
// path, else ~/.claude. env is the variable as set ("" when unset), which
// names Claude Code's Keychain item (usage.KeychainService).
func ClaudeConfigDir(home string, getenv func(string) string) (dir, env string) {
	if env = getenv("CLAUDE_CONFIG_DIR"); env != "" {
		return filepath.Clean(env), env
	}
	return filepath.Join(home, ".claude"), ""
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
// A harness run by its JavaScript runtime (claude or qwen under node, omp
// under bun) is judged by the arguments after its script — the first
// argument that is not an option, as Of finds it: the runtime's own
// options before it are not the harness's.
func BatchArgs(id string, argv []string) bool {
	h, ok := Lookup(id)
	if !ok || len(argv) < 2 {
		return false
	}
	args := argv[1:]
	if Interpreter(ptable.Argv0(argv)) {
		script := slices.IndexFunc(args, func(a string) bool { return !strings.HasPrefix(a, "-") })
		if script < 0 {
			return false
		}
		args = args[script+1:]
	}
	return h.Batch.args(args)
}

func (b Batch) args(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if len(b.Subcommands) > 0 && slices.Contains(b.Subcommands, args[0]) {
		return true
	}
	var value, ended, interactive, prompt bool
	for i, a := range args {
		if b.Prompt {
			switch {
			case value:
				value = false
			case ended || a == "-" || !strings.HasPrefix(a, "-"):
				prompt = true
			case a == "--":
				ended = true
			case slices.Contains(b.Interactive, a):
				interactive, value = true, true
			case slices.ContainsFunc(b.Interactive, func(o string) bool { return strings.HasPrefix(a, o+"=") }):
				interactive = true
			case slices.Contains(b.Values, a):
				value = true
			}
		}
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
	return prompt && !interactive
}
