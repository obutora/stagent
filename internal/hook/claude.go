package hook

import "os"

// claudeHook: Claude Code tells how it was started in its environment
// (CLAUDE_CODE_ENTRYPOINT: cli, sdk-*, claude-vscode, …).
var claudeHook = hookHarness{
	launch: func(li *launchInfo, _ []byte) {
		li.entrypoint = os.Getenv("CLAUDE_CODE_ENTRYPOINT")
	},
}
