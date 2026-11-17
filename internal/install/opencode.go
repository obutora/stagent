package install

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// opencode: a plugin of stagent's (OpenCode V2's plugin form, `export
// default { id, setup }`) in OpenCode's global plugin directory,
// ${OPENCODE_CONFIG_DIR:-${XDG_CONFIG_HOME:-~/.config}/opencode}/plugins/
// ssh-term-stagent.js. OpenCode loads every file there, and reloads them
// when one comes or goes, also in running servers: no setting of the
// user's (opencode.json) is touched and no restart is needed. Installing
// writes the file, removing deletes it.
//
// The plugin turns OpenCode's events into Claude Code's hook events and
// runs `stagent hook opencode <event>` for each, one after the other so
// the daemon gets them in order, without OpenCode waiting on them (OpenCode
// waits on its own hooks, so the plugin hooks into none; it only reads the
// event stream). It also has the hook read the session again (the
// pseudo-event Transcript: `opencode session export`, which stagent keeps
// as the session's transcript, transcript/opencode.go) when a step of the
// conversation was written, at most one such read waiting per session.
//
// OpenCode runs plugins in its server: a terminal UI started with
// --standalone (stagent adds it: holder/opencode.go) runs one of its own,
// `opencode serve --stdio`, as its child, with its environment
// (STAGENT_SESSION_ID); the background service (`serve --service`) and
// `opencode serve` serve several terminals with the environment of
// whoever started them, so the plugin stays silent there. A server of
// `opencode run` looks the same as a terminal UI's: the hook tells it by
// the server's parent (hook/opencode.go).

type openCodeIntegration struct{}

func (openCodeIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.openCodeTarget())
	return []*Change{c}, err
}

// targets include the plugin where it was written under an earlier
// OPENCODE_CONFIG_DIR or XDG_CONFIG_HOME.
func (openCodeIntegration) targets(e *env) []*target {
	return e.withRecorded(e.openCodeTarget(), e.openCodeTargetAt)
}

func (openCodeIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessOpenCode, e.openCodeTarget())
	return h
}

// openCodeConfigDir is OpenCode's global configuration directory:
// $OPENCODE_CONFIG_DIR, else $XDG_CONFIG_HOME/opencode, else
// ~/.config/opencode (on every OS).
func (e *env) openCodeConfigDir() string {
	if dir := e.getenv("OPENCODE_CONFIG_DIR"); dir != "" {
		return dir
	}
	if dir := e.getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "opencode")
	}
	return filepath.Join(e.l.Home, ".config", "opencode")
}

func (e *env) openCodeTarget() *target {
	return e.openCodeTargetAt(filepath.Join(e.openCodeConfigDir(), "plugins", "ssh-term-stagent.js"))
}

func (e *env) openCodeTargetAt(path string) *target {
	return e.extensionTarget(wire.HarnessOpenCode, path, openCodePluginSource(e.l.Bin))
}

// openCodePluginSource is the plugin, running stagent's binary at bin.
func openCodePluginSource(bin string) string {
	quoted, _ := json.Marshal(bin)
	return strings.Replace(openCodePlugin, "@BIN@", string(quoted), 1)
}

