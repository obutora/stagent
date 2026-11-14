package install

import (
	"strconv"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// omp: an extension of stagent's in ~/.omp/agent/extensions that runs
// `stagent hook omp` itself.

type ompIntegration struct{}

func (ompIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.ompTarget())
	if c != nil && e.findHarness(wire.HarnessOmp) != "" {
		e.note(noteOmpRestart, "omp loads extensions at startup; restart running omp sessions to activate the extension.")
	}
	return []*Change{c}, err
}

func (ompIntegration) targets(e *env) []*target { return []*target{e.ompTarget()} }

func (ompIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessOmp, e.ompTarget())
	return h
}

func (e *env) ompExtension() string {
	return e.home(".omp", "agent", "extensions", "ssh-term-stagent.ts")
}

func (e *env) ompTarget() *target {
	return e.extensionTarget(wire.HarnessOmp, e.ompExtension(), ompExtensionSource)
}

// ompExtensionSource is written to ~/.omp/agent/extensions/ssh-term-stagent.ts.
// It forwards session events to `stagent hook omp` using Claude Code's event
// names, and its question tool's (harness.Harness.QuestionTool: ask) call
// and result as PreToolUse / PostToolUse (a question to the user and its
// end). It never blocks omp: the hook runs detached, errors are swallowed,
// and nothing happens when the binary is missing or omp is nested.
var ompExtensionSource = extension{
	id: wire.HarnessOmp, name: "omp",
	nested: `// omp marks every shell it spawns with OMPCODE=1; a nested omp started from
// such a shell is not the session the user runs, so it stays silent.
const nested = process.env.OMPCODE === "1";
`,
	local: `function isSubSession(ctx: any): boolean {
  if (ctx?.hasUI === false || ctx?.agentKind === "sub") return true;
  try {
    const header = ctx?.sessionManager?.getHeader?.();
    return typeof header?.parentSession === "string";
  } catch {
    return false;
  }
}
`,
	skip: "isSubSession(ctx)",
	handlers: `  pi.on("session_start", (_event, ctx) => send("SessionStart", ctx));
  pi.on("session_switch", (_event, ctx) => send("SessionStart", ctx));
  pi.on("before_agent_start", (event, ctx) => send("UserPromptSubmit", ctx, text(event?.prompt)));
  pi.on("agent_end", (event, ctx) => {
    // A continuation is already scheduled: not the end of the turn.
    if (event?.willContinue === true) return;
    send("Stop", ctx, assistantText(event?.messages));
  });
  pi.on("session_shutdown", (_event, ctx) => send("SessionEnd", ctx));
  // The question tool asks the user; its result comes once answered (or
  // cancelled, or timed out).
  const questionTool = ` + strconv.Quote(harness.QuestionToolOf(wire.HarnessOmp)) + `;
  pi.on("tool_call", (event, ctx) => {
    if (event?.toolName === questionTool) send("PreToolUse", ctx, undefined, { name: questionTool, input: event.input });
  });
  pi.on("tool_result", (event, ctx) => {
    if (event?.toolName === questionTool) send("PostToolUse", ctx, undefined, { name: questionTool, input: event.input });
  });
`,
}.source()
