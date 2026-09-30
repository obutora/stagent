# stagent protocol (version 1)

The app talks to `stagent bridge` over the stdin/stdout of an SSH exec
channel. Go types live in `internal/wire` (`methods.go`, `types.go`); this
document is the normative description for the app side.

## Framing

- One JSON object per line (`\n`; a trailing `\r` is tolerated). Max 16 MiB.
- Server output is pure ASCII: non-ASCII characters are `\uXXXX`-escaped.
- `[]byte` fields (`data`) are standard base64 with padding.
- Times are unix milliseconds. Ids of sessions are 16 lowercase hex chars.

Envelope (JSON-RPC 2.0 shape without the `jsonrpc` member):

| kind | members |
|---|---|
| request | `id` (int), `method`, `params?` |
| response | `id`, `result` **or** `error: {code, message}` |
| notification | `method`, `params?` (no `id`) |

Error codes: `bad_request`, `unknown_method`, `not_found`, `unsupported`,
`internal`, `unavailable`, `version_mismatch`, `session_ended`,
`not_size_owner`, `approval_closed`.

Requests on one connection are processed in order (input keystrokes keep
their order). Notifications may arrive at any time between responses.

## Starting the bridge

| host OS | exec command |
|---|---|
| Linux / macOS | `'<home>/.ssh-term/agent/bin/stagent' bridge` (single-quoted absolute path, `'` escaped as `'\''`) |
| Windows, sshd DefaultShell = cmd | `"<home>\.ssh-term\agent\bin\stagent.exe" bridge` |
| Windows, sshd DefaultShell = PowerShell | `& '<home>\.ssh-term\agent\bin\stagent.exe' bridge` |

The first request must be `hello`. If `result.protocol` differs from the
app's protocol, the app offers an update of the binary.

## Methods

### Bridge

| method | params | result |
|---|---|---|
| `hello` | `{protocol, client}` | `{protocol, version, os, arch, home, capabilities[]}` |
| `ping` | – | `{}` |
| `session.spawn` | `{command[], cwd?, cols, rows, env?{}}` | `{session}` — a detached session; app owns its size |

`session.spawn` starts the command with the environment of the user's login
shell (`$SHELL -l -i`, then `$SHELL -l`, captured once per bridge), not the
narrower one of the SSH exec channel, so agents on a PATH set in
`~/.bash_profile` / `~/.zprofile` / `~/.zshrc` are found. A bare command
name still missing from that PATH is looked up in the usual per-user tool
directories (`~/.bun/bin`, `~/.local/bin`, npm/pnpm/volta/nvm/fnm/mise,
Homebrew). `env` entries override both.

Capabilities: `screen_mode`, `spawn`, `hooks`, `transcript`, `push`.

### Session (forwarded to the session's holder)

| method | params | result |
|---|---|---|
| `session.info` | `{id}` | `Session` |
| `session.attach` | `{id, mode: "raw"\|"screen", fps?}` | `{cols, rows, mode}` then `output` / `resize` / `closed` notifications |
| `session.detach` | `{id}` | `{}` |
| `session.input` | `{id, text?, paste?, keys?[], submit?}` | `{}` |
| `session.resize` | `{id, cols, rows, force?}` | `{}`; `not_size_owner` for a passthrough session without `force` |
| `session.scrollback` | `{id, before?, max_bytes?}` | `{data, start, end, first}` |
| `session.signal` | `{id, signal: "interrupt"\|"terminate"\|"kill"}` | `{}` |

Attach modes:

- `raw`: PTY bytes as produced. The first `output` has `reset: true` and
  carries an ANSI snapshot of the current screen. If the client falls behind
  (its queue exceeds 256 KiB) queued bytes are dropped and the next `output`
  is again a `reset: true` snapshot.
- `screen`: at most `fps` (default 15, max 30) `output` notifications per
  second, each an ANSI sequence redrawing only the rows that changed since
  the previous frame (plus cursor position). Intermediate states are
  skipped; a slow client only ever gets the latest screen. Scrollback is not
  streamed in this mode; use `session.scrollback`.

On `reset: true` the client clears screen and scrollback before writing
`data`. `resize {id, cols, rows}` arrives when the session size changes;
the client resizes its terminal to exactly that size. `closed {id,
exit_code}` ends the stream.

