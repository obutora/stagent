package install

import (
	"path/filepath"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// pi: an extension of stagent's in Pi's agent directory
// ($PI_CODING_AGENT_DIR or ~/.pi/agent)/extensions that runs `stagent hook
// pi` itself, like omp's (omp is a fork of Pi).

type piIntegration struct{}

func (piIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.piTarget())
	if c != nil && e.findHarness(wire.HarnessPi) != "" {
		e.note(notePiRestart, "Pi loads extensions at startup; restart running pi sessions (or run /reload) to activate the extension.")
	}
	return []*Change{c}, err
}

// targets include the extension where it was written under an earlier
// PI_CODING_AGENT_DIR.
func (piIntegration) targets(e *env) []*target {
	return e.withRecorded(e.piTarget(), e.piTargetAt)
}

func (piIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessPi, e.piTarget())
	return h
}

func (e *env) piTarget() *target {
	return e.piTargetAt(filepath.Join(harness.PiAgentDir(e.l.Home, e.getenv), "extensions", "ssh-term-stagent.ts"))
}

func (e *env) piTargetAt(path string) *target {
	return e.extensionTarget(wire.HarnessPi, path, piExtensionSource)
}

// piExtensionSource is written to <Pi's agent dir>/extensions/ssh-term-stagent.ts.
// It forwards session events to `stagent hook pi` using Claude Code's event
// names. It never blocks Pi: the hook runs detached, errors are swallowed,
// and nothing happens when the binary is missing.
//
// It stays silent in a Pi that is not the user's session:
//   - Printing (-p, --print) or driven by a program (--mode json, --mode
//     rpc): told by Pi's arguments (piHeadlessSource), as Pi 1.3.0 reports
//     a UI (hasUI) in rpc mode.
//   - Without a UI: Pi runs its terminal UI only when its stdin and stdout
//     are terminals, so a pi started by Pi's own shell (the bash tool and
//     `!` commands both run detached with pipes) is one of these too.
//   - With PI_SESSION_ID set: Pi gives it to the commands of its bash tool
//     and never sets it in its own environment, so it marks a pi the agent
//     started, even one given a terminal (`script`, tmux).
//
// Pi sets PI_CODING_AGENT=true in its own environment before it loads
// extensions, so that marker cannot tell a nested Pi. Pi's process title
// hides its arguments from the process table, so the extension passes them
// (argv) for the hook to tell a batch run (--no-session, the subcommands).
var piExtensionSource = extension{
	id: wire.HarnessPi, name: "Pi",
	nested: piHeadlessSource + `
// Pi sets PI_SESSION_ID only for the commands its bash tool runs: a pi
// started there is the agent's, not the session the user runs. Neither is
// a pi that prints its answer or serves a program.
const nested = !!process.env.PI_SESSION_ID || headless(process.argv.slice(2));
`,
	local: `// The answer of the run that is settling (agent_end comes before).
let answer: string | undefined;
`,
	skip:    "ctx?.hasUI === false",
	payload: "    argv: process.argv.slice(2),\n",
	handlers: `  // Every start: startup, reload, /new, /resume, /fork (event.reason).
  pi.on("session_start", (_event, ctx) => send("SessionStart", ctx));
  // A run of a prompt starts. Pi's input event comes earlier, also for
  // input that never starts a run (another extension handles it, no model
  // is set), which would leave the session working with no turn to end.
  pi.on("before_agent_start", (event, ctx) => {
    if (event?.messageType === "custom") return;
    send("UserPromptSubmit", ctx, text(event?.prompt));
  });
  pi.on("agent_end", (event) => {
    answer = assistantText(event?.messages);
  });
  // The run is over: no retry, compaction or queued message follows.
  pi.on("agent_settled", (_event, ctx) => {
    const message = answer;
    answer = undefined;
    send("Stop", ctx, message);
  });
  pi.on("session_shutdown", (_event, ctx) => send("SessionEnd", ctx));
`,
}.source()

// piHeadlessSource is the extension's test of Pi's arguments (process.argv
// after the script) for a run without the terminal UI: -p / --print, or
// --mode json / rpc (also written --mode=rpc). Plain JavaScript, so that
// tests run it with node.
const piHeadlessSource = `function headless(argv) {
  for (let i = 0; i < argv.length; i += 1) {
    const a = argv[i];
    if (a === "-p" || a === "--print") return true;
    const mode = a === "--mode" ? argv[i + 1] : a.startsWith("--mode=") ? a.slice("--mode=".length) : undefined;
    if (mode === "json" || mode === "rpc") return true;
  }
  return false;
}
`
