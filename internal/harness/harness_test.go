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
		{"node", []string{"node", "server.js"}, ""},
		{"vim", []string{"vim", "claude"}, ""},
		{"bash", []string{"bash", "/usr/local/bin/claude"}, ""},
	} {
		if got := Of(c.name, c.argv); got != c.want {
			t.Errorf("Of(%q, %q) = %q, want %q", c.name, c.argv, got, c.want)
		}
	}
}