`session.input` is applied in order: `text` raw, `paste` (wrapped in
`ESC[200~ … ESC[201~` when the program enabled bracketed paste), `keys`,
then `\r` if `submit`. Key names: `enter esc tab shift-tab backspace delete
space up down right left home end pageup pagedown ctrl-c ctrl-d ctrl-z
ctrl-l ctrl-r ctrl-u ctrl-o ctrl-t`.

Input details: newlines inside `paste` are sent as `\r` (what a terminal
sends for Enter) and any `ESC[201~` inside it is removed; `up down right left
home end` use the SS3 form (`ESC O A` …) while the program has enabled
application cursor keys (DECCKM); a `submit` that follows `text`/`paste` is
written 30 ms later as a separate write so TUIs that debounce pasted input
see Enter as a keystroke.

`session.scrollback`: raw PTY bytes `[start, end)` of the session's output
stream, newest first page when `before` is 0; `first` is the oldest retained
offset (`start == first` → beginning reached). `max_bytes` default 256 KiB,
max 1 MiB.

### Daemon (forwarded to the daemon)

| method | params | result |
|---|---|---|
| `watch` | `{since}` | `{sessions[], approvals[], seq, missed[], truncated}` then `event` / `session.updated` / `session.removed` notifications |
| `sessions.list` | – | `{sessions[]}` |
| `conversations.list` | `{harness?[], limit?}` | `{conversations[]}` |
| `transcript.get` | `{session_id? \| harness+conversation_id? \| path?, limit?, before?}` | `{path, harness, messages[], cursor}` |
| `transcript.subscribe` | `{session_id? \| path?}` | `{}` then `transcript` notifications |
| `transcript.unsubscribe` | same | `{}` |
| `approvals.list` | – | `{approvals[]}` |
| `approval.respond` | `{request_id, decision: "allow"\|"deny", message?}` | `{}`; `approval_closed` if it already timed out |
| `config.get` | – | `{config}` |
| `config.set` | `{config}` | `{config}` (with defaults applied) |
| `notify.test` | – | `{}`; emits a `notification` event (reason `test`) and pushes to enabled channels |

`watch {since}` returns the events after `since` in `missed` (oldest first)
and `truncated: true` when retention already dropped some of them. Store
the highest `seq` seen and pass it on the next connection.

If the daemon restarts while the bridge is connected, daemon requests in
flight fail with `unavailable` and the bridge restores the watch itself:
events the app missed arrive as `event`, every current session as
`session.updated`, and sessions that disappeared as `session.removed`. The
app needs no daemon-restart handling. Session requests go to the holders
directly and are unaffected.

### Notifications

| method | params |
|---|---|
| `event` | `Event` |
| `session.updated` | `Session` (full object; replaces the previous one) |
| `session.removed` | `{id}` |
| `output` | `{id, data, reset?}` |
| `resize` | `{id, cols, rows}` |
| `closed` | `{id, exit_code}` |
| `transcript` | `{session_id?, path, messages[]}` |

## Objects

`Session`: `id, harness (claude|codex|omp|other), command[], cwd, pid,
holder_pid, mode (passthrough|detached), state (working|idle|waiting_input|
needs_approval|exited), state_source (hook|terminal|activity|process),
title?, conversation_id?, transcript_path?, last_message?, cols, rows,
started_at, last_activity_at, exit_code?`.

`Event`: `seq, time, session_id?, kind, data`. Kinds and `data`:

| kind | data |
|---|---|
| `session_started` | `{harness, command[], cwd, mode}` |
| `session_ended` | `{exit_code}` |
| `state_changed` | `{from, to, source}` |
| `notification` | `{title, body, level (info\|warn), reason, count?}` — reason `waiting_input\|needs_approval\|turn_complete\|exited\|terminal\|test\|digest` |
| `approval_requested` | `Approval` |
| `approval_resolved` | `{request_id, decision, by (app\|timeout\|cancelled)}` |

`Approval`: `request_id, session_id?, harness, tool_name?, summary,
created_at, expires_at`.

`Message`: `role (user|assistant|tool|system), text, tool_name?, time?`.

`Conversation`: `harness, id, cwd, title, updated_at, path,
live_session_id?`.

