package install

import "bytes"

// ompMarker is the first line of the omp extension; it identifies the file
// as ours.
const ompMarker = "// installed by ssh-term (stagent integrate); managed file"

func isOmpExtension(b []byte) bool {
	return bytes.HasPrefix(b, []byte(ompMarker))
}

// ompExtensionSource is written to ~/.omp/agent/extensions/ssh-term-stagent.ts.
// It forwards session events to `stagent hook omp` using Claude Code's event
// names. It never blocks omp: the hook runs detached, errors are swallowed,
// and nothing happens when the binary is missing or omp is nested.
const ompExtensionSource = ompMarker + `
// SSH Term Agent Mode: reports omp session events to the local stagent daemon.
// Removed by ` + "`stagent uninstall --level unhook`" + ` (or ` + "`stagent integrate --remove omp`" + `).
// @ts-nocheck

import { spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";

// omp marks every shell it spawns with OMPCODE=1; a nested omp started from
// such a shell is not the session the user runs, so it stays silent.
const nested = process.env.OMPCODE === "1";

function stagentBin(): string | undefined {
  const home = process.env.HOME || process.env.USERPROFILE;
  if (!home) return undefined;
  const exe = process.platform === "win32" ? "stagent.exe" : "stagent";
  const bin = path.join(home, ".ssh-term", "agent", "bin", exe);
  try {
    return fs.existsSync(bin) ? bin : undefined;
  } catch {
    return undefined;
  }
}

function text(v: unknown, max = 2000): string | undefined {
  if (typeof v !== "string" || v.length === 0) return undefined;
  return v.length > max ? v.slice(0, max) : v;
}

function assistantText(messages: unknown): string | undefined {
  if (!Array.isArray(messages)) return undefined;
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i] as any;
    if (m?.role !== "assistant") continue;
    if (typeof m.content === "string") return text(m.content);
    if (!Array.isArray(m.content)) return undefined;
    let out = "";
    for (const part of m.content) {
      if (part?.type === "text" && typeof part.text === "string") out += part.text;
    }
    return text(out);
  }
  return undefined;
}

function isSubSession(ctx: any): boolean {
  if (ctx?.hasUI === false || ctx?.agentKind === "sub") return true;
  try {
    const header = ctx?.sessionManager?.getHeader?.();
    return typeof header?.parentSession === "string";
  } catch {
    return false;
  }
}

// The last session seen; session_shutdown may arrive without a context.
const last = { sessionId: "", transcriptPath: "", cwd: "" };

function remember(ctx: any): void {
  try {
    const id = ctx?.sessionManager?.getSessionId?.();
    const file = ctx?.sessionManager?.getSessionFile?.();
    if (typeof id === "string" && id) last.sessionId = id;
    if (typeof file === "string" && file) last.transcriptPath = file;
  } catch {}
  if (typeof ctx?.cwd === "string" && ctx.cwd) last.cwd = ctx.cwd;
}

function send(event: string, ctx: any, message?: string): void {
  if (isSubSession(ctx)) return;
  remember(ctx);
  const bin = stagentBin();
  if (!bin) return;
  const payload: Record<string, unknown> = {
    hook_event_name: event,
    session_id: last.sessionId,
    transcript_path: last.transcriptPath,
    cwd: last.cwd || process.cwd(),
  };
  if (message) payload.message = message;
  try {
    const child = spawn(bin, ["hook", "omp"], {
      stdio: ["pipe", "ignore", "ignore"],
      detached: true,
      windowsHide: true,
    });
    child.on("error", () => {});
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify(payload));
    child.unref();
  } catch {}
}

export default function (pi) {
  if (nested) return;
  pi.on("session_start", (_event, ctx) => send("SessionStart", ctx));
  pi.on("session_switch", (_event, ctx) => send("SessionStart", ctx));
  pi.on("before_agent_start", (event, ctx) => send("UserPromptSubmit", ctx, text(event?.prompt)));
  pi.on("agent_end", (event, ctx) => {
    // A continuation is already scheduled: not the end of the turn.
    if (event?.willContinue === true) return;
    send("Stop", ctx, assistantText(event?.messages));
  });
  pi.on("session_shutdown", (_event, ctx) => send("SessionEnd", ctx));
}
`
