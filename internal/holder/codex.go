package holder

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Codex 0.157 and later run the TUI's threads, and so their hooks, in one
// shared `codex app-server --managed-daemon` per CODEX_HOME. That server
// keeps the environment of the codex that started it, so the hooks of every
// later session carried the first session's STAGENT_SESSION_ID (#319).
// `--no-daemon` (Codex 0.156+) keeps the threads inside the session's own
// codex process, with its own environment.
const (
	codexNoDaemon       = "--no-daemon"
	codexNoDaemonMinor  = 156 // first 0.x release with --no-daemon
	codexVersionTimeout = 5 * time.Second
)

// codexCommand returns argv with --no-daemon after the program when argv
// runs a Codex CLI that has the flag — directly or in the end, after
// `stagent task run … --` (wire.AgentArgv). Invocations Codex refuses it
// with (`codex agents`, `--remote`), ones already giving it and a codex
// whose version cannot be read are left as they are. The version is asked
// the way pty.Start runs the program: same lookup, dir and env.
func codexCommand(argv []string, dir string, env []string) []string {
	agent := wire.AgentArgv(argv)
	if wire.DetectHarness(agent) != wire.HarnessCodex {
		return argv
	}
	for _, a := range agent[1:] {
		if a == codexNoDaemon || a == "agents" || a == "--remote" || strings.HasPrefix(a, "--remote=") {
			return argv
		}
	}
	if !codexHasNoDaemon(agent[0], dir, env) {
		return argv
	}
	pre := len(argv) - len(agent)
	out := make([]string, 0, len(argv)+1)
	out = append(out, argv[:pre+1]...)
	out = append(out, codexNoDaemon)
	return append(out, agent[1:]...)
}

func codexHasNoDaemon(bin, dir string, env []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), codexVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Dir, cmd.Env = dir, env
	// npm's codex is a node launcher; a native child left holding stdout
	// must not keep Output waiting after the launcher is killed.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	major, minor, ok := parseCodexVersion(string(out))
	return ok && (major > 0 || minor >= codexNoDaemonMinor)
}

// parseCodexVersion finds the "codex-cli 0.160.0" line of `codex --version`.
func parseCodexVersion(out string) (major, minor int, ok bool) {
	for line := range strings.Lines(out) {
		f := strings.Fields(line)
		if len(f) != 2 || f[0] != "codex-cli" {
			continue
		}
		parts := strings.SplitN(f[1], ".", 3)
		if len(parts) < 2 {
			return 0, 0, false
		}
		major, err1 := strconv.Atoi(parts[0])
		minor, err2 := strconv.Atoi(parts[1])
		return major, minor, err1 == nil && err2 == nil
	}
	return 0, 0, false
}
