package harness

import "testing"

func TestOf(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"claude", []string{"claude"}, "claude"},
		{"2.1.285", []string{"claude", "--resume"}, "claude"},
		{"node", []string{"node", "--enable-source-maps", `C:\Users\u\AppData\Roaming\npm\node_modules\@anthropic-ai\claude-code\cli.js`}, "claude"},
		{"claude.exe", nil, "claude"},
		{"node", []string{"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js", "resume"}, "codex"},
		{"codex-x86_64-u", []string{"/x/bin/codex-x86_64-unknown-linux-musl"}, "codex"},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp"}, "omp"},
		{"bun", []string{"/home/u/.bun/bin/bun", "/home/u/.bun/install/global/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js"}, "omp"},
		// Pi sets its process title to "pi" (macOS keeps the name node and
		// shows argv ["pi", "", ...]; Linux renames the process), and runs
		// as node with its script until it does, or on Windows.
		{"node", []string{"pi", "", "", ""}, "pi"},
		{"pi", []string{"pi"}, "pi"},
		{"node", []string{"node", "/usr/lib/node_modules/@earendil-works/pi-coding-agent/dist/bundle/cli.js", "--session", "x"}, "pi"},
		{"node.exe", []string{`C:\Program Files\nodejs\node.exe`, `C:\Users\u\AppData\Roaming\npm\node_modules\@mariozechner\pi-coding-agent\dist\cli.js`}, "pi"},
		// Copilot CLI's npm package runs its native binary (a Node SEA,
		// also what brew, WinGet and the install script put on PATH) from
		// a node launcher; both are copilot.
		{"copilot", []string{"/usr/lib/node_modules/@github/copilot/node_modules/@github/copilot-linux-x64/copilot"}, "copilot"},
		{"node", []string{"node", "/usr/lib/node_modules/@github/copilot/npm-loader.js", "--resume=0193"}, "copilot"},
		{"node.exe", []string{`C:\Program Files\nodejs\node.exe`, `C:\Users\u\AppData\Roaming\npm\node_modules\@github\copilot\npm-loader.js`}, "copilot"},
		{"copilot.exe", nil, "copilot"},
		{"node", []string{"node", "server.js"}, ""},
		{"vim", []string{"vim", "claude"}, ""},
		{"bash", []string{"bash", "/usr/local/bin/claude"}, ""},
	} {
		if got := Of(c.name, c.argv); got != c.want {
			t.Errorf("Of(%q, %q) = %q, want %q", c.name, c.argv, got, c.want)
		}
	}
}