`Config`: see `wire.Config` — `notify {ntfy {enabled, server, topic,
token?}, webhook {enabled, url, headers?}, debounce_ms, digest_window_ms},
retention {events_days, events_max, scrollback_days, scrollback_total_mib,
scrollback_session_mib}, approval_timeout_sec, idle_after_ms`.

## `stagent follow` (terminal chat view, follow protocol 1)

`stagent follow` streams the transcript of the coding agent (Claude Code,
Codex, omp) running in a terminal tab. The app runs it over an exec channel
**on the same SSH connection as the tab's shell**, with the command forms of
"Starting the bridge" (`follow` instead of `bridge`). It needs no daemon,
hooks or installation beyond the binary, and exits when stdin reaches EOF.

Framing as above (one JSON object per line, ASCII-only output, `\r`
tolerated on input), but frames are plain objects named by `t`, not
JSON-RPC envelopes. Times are unix milliseconds.

### Server → app

| frame | members |
|---|---|
| `hello` | `{t, version, follow_protocol, os, arch}` — first frame; `follow_protocol` is 1 |
| `target` | `{t, mux, mux_detail?, reason?, diag?, agents[], selected}` |
| `messages` | `{t, key, path, harness, reset, messages[], cursor}` |
| `page` | `{t, key, messages[], cursor}` — reply to `older` |
| `error` | `{t, message}` — non-fatal (bad request, failing multiplexer query, unreadable transcript) |

`target` is sent after `hello` and then whenever any of its members
changes (`diag` excepted: its counts move with every scan). `mux` is `none
| tmux | zellij | screen | herdr`: the multiplexer whose client the tab
shows. `mux_detail` names the session and the shown pane: tmux `"<session>
<pane id>"` (`main %3`), zellij `"<session> terminal_<n>"`, screen `"<sty>
<window>"`, herdr `"<session|default> <pane id>"`. `reason` is `no_terminal`
(the tab's processes cannot be found), `no_agent` (found, without an
agent), `terminal_ambiguous` (several terminals may be the tab's — see
below — so the agents of all of them are listed, each with its `tty`, and
the automatic choice is a guess) or `mux_ambiguous` (the multiplexer cannot
tell which of several clients is the tab's, so the agents of every pane its
clients show are listed); the first that applies, in this order.
`selected` is the followed agent's key or `null`.

`diag` comes with `no_terminal`: one line, for bug reports, of how the tab
was looked for — stagent's pid and uid, whether its `SSH_CONNECTION` is
set, the connection root it found or what the Tailscale session search and
the `SSH_CONNECTION` fallback counted, and its ancestors (`pid:name:uid`,
`?` for an unknown owner, nearest first, then `|top`, `|gone:<ppid>` for a
parent missing from the process table, `|newer:<ppid>` for a parent that
started after its child, or `|more`). It names processes and users, never
environment values or addresses. Its format is not stable: apps show it,
they do not parse it.

`Agent`: `key` (`<harness>:<pid>`), `harness`, `pid`, `cwd`, `pane?` (tmux
pane id, `terminal_<n>`, screen window number, herdr pane id), `tty?` (the
terminal of the agent's session below `/dev`, `ttys003`, `pts/3`; with
`terminal_ambiguous`), `title?` (the conversation's title),
`transcript_path?` (absent until the agent has written its transcript),
`foreground` (in the foreground of the terminal the user sees; always false
on Windows, which has no terminal process groups).

`messages` with `reset: true` replaces the list: it follows every change of
the selected agent or of its transcript path (`/clear`, resume, another
pane) and carries the newest 60 messages, oldest first. No `messages` frame
is sent while the selected agent has no transcript. `reset: false` appends
records written since. `cursor` is the byte offset to pass to `older` (0 =
the beginning was reached); appended frames repeat the oldest cursor handed
out so far. `Message` is the object of the Agent Mode protocol.

### App → server

| op | members | effect |
|---|---|---|
| `select` | `{op, key}` | follow this agent until its process exits, even when the tab stops showing it; `key` `null` or `""` returns to the automatic choice |
| `older` | `{op, before}` | `page` with the 60 messages before byte offset `before` (the last `cursor`) |

### How the tab's agent is found

Every channel of an SSH connection is a child of the connection's SSH
server process, so stagent walks up from itself to the nearest `sshd`,
`sshd-session` or `dropbear` (`.exe` on Windows) and takes that process's
other children — the tab's shell — as the tab.

Tailscale SSH has no such process: tailscaled runs every channel of every
connection below itself, through root's `login` (macOS; terminals on
Linux), `su -l` (commands on Linux) or its incubator `tailscaled be-child
ssh` (under the daemon's name, so of adjacent `tailscaled` ancestors the
topmost is the daemon). Below stagent's tailscaled, the terminals that may
be the tab are the topmost processes of stagent's user in each other
session that have a controlling terminal — reached only through root's
processes, never through another user's, and never below a multiplexer.
They are narrowed by what is known of each connection, strongest first:

