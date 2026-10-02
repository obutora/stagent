// Command stagent is the server-side half of SSH Term's agent features: the
// terminal chat view's transcript follower, and Agent Mode's PTY holders,
// daemon, app bridge, harness hooks and its own installer. See PROTOCOL.md
// and docs/agent-bridge-plan.md.
package main

import (
	"fmt"
	"os"
	"runtime"

	"github.com/obutora/stagent/internal/attachcli"
	"github.com/obutora/stagent/internal/bridge"
	"github.com/obutora/stagent/internal/daemon"
	"github.com/obutora/stagent/internal/follow"
	"github.com/obutora/stagent/internal/holder"
	"github.com/obutora/stagent/internal/hook"
	"github.com/obutora/stagent/internal/install"
	"github.com/obutora/stagent/internal/version"
)

const usage = `usage: stagent <command> [args]

  run [--detached | --handoff] [--cols N --rows N] [--cwd DIR] -- <cmd> [args...]
                      run a program under a PTY holder (agent session);
                      --handoff keeps it running detached when this terminal closes
  ls [--json]         list sessions, most recently active first
  attach [ID | --last] [--detach-key ctrl-]]
                      attach this terminal to a session (Ctrl-] detaches)
  daemon              run the session registry / event daemon (auto-started)
  bridge              speak the app protocol on stdin/stdout
  follow [--session ID]
                      stream the transcript of the coding agent running in
                      this SSH connection's terminal, or of stagent session ID
                      (JSON lines on stdout)
  hook <harness> [event]
                      entry point for harness hooks (reads the payload on stdin)
  install             create the layout and manifest (after the binary is placed)
  integrate           plan or apply harness hooks, shell wrappers, login service
  uninstall           stop / unhook / purge
  doctor              report installation state as JSON
  version             print the version
`

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return holder.Main(rest)
	case "ls":
		return attachcli.Ls(rest)
	case "attach":
		return attachcli.Attach(rest)
	case "daemon":
		return daemon.Main(rest)
	case "bridge":
		return bridge.Main(rest)
	case "follow":
		return follow.Main(rest)
	case "hook":
		return hook.Main(rest)
	case "install", "integrate", "uninstall", "doctor":
		return install.Main(cmd, rest)
	case "__env": // internal: environment probe run through the login shell
		return bridge.EnvDump()
	case "spawn-task":
		return spawnTask(rest)
	case "version", "--version", "-v":
		fmt.Printf("stagent %s (protocol %d, %s/%s)\n", version.Version, version.Protocol, runtime.GOOS, runtime.GOARCH)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "stagent: unknown command %q\n\n%s", cmd, usage)
	return 2
}
