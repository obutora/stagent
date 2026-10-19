package install

import (
	"bytes"
	"strings"
)

// The harnesses of the Pi family (omp, Pi) load a TypeScript extension of
// stagent's that runs `stagent hook <id>` itself. The extensions share
// their header, helpers and the way they send an event; each harness adds
// how it tells a session that is not the user's and which of its events it
// reports.

// extensionTarget is the extension file of stagent's at path that harness
// id (omp, Pi) loads, written as source.
func (e *env) extensionTarget(id, path, source string) *target {
	return &target{
		id: id, path: path, owned: true, mode: 0o644,
		identify: "whole file (first line " + extensionMarker + ")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !isOurExtension(cur) {
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(source), summary: "install the " + id + " extension that reports session events to stagent"}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !isOurExtension(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if isOurExtension(cur) {
				return "stagent " + id + " extension"
			}
			return ""
		},
	}
}

// extensionMarker is the first line of an extension of stagent's; it
// identifies the file as ours.
const extensionMarker = "// installed by ssh-term (stagent integrate); managed file"

func isOurExtension(b []byte) bool {
	return bytes.HasPrefix(b, []byte(extensionMarker))
}

// extension is the source of a harness's extension, composed by source.
type extension struct {
	// id is the harness id, the argument of `stagent hook`; name is how the
	// comments call the harness.
	id, name string
	// nested declares the constant nested: the harness runs as a session
	// that is not the user's.
	nested string
	// local declares what the harness's send and handlers use besides the
	// shared helpers.
	local string
	// skip is the expression of ctx that makes send report nothing.
	skip string
	// payload adds fields to the payload, one per line.
	payload string
	// handlers is the body of the default export: the handlers of the
	// harness's events.
	handlers string
}

// source is the extension's file: the marker, the header, nested, the
// shared helpers, local, send, and the default export.
func (x extension) source() string {
	var b strings.Builder
	b.WriteString(extensionMarker + "\n")
	b.WriteString("// SSH Term Agent Mode: reports " + x.name + " session events to the local stagent daemon.\n")
	b.WriteString("// Removed by `stagent uninstall --level unhook` (or `stagent integrate --remove " + x.id + "`).\n")
	b.WriteString("// @ts-nocheck\n\n")
	b.WriteString("import { spawn } from \"node:child_process\";\nimport fs from \"node:fs\";\nimport path from \"node:path\";\n\n")
	b.WriteString(x.nested + "\n")
	b.WriteString(extensionHelpers + "\n")
	if x.local != "" {
		b.WriteString(x.local + "\n")
	}
	b.WriteString(extensionRemember + "\n")
	b.WriteString("function send(event: string, ctx: any, message?: string): void {\n")
	b.WriteString("  if (" + x.skip + ") return;\n")
	b.WriteString(`  remember(ctx);
  const bin = stagentBin();
  if (!bin) return;
  const payload: Record<string, unknown> = {
    hook_event_name: event,
    session_id: last.sessionId,
    transcript_path: last.transcriptPath,
    cwd: last.cwd || process.cwd(),
`)
	b.WriteString(x.payload)
	b.WriteString(`  };
  if (message) payload.message = message;
  try {
    const child = spawn(bin, ["hook", "` + x.id + `"], {
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
`)
	b.WriteString(x.handlers)
	b.WriteString("}\n")
	return b.String()
}

// extensionHelpers finds stagent's binary (none when it is missing) and
// takes the text of a prompt and of the assistant's last message.
const extensionHelpers = `function stagentBin(): string | undefined {
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
`

// extensionRemember keeps the session of the last event, which send
// reports.
const extensionRemember = `// The last session seen; session_shutdown may arrive without a context.
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
`