1. the same `SSH_CONNECTION`, when stagent's (its own environment, else
   its ancestors' below tailscaled) and the terminal's (the environment of
   its processes of stagent's user, the shell first, else of root's
   processes that started it) can both be read;
2. the same client address: stagent's from `SSH_CONNECTION` / `SSH_CLIENT`,
   else its ancestors' command lines (`login … -h IP`, `tailscaled be-child
   ssh … --remote-ip=IP`); the terminal's from the same sources, else, on
   macOS, the host utmpx records for the terminal (`login -h` writes it;
   `/var/run/utmpx` is readable by everyone);
3. terminals whose connection is unknown.

A terminal proven to be on another connection or address is dropped. One
left is the tab; several give `terminal_ambiguous`, and the automatic
choice takes the most recently started session first. Environments often
cannot be read: macOS withholds the environment of restricted programs —
`/bin/zsh`, `/bin/sh`, `/usr/bin/*`, programs signed with entitlements —
from other processes while System Integrity Protection is on, root
included; `su -l` clears `SSH_CONNECTION` from Linux's exec channels;
command lines of root's processes are readable to everyone on Linux, to
root only on macOS. So stagent running as the user tells two tabs from the
same device apart only by `SSH_CONNECTION` (readable on Linux, and on macOS
where an agent's environment is), and on Linux without a client address of
its own it cannot tell terminals apart at all.

When neither applies, or tailscaled shows no terminal session, the
processes of stagent's user whose environment has stagent's
`SSH_CONNECTION` stand in for the tab (Linux and macOS read other
processes' environments, macOS not those it withholds; Windows too for the
user's own processes). When stagent's own `SSH_CONNECTION` is unset there
is then no tab to find: the target is `no_terminal`.

In the tab's process tree, agents are recognized by process name or argv
(`claude`, `codex`, `omp`, also `node|bun … <script>` of their npm
packages); a process of the same harness below an agent belongs to it
(Codex's native binary under its node launcher). A multiplexer client there
is followed into the pane it shows:

| client | shown pane |
|---|---|
| tmux | `tmux [-L/-S of the client] list-clients -F '#{client_pid}\t#{pane_pid}\t#{pane_id}\t#{session_name}'`, the row of the client's pid |
| zellij | session from `attach NAME` / `-s NAME` (else the server the client started, else the only server); `zellij --session NAME action list-clients`; the pane's processes are the server's children with `ZELLIJ_PANE_ID=<n>` |
| screen | `screen -S <sty> -Q number`; the window's processes are the server's children with `WINDOW=<n>` (every window, `mux_ambiguous`, when `-Q` is unsupported) |
| herdr | `herdr [--session NAME] pane list` (the `focused` pane), then `pane process-info --pane ID` (`shell_pid`, `foreground_processes`) |

The CLIs run as the client's own executable with the client's environment,
minus the dynamic loader's variables (see Security below).
Multiplexers are Linux / macOS only; nested ones are followed up to three
levels.

The automatic choice prefers, with `terminal_ambiguous`, an agent of the
most recently started session, then an agent in the foreground (its
process group is the terminal's foreground group; herdr also reports it),
then the newest transcript; it only moves when its agent is gone or another
agent comes to the foreground while it is not. The tree is walked every
second and the followed transcript polled every 300 ms.

Transcripts: Claude Code — `${CLAUDE_CONFIG_DIR:-~/.claude}/sessions/<pid>.json`
(`sessionId`) → `projects/*/<sessionId>.jsonl`. Codex and omp, first match
wins:

1. the rollout / session file the agent has open (Linux; both keep it open
   once they have written to it);
2. the session its command line names: `codex [exec] resume <uuid>` →
   `sessions/**/rollout-*-<uuid>.jsonl`; `omp -r/--resume <id prefix|path>`
   → the newest `<ts>_<id>.jsonl` whose id starts with the prefix, or the
   file; `omp -c/--continue` → the newest session of the agent's directory
   created before the agent started (`--session-dir` honoured);
3. the harness's newest conversation in the agent's working directory
   written since the agent started that no other running Codex / omp
   process has open or names on its command line, and no other listed
   agent already follows. An agent that starts a new conversation (no
   resume / continue) prefers one created since it started.

`CLAUDE_CONFIG_DIR` and `CODEX_HOME` are read from the agent's environment.

### Security

`stagent follow` only uses processes of the user it runs as: the same
effective uid, with the real uid equal to it (a set-user-ID program has its
owner's privileges but the arguments and environment of whoever started it,
so it counts as another user's), or on Windows the same token user SID. A
process whose owner cannot be read counts as another user's; the owner is
checked on every scan. Another user's process

- never stands in for the tab in the `SSH_CONNECTION` fallback (anyone can
  copy the connection's `SSH_CONNECTION` into their environment), nor is
  one of the terminals below tailscaled, nor is looked through for one,
- is never an agent, a multiplexer client or a multiplexer server, so its
  executable is never run and its environment never passed on,
- is never asked which transcript it writes: its command line, environment,
  working directory and open files are not used.

Processes below it that run as stagent's user again (`su bob`, then `su -`
back) still belong to the tab. This matters most when stagent runs as root
(Tailscale SSH as root, `su bob` in root's tab): running another user's
multiplexer binary, or any binary with their environment, would run their
code as root. Root's own processes that start Tailscale SSH sessions
(`login`, `su`, the incubator) are only read — command line, and
environment when readable — to compare connections.

Whichever environment a multiplexer CLI gets (the client's, or stagent's own
when the client's cannot be read), the dynamic loader's variables (`LD_*`,
`DYLD_*`) are removed from it: they would load code into the CLI.

## Installer CLI (run by the app over plain exec, not the bridge)

All commands print one JSON document on stdout with `--json` and exit 0 on
success (non-zero with `{"error": "..."}` on failure).

| command | output |
|---|---|
| `stagent install --json` | `{ok, version, layout{root, bin, run_dir, data_dir, data_on_network_fs}}` — creates directories and the manifest; idempotent |
| `stagent doctor --json` | `DoctorReport` |
| `stagent integrate --json --plan\|--apply [--harness claude,codex,omp] [--shell-wrapper] [--service] [--remove claude,codex,omp,shell-wrapper,service]` | `{applied, changes[{id, target, action (create\|modify\|delete), summary, diff}], notes[]}` |
| `stagent uninstall --json --level stop\|unhook\|purge [--uploads]` | `{level, steps[{action, target, ok, error?}], removed[], failed[{path, reason, sessions[]}], remaining[]}` |

Levels are cumulative: `unhook` includes `stop`, `purge` includes `unhook`.

`DoctorReport`:

```json
{
  "version": "0.1.0", "protocol": 1, "os": "linux", "arch": "amd64", "home": "/home/u",
  "layout": {"root": "...", "bin": "...", "run_dir": "...", "data_dir": "...", "data_on_network_fs": false},
  "daemon": {"running": true, "pid": 123, "version": "0.1.0", "sessions": 2},
  "harnesses": [
    {"id": "claude", "found": true, "path": "/usr/bin/claude", "version": "2.1.284",
     "config_path": "/home/u/.claude/settings.json", "integrated": true,
     "hooks_supported": true, "notes": []}
  ],
  "shell_wrapper": {"installed": false, "files": []},
  "service": {"kind": "systemd|launchd|schtasks|none", "installed": false, "running": false},
  "orphans": [{"target": "/home/u/.claude/settings.json", "detail": "..."}],
  "problems": []
}
```

## Harness hooks

Installed hook commands call `stagent hook <harness>` with the harness's
hook payload on stdin (`hook_event_name` inside the payload names the
event). Codex's `notify` program is `stagent hook codex notify` and gets the
JSON as its last argument. The omp extension runs `stagent hook omp` with
`{hook_event_name, session_id, transcript_path, cwd, message?}` on stdin,
using Claude's event names (`SessionStart`, `UserPromptSubmit`, `Stop`,
`SessionEnd`). A hook never fails the harness: when the daemon is
unreachable it exits 0 without output.
