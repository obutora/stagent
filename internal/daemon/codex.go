package daemon

import "github.com/obutora/stagent/internal/wire"

// codexDaemon: Codex's approval menu is watched on the screen, and the
// originator of its conversation (session_meta) tells how it was started.
var codexDaemon = daemonHarness{
	watchPrompt: true,
	launch: func(p wire.HookEventParams) launchKind {
		switch {
		case p.Subagent:
			return launchBatch
		case p.Originator == "":
			return launchUnknown
		case codexDesktop[p.Originator]:
			return launchIDE
		case !codexTerminal[p.Originator]:
			return launchBatch // codex_exec, codex_sdk_ts, …
		}
		// The terminal UI, typed in a terminal or run by a program
		// without one: judged like the others.
		return launchUnknown
	},
}

// Codex originators (session_meta.originator): its terminal UI (current
// and older names) and the apps that run it without a terminal.
var (
	codexTerminal = map[string]bool{"codex-tui": true, "codex_cli_rs": true}
	codexDesktop  = map[string]bool{"Codex Desktop": true, "codex_vscode": true}
)
