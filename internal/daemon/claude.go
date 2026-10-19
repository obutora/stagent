package daemon

import (
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// claudeDaemon: Claude Code's permission menu is watched on the screen, and
// its entry point (CLAUDE_CODE_ENTRYPOINT) tells how it was started: cli
// is the terminal, sdk-* automation, anything else an app or extension.
var claudeDaemon = daemonHarness{
	watchPrompt: true,
	launch: func(p wire.HookEventParams) launchKind {
		switch {
		case p.Entrypoint == "":
			return launchUnknown
		case strings.HasPrefix(p.Entrypoint, "sdk-"):
			return launchBatch
		case p.Entrypoint != "cli":
			return launchIDE
		}
		return launchTerminal
	},
}
