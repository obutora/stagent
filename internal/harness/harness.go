// Package harness recognizes the processes of the coding agents stagent
// knows (Claude Code, Codex, omp), and connections that come from their
// process trees (guard.go).
package harness

import (
	"strings"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// interpreters run agents distributed as scripts.
var interpreters = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

// packages are the npm packages of the agents, for launchers that run their
// script by path (Claude Code's cli.js).
var packages = []struct{ dir, harness string }{
	{"/@anthropic-ai/claude-code/", wire.HarnessClaude},
	{"/@openai/codex/", wire.HarnessCodex},
	{"/@oh-my-pi/pi-coding-agent/", wire.HarnessOmp},
}

// Interpreter reports whether n (a ptable.BaseName) is a script interpreter
// an agent may run under.
func Interpreter(n string) bool { return interpreters[n] }

// Of recognizes an agent by its process name, its argv[0] (native binaries,
// and scripts that set their process title) or the script an interpreter
// runs; "" for other processes.
func Of(name string, argv []string) string {
	if h := Name(ptable.BaseName(name)); h != "" {
		return h
	}
	if h := Name(ptable.Argv0(argv)); h != "" {
		return h
	}
	if !interpreters[ptable.Argv0(argv)] {
		return ""
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		script := strings.ReplaceAll(a, `\`, "/")
		base := ptable.BaseName(script)
		for _, ext := range []string{".js", ".mjs", ".cjs", ".ts"} {
			base = strings.TrimSuffix(base, ext)
		}
		if h := Name(base); h != "" {
			return h
		}
		for _, p := range packages {
			if strings.Contains(script, p.dir) {
				return p.harness
			}
		}
		return ""
	}
	return ""
}

// Name returns the harness a program called n (a ptable.BaseName) is, ""
// for other programs.
func Name(n string) string {
	switch n {
	case wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp:
		return n
	}
	// Codex's npm package has shipped its native binary as
	// codex-<target triple>.
	if strings.HasPrefix(n, "codex-x86_64-") || strings.HasPrefix(n, "codex-aarch64-") {
		return wire.HarnessCodex
	}
	return ""
}