// openCodePlugin is the plugin's source; @BIN@ stands for stagent's binary
// as a JavaScript string. Plain JavaScript (OpenCode loads .js plugins as
// modules), so that tests run it with node.
//
// Events → hooks (payloads in Claude Code's form, plus exec_path, the
// OpenCode binary the hook reads the session with):
//   - session.created → SessionStart (only at a session's first prompt);
//   - session.execution.started → UserPromptSubmit, with the text of the
//     user's message it was queued for (session.inbox.enqueued);
//   - permission.asked → PermissionRequest, the tool call it asks about
//     (session.tool.input.started names it, session.tool.called gives its
//     input; else the action and the resources it asks for);
//   - permission.replied → PermissionReplied (once, always or reject:
//     answered, here or anywhere);
//   - form.created of the question tool → PreToolUse of tool "question",
//     its fields as Claude Code's questions; its form.replied or any other
//     end of the form → PostToolUse;
//   - session.execution.succeeded / failed → Stop, with the last text the
//     assistant ended (session.text.ended);
//   - session.execution.interrupted (a permission turned down, here or on
//     the PC, or the turn stopped) → StopCancelled, as Grok Build's: the
//     user cut the turn short, no turn-complete notification;
//   - location.shutdown (the server stops) → SessionEnd.
const openCodePlugin = extensionMarker + `
// SSH Term Agent Mode: reports OpenCode session events to the local stagent daemon.
// Removed by ` + "`stagent uninstall --level unhook`" + ` (or ` + "`stagent integrate --remove opencode`" + `).

import { spawn } from "node:child_process";
import fs from "node:fs";

const BIN = @BIN@;

// Only the private server of one terminal UI (--standalone runs
// ` + "`serve --stdio`" + `) reports: the background service and ` + "`opencode serve`" + `
// serve terminals whose events cannot be told apart.
function ownServer(argv) {
  return argv[0] === "serve" && argv.includes("--stdio");
}

function text(v, max = 2000) {
  if (typeof v !== "string" || v.length === 0) return undefined;
  return v.length > max ? v.slice(0, max) : v;
}

// The hooks run one at a time, in the order of the events.
const queue = [];
let running = false;
const syncing = new Set();

function next() {
  if (running) return;
  const job = queue.shift();
  if (!job) return;
  running = true;
  if (job.event === "Transcript") syncing.delete(job.payload.session_id);
  let done = false;
  const finish = () => {
    if (done) return;
    done = true;
    clearTimeout(timer);
    running = false;
    next();
  };
  const timer = setTimeout(finish, 15000);
  try {
    const child = spawn(BIN, ["hook", "opencode", job.event], {
      stdio: ["pipe", "ignore", "ignore"],
      detached: true,
      windowsHide: true,
    });
    child.on("error", finish);
    child.on("exit", finish);
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(job.payload));
    child.unref();
  } catch {
    finish();
  }
}

const dirs = new Map();
let lastSession = "";

function send(event, sessionId, fields) {
  if (sessionId) lastSession = sessionId;
  const id = sessionId || lastSession;
  queue.push({
    event,
    payload: {
      hook_event_name: event,
      session_id: id,
      cwd: dirs.get(id) || process.cwd(),
      exec_path: process.execPath,
      ...fields,
    },
  });
  next();
}

// Read the session again once the hooks queued before it ran.
function sync(sessionId) {
  if (!sessionId || syncing.has(sessionId)) return;
  syncing.add(sessionId);
  send("Transcript", sessionId);
}

function questions(fields) {
  return (Array.isArray(fields) ? fields : []).map((f) => ({
    question: f?.description || f?.title || "",
    header: f?.title || "",
    multiSelect: f?.type === "array" || !!f?.multiple,
    options: (Array.isArray(f?.options) ? f.options : []).map((o) => ({ label: o?.label ?? o?.value ?? "" })),
  }));
}

export default {
  id: "ssh-term.stagent",
  async setup(ctx) {
    if (!fs.existsSync(BIN) || !ownServer(process.argv.slice(2))) return;
    const prompts = new Map();
    const answers = new Map();
    const tools = new Map();
    const forms = new Map();
    const controller = new AbortController();
    void (async () => {
      try {
        for await (const e of ctx.event.subscribe({ signal: controller.signal })) {
          const d = e?.data || {};
          const sid = d.sessionID || d.form?.sessionID || "";
          const dir = e?.location?.directory || d.location?.directory;
          if (sid && dir) dirs.set(sid, dir);
          switch (e?.type) {
            case "session.created":
              send("SessionStart", sid);
              break;
            case "session.inbox.enqueued":
              if (d.item?.type === "user") prompts.set(sid, text(d.item?.payload?.text));
              break;
            case "session.execution.started":
              send("UserPromptSubmit", sid, { prompt: prompts.get(sid) });
              prompts.delete(sid);
              sync(sid);
              break;
            case "session.tool.input.started":
              tools.set(d.id, { name: d.name });
              break;
            case "session.tool.called":
              tools.set(d.id, { ...tools.get(d.id), input: d.input });
              break;
            case "permission.asked": {
              const call = tools.get(d.source?.id) || {};
              send("PermissionRequest", sid, {
                tool_name: call.name || d.action,
                tool_input: call.input ?? { action: d.action, resources: d.resources },
              });
              break;
            }
            case "permission.replied":
              send("PermissionReplied", sid, { reply: d.reply });
              break;
            case "form.created":
              if (d.form?.metadata?.kind === "question") {
                forms.set(d.form.id, d.form.sessionID);
                send("PreToolUse", d.form.sessionID, { tool_name: "question", tool_input: { questions: questions(d.form.fields) } });
              }
              break;
            case "session.text.ended":
              answers.set(sid, text(d.text));
              break;
            case "session.step.ended":
            case "session.step.failed":
            case "session.renamed":
              sync(sid);
              break;
            case "session.execution.succeeded":
            case "session.execution.failed":
            case "session.execution.interrupted":
              send(e.type === "session.execution.interrupted" ? "StopCancelled" : "Stop", sid, { last_assistant_message: answers.get(sid) });
              answers.delete(sid);
              sync(sid);
              break;
            case "location.shutdown":
              send("SessionEnd", "");
              break;
            default:
              if (typeof e?.type === "string" && e.type.startsWith("form.") && forms.has(d.id)) {
                send("PostToolUse", forms.get(d.id), { tool_name: "question" });
                forms.delete(d.id);
              }
          }
        }
      } catch {}
    })();
    return () => controller.abort();
  },
};
`
