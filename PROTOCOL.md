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
`not_size_owner`, `not_configured`, `menu_open`, `foreign_owner`,
`agent_refused`.

`foreign_owner`: a request that needs the daemon or a session's holder
(any daemon or session method, `session.spawn`) found the stagent location
it would use — the run directory, a socket or a named pipe — owned by
another user (see `hello`'s `blocked`). Nothing was sent there. The
message names the location and its owner.

`agent_refused`: the local connection came from a process that a coding
agent (claude, codex, omp) started — stagent walks its parents — or, on
macOS, from a process in a sandbox (Seatbelt, which coding agents run
their commands in), or from one it could not check: it exited first, or
its parents were more than 256 deep or kept changing (ADR 0004). A
holder answers whatever such a connection asks first with it and closes
the connection; the daemon refuses `config.set`, `presence.set`,
`notify.test` and `daemon.shutdown`,
and serves the rest (`hook.event`, `holder.*`, reads). The daemon decides
when the connection is made. The message says why. Only Linux and macOS
check, and only without `STAGENT_HOME`. `stagent bridge`, started by
sshd, has no agent among its parents, so the app does not see this error
unless the bridge itself runs under an agent; such a bridge answers
`session.spawn` with it before starting a holder (which would refuse the
bridge and keep running out of its reach).

Requests on one connection are processed in order (input keystrokes keep
their order). Notifications may arrive at any time between responses.

## Starting the bridge

| host OS | exec command |
|---|---|
| Linux / macOS | `exec '<home>/.ssh-term/agent/bin/stagent' bridge` (single-quoted absolute path, `'` escaped as `'\''`) |
| Windows, sshd DefaultShell = cmd | `"<home>\.ssh-term\agent\bin\stagent.exe" bridge` |
| Windows, sshd DefaultShell = PowerShell | `& '<home>\.ssh-term\agent\bin\stagent.exe' bridge` |

The Linux / macOS line starts with `exec ` for WSL behind Windows' SSH
server (sshd DefaultShell = WSL's `bash.exe`): Win32-OpenSSH passes a
command starting with `'` to the outer bash unquoted, which drops every
argument after the path. That outer bash also expands `$` inside the
single quotes, so no argument contains one. Such a host is Linux to the
app (the uname probe runs before the PowerShell one whatever the banner
says), and its SFTP is never opened: Windows' `sftp-server.exe`, started
through WSL, does not answer.

The first request must be `hello`. If `result.protocol` differs from the
app's protocol, the app offers an update of the binary.

The bridge exits when stdin reaches EOF. On Windows it also exits when the
process that created its stdin pipe ends — Win32-OpenSSH's session process
(`sshd -z`) — since stdin may not reach EOF when that process is killed.

### Versions and compatibility

- The app pins one stagent release (`stagent_release.dart`); that version
  is the minimum for every feature of the app. A host whose `hello.version`
  is older is updated as a whole (the binary, then `stagent install`); the
  app never offers part of its features to an older host.
- `protocol` stays 1. Raising it would make every app already released
  treat a newer host as needing an install it cannot do (the host's
  version is newer than its pin), so those apps could no longer open it.
- While `protocol` is 1, stagent only adds keys and methods. Something may
  be removed only when no released app uses it; `approval.respond` and
  `config.set` replacing the whole config qualify.
- Capabilities are reserved for what depends on the host OS. Linux and
  macOS keep relying on `persist`; Windows needs `persist_shell` as well.
  Whether a key is present is checked only for `host_id`.

## Methods

### Bridge

| method | params | result |
|---|---|---|
| `hello` | `{protocol, client}` | `{protocol, version, os, arch, home, capabilities[], host_id?, blocked?}` |
| `ping` | – | `{}` |
| `session.spawn` | `{command[]?, shell?, cwd?, cols, rows, env?{}}` | `{session}` — a detached session; app owns its size |

`host_id` (32 lowercase hex chars) is 128 random bits stagent creates the
first time it runs (`stagent install`, the bridge's `hello` or the daemon)
and keeps in `<root>/host-id`, apart from `config.json`: `config.set` and
removing `notify` never change it; only `uninstall --level purge` removes
it. The app maps it to the saved connection it last used for this host,
to open notification links (`notify.click_base`). It is absent when it
could not be read or created, and from stagent versions before 0.4.0.

`blocked: {path, owner}` is present when another user owns this host's
stagent location, so no daemon, kept shell or detached session can work
here until that user or the host's administrator removes it: `path` is the
run directory (`/tmp/stagent-<uid>` on Linux), the parent of a relocated
data directory, or the daemon's socket or named pipe
(`\\.\pipe\stagent-<SID>`); `owner` is that user's name, or their uid
(Unix) / SID (Windows) when it does not resolve. Someone created it first,
in a shared temporary directory or the shared pipe namespace; stagent
checks the owner of every socket and pipe it connects to (on Windows a
pipe owned by `BUILTIN\Administrators` counts as the user's own: an
administrator's elevated daemon) and never connects to another user's.
A Windows pipe whose DACL refuses the user is reported here too when its
owner can still be read; when that is refused as well, the connection
fails as an ordinary error and `blocked` is absent. Plain SSH terminals
are unaffected. Absent from stagent versions before 0.7.0.

`session.spawn` starts the command with the environment of the user's login
shell (`$SHELL -l -i`, then `$SHELL -l`, captured once per bridge), not the
narrower one of the SSH exec channel, so agents on a PATH set in
`~/.bash_profile` / `~/.zprofile` / `~/.zshrc` are found. A bare command
name still missing from that PATH is looked up in the usual per-user tool
directories (`~/.bun/bin`, `~/.local/bin`, npm/pnpm/volta/nvm/fnm/mise,
Homebrew). `env` entries override both.

With `shell: true` (capability `persist`) the session runs the user's
shell instead of a command. On Linux and macOS that is their login shell:
argv `[<shell>, "-l"]`, `<shell>` being `SHELL` of that captured
environment (`/bin/sh` when unset). On Windows (capability
`persist_shell`) it is the shell an SSH terminal of the account gets:
argv `[<shell>]`, `<shell>` being the bridge's `SHELL` — sshd sets it to
its `DefaultShell`, `cmd.exe` when that is unset — or `%ComSpec%` when
`SHELL` is empty; no `-l` (Windows PowerShell 5.1 would run it as a
command and exit). `command` must then be empty or absent (`bad_request`
otherwise). This is how a terminal tab runs its shell in a persistent
session: it outlives the SSH connection and is re-attached later.

On Linux a holder that must leave the login session it was started from
(here the bridge's SSH session) to outlive it moves itself into a new scope
of the user's service manager as the first thing `stagent run` does — see
"Leaving the login session" under Command-line tools. The bridge starts
holders directly there. On macOS, while the user is logged in to the GUI,
the bridge starts them in the GUI login session so that the agent can use
the login keychain — see "Starting in the GUI login session".

Capabilities: `screen_mode`, `spawn`, `hooks`, `transcript`, `push`,
`persist`, and on Windows `persist_shell`. `persist` announces everything
this document marks with it: `session.spawn` `shell`, attach resume
(`since`, `offset`, `resumed`, `output.end`), the `hangup` signal,
input-mode restoring snapshots and sessions changing from `passthrough`
to `detached` (handoff). An app that relies on them treats a bridge
without `persist` as needing an update. Windows bridges before 0.5.0
announced `persist` but refused `shell` with `unsupported`; on Windows
`persist_shell` announces that `shell` works, and an app treats a
Windows bridge without it as needing an update.

### Session (forwarded to the session's holder)

| method | params | result |
|---|---|---|
| `session.info` | `{id}` | `Session` |
| `session.attach` | `{id, mode: "raw"\|"screen", fps?, since?}` | `{cols, rows, mode, offset, resumed}` then `output` / `resize` / `closed` notifications |
| `session.detach` | `{id}` | `{}` |
| `session.input` | `{id, text?, paste?, keys?[], submit?, local?}` | `{}`; `menu_open` for a chat message (`paste` and `submit`) while a menu is on the screen, or one shown before the message's turn to be written |
| `session.resize` | `{id, cols, rows, force?}` | `{}`; `not_size_owner` for a passthrough session without `force` |
| `session.scrollback` | `{id, before?, max_bytes?}` | `{data, start, end, first}` |
| `session.signal` | `{id, signal: "interrupt"\|"terminate"\|"kill"\|"hangup"}` | `{}` |

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

A raw snapshot leaves the client's terminal in the state the program's next
bytes expect: cursor position and visibility, pen, scroll region, autowrap,
the alternate screen, and (`persist`) the input modes — DECCKM (`?1`),
mouse tracking (`?9`, `?1000`, `?1002`, `?1003`), mouse encodings
(`?1005`, `?1006`, `?1015`), focus events (`?1004`) and bracketed paste
(`?2004`): every one of them the program has off is reset (`CSI ? … l`),
then every one it has on is set (`CSI ? … h`).

Stream positions (`persist`, raw mode). Every byte the program writes has a
position in the session's output stream, the same offsets as
`session.scrollback`. Each raw `output` carries `end`: the position after
its `data` (for a `reset: true` snapshot, the position the snapshot was
taken at; `end` is omitted when it is 0). The attach result's `offset` is
the position right after the first `output` item, i.e. where live output
continues. A client stores the `end` of the last `output` it fully wrote to
its terminal and passes it as `since` when it attaches again (after a
detach, a dropped connection, an app restart). The holder resumes when
`mode` is `raw`, the session has a scrollback, `since <= P` (P = the
current position), `P - since <= 1 MiB`, the bytes `[since, P)` are still
retained and the session size did not change at or after `since`. The
result then has `resumed: true` and the first `output` is `{reset: false,
data: bytes [since, P), end: P}` — omitted when `since == P` — so the
client's terminal continues exactly where it stopped, scrollback included.
Otherwise `resumed` is false and the first `output` is the usual `reset:
true` snapshot. Absent `since` never resumes. Screen mode ignores `since`
and its frames carry no `end`.

Terminal queries (DA, CPR, DECRQM, OSC 10/11/12/4 colour queries, …) the
program sends are answered by the holder's emulator whenever the session
has no local terminal (detached, or passthrough after a handoff); in a
passthrough session the local terminal answers them. Attached clients must
not answer queries themselves: their replies would arrive as typed input.

`session.signal`: `interrupt` writes Ctrl-C to the terminal, `terminate`
sends SIGTERM, `kill` SIGKILL, and `hangup` (`persist`) SIGHUP — what
closing a terminal does; a shell exits on it, which is how the app ends a
persistent shell session. Windows has no such signals: `terminate` and
`hangup` write ^C to the pseudo console and, if the program is still
running 3 seconds later, `TerminateProcess(…, 1)` it (an idle shell does
not exit on ^C, so a hung-up shell usually ends with exit code 1); `kill`
is `TerminateProcess(…, 1)` at once. Closing the pseudo console then ends
the processes still attached to it; a child with a console of its own
(`start ""`) is left running, as a `setsid` child is on Unix. The holder
records a `hangup` (on every OS): the session ends with `session_ended
{hung_up: true}` and its exit is not an abnormal one, whatever the exit
code (see "Push notifications"). On Windows, a passthrough session whose
console closes (the user closes the window it runs in; Windows has no
handoff) is hung up the same way and recorded as a `hangup` too.

`session.input` `local: true` is what `stagent attach` sends: keyboard
input of a terminal on the host. It counts as `last_local_input_at`, and
the focus reports in its `text` (`ESC [ I`, `ESC [ O`) set `focused` for as
long as its connection lasts. The app never sets it.

A passthrough session started with `stagent run --handoff` changes its
`mode` to `detached` when its local terminal hangs up (see "Command-line
tools"); `session.updated` reports it. From then on the app owns the size
(`session.resize` without `force`) and the holder answers terminal queries.

`session.input` is applied in order: `text` raw, `paste` (wrapped in
`ESC[200~ … ESC[201~` when the program enabled bracketed paste), `keys`,
then `\r` if `submit`. Key names: `enter esc tab shift-tab backspace delete
space up down right left home end pageup pagedown ctrl-c ctrl-d ctrl-z
ctrl-l ctrl-r ctrl-u ctrl-o ctrl-t`.

Input details: newlines inside `paste` are sent as `\r` (what a terminal
sends for Enter) and any `ESC[201~` inside it is removed, repeatedly until
none is left (removing one may join the bytes around it into another);
`up down right left home end` use the SS3 form (`ESC O A` …) while the
program has enabled application cursor keys (DECCKM); a `submit` that
follows `text`/`paste` is written 30 ms later (250 ms on Windows) as a
separate write so TUIs that debounce pasted input see Enter as a keystroke
— on Windows a pseudo console hands programs key events rather than a
bracketed paste, and Codex takes an Enter within about 120 ms of fast
input as part of it. While the
program has enabled win32-input-mode (`CSI ? 9001 h`, which conhost behind
a Windows pseudo console asks for), an Esc that starts no sequence (`esc`,
or a lone `ESC` in `text`) is sent as that mode's Esc key press and release
(`CSI 27;1;27;1;0;1 _`, `CSI 27;1;27;0;0;1 _`): Claude Code on Windows
does not take a lone `ESC` byte written there as the Esc key.

A chat message — `session.input` with both `paste` and `submit` — never
lands on a menu: while one of claude's or Codex's menus is on the holder's
screen — numbered options, one under the cursor (claude's `❯`, Codex's
`›`), a key hint below them; an approval, claude's AskUserQuestion, Codex's
update notice (`1. Update now`) or folder trust prompt (`1. Trust and
continue`) — nothing of it is written and the request fails with
`menu_open` (the app's copy of the screen lags behind the host's). The
holder looks again when the message's
turn in the input queue comes, right before writing its text: a menu shown
meanwhile drops the whole message, and the request fails with `menu_open`
too. So a chat message is answered only once its text is written (or
dropped), after the input queued before it. Once the text is written, the
holder looks again right before the `\r`: if a menu showed up after the
paste was written, the `\r` is held and written once the menu has been
off the screen for 400 ms (as `holder.prompt_gone`; claude shows parallel
tool calls' prompts one after the other), so it cannot pick the menu's
option. Input typed in the app or
on the host while the menu is up (its answer) keeps the hold; input typed
after the menu went drops the held `\r`, leaving the message in the
program's input box (terminal query replies and focus or mouse reports do
not count). The request has already succeeded in that case — the paste was
written; sending it again would type the message twice. `text`/`keys`
input (answers to menus, editing keys) is never refused or held. Protocol
stays 1: older holders write chat messages regardless, 0.7.x holders
answer a chat message once it is queued and write its text without
looking again, and holders before #440 refuse and hold only on approval
menus (`1. Yes…`).

`session.scrollback`: raw PTY bytes `[start, end)` of the session's output
stream, newest first page when `before` is 0; `first` is the oldest retained
offset (`start == first` → beginning reached). `max_bytes` default 256 KiB,
max 1 MiB.

### Daemon (forwarded to the daemon)

| method | params | result |
|---|---|---|
| `watch` | `{since}` | `{sessions[], approvals[], unwrapped[], seq, missed[], truncated}` then `event` / `session.updated` / `session.removed` / `unwrapped.updated` notifications |
| `sessions.list` | – | `{sessions[]}` |
| `conversations.list` | `{harness?[], limit?}` | `{conversations[]}` |
| `transcript.get` | `{session_id? \| harness+conversation_id? \| path?, limit?, before?}` | `{path, harness, messages[], cursor}` |
| `transcript.subscribe` | `{session_id? \| path?}` | `{}` then `transcript` notifications |
| `transcript.unsubscribe` | same | `{}` |
| `approvals.list` | – | `{approvals[]}` |
| `config.get` | – | `{config, notify: {last_error}}` |
| `config.set` | `{config}` — a JSON Merge Patch (RFC 7396) of `Config` | `{config, notify: {last_error}}` (the effective config, with defaults applied) |
| `notify.test` | – | `{}`; pushes a test to the enabled channels right away and emits a `notification` event (reason `test`); `not_configured` (nothing sent, no event) when no channel is enabled; `unavailable` with the failure when a channel failed |
| `presence.set` | `{foreground}` | `{}`; the app is (not) in the foreground (see "Push notifications") |

Nothing in the protocol answers an approval: the program's own screen does
(the PC, Claude's Remote Control, or a client typing the option's key with
`session.input`). `approvals.list`, `watch`'s `approvals[]` and the
`approval_*` events are for lists, notifications and summaries (see
`Approval` below). Daemons before 0.4.0 also had `approval.respond`; it is
now `unknown_method`.

`watch {since}` returns the events after `since` in `missed` (oldest first)
and `truncated: true` when retention already dropped some of them. Store
the highest `seq` seen and pass it on the next connection.

`unwrapped` lists the agents started interactively on this host without a
stagent session (`UnwrappedLaunch`, newest `last_activity_at` first; GLOSSARY
このホストの agent に出ない起動 — the app calls them ここに出ない agent), and
`unwrapped.updated` replaces the whole list whenever it changes. See
"Agents without a stagent session" under Harness hooks for what is recorded
and for how long. A stagent before 0.4.0 omits the field.

`config.set` applies its `config` merge patch to `config.json` as stored:
only the keys present in the patch change, `null` deletes a key, nested
objects merge member by member and any other value replaces the stored
one. Keys the patch does not name — including keys this stagent version
does not know — stay as they are, and defaults are never written to the
file (it holds only explicitly set values; `config.json` is mode 0600). The
merged document must still be a valid `Config` (for example a webhook URL
must be http(s)); otherwise the call fails with `bad_request` and nothing
is written. The result, like `config.get`, is the effective config with
defaults filled in. Example: `{"config": {"disable_handoff": true}}`
turns handoff off for `stagent run --handoff=auto` without touching any
other setting.

Every push (ntfy, webhook, including `notify.test`) is titled with the
host it comes from: `notify.host_label`, or the host name when it is empty
(no prefix when that is unknown too). A single notification reads
`<label> · <title>` (e.g. `開発機 · claude · api`), a digest
`<label>: <title>`. The in-app `notification` event keeps the plain title.

A notification about one session carries its `session_id`. On ntfy its
sequence ID (`X-Sequence-ID`) is the session id — or, for an id outside
ntfy's rule (1–64 of `[A-Za-z0-9_-]`; stagent's never are), the first 32
hex digits of its SHA-256: a newer notification of the session replaces
the older one. When `notify.click_base` is set, ntfy pushes carry a
`Click` link: `click_base` with `h=<host_id>` and, for a session,
`s=<session id>` added to its query (e.g.
`sshtermx://open?h=<host_id>&s=<session id>`). A digest and `notify.test`
link to the host only (`h`) and have no sequence ID. Nothing else goes
into the link; an empty `click_base` (or a host without `host_id`) sends
no `Click`. stagent knows no app scheme: the app sets `click_base`.

#### Push notifications

Every notification is a `notification` event for the app. It is also
pushed (ntfy, webhook) only when all of these hold:

- It is about a session started through stagent. Hooks of programs that
  were not (an IDE, `claude -p`, the SDK; no `STAGENT_SESSION_ID` and no
  known conversation) stay in-app.
- `notify.reasons` selects its reason. By default `needs_approval`,
  `waiting_input`, `turn_complete` and abnormal `exited` (level `warn`:
  an exit code other than 0, or a lost session — not a session a client
  hung up) are pushed; a normal `exited` and `terminal` are not (before
  0.4.0 everything was).
- Not `notify.skip_when_claude_app_notifies` with a session whose latest
  hook ran with `CLAUDE_CODE_BRIDGE_SESSION_ID` (claude connected to
  Remote Control: the Claude app notifies too). Claude's own settings are
  not read.
- No app in the foreground watching this host: a connection that has a
  `watch` and last sent `presence.set {foreground: true}`. A closed
  connection is not in the foreground, and a live `watch` alone never
  means it is (the app keeps its connection in the background). The
  bridge restores the app's foreground together with its watch after a
  daemon restart.
- Nobody present at the session (在席): no keystroke at a terminal on the
  host (`last_local_input_at`: the passthrough session's own terminal or
  `stagent attach`) within the last 60 s, nor within the last 10 min while
  that terminal has the focus (`focused`; terminals that send no focus
  reports count by keystrokes alone), and no file at the holder's
  `$CLAUDE_CLIENT_PRESENCE_FILE` (`presence_file`; it applies to every
  session, handed-off ones included). A handed-off session without such
  input counts as absent.

A push of a `passthrough` session waits 15 s first. These 60 s, 10 min
and 15 s are fixed. Presence and the foreground are checked when the push
goes out (after the wait).

A session settles what its notifications are about when its approval
closes (answered anywhere, or the turn moved on), when it goes back to
`working` — by a hook (a prompt was submitted) for what a hook raised, by
the terminal for what the terminal raised — or when it ends. Its pushes
still waiting (or in the digest window) are then dropped, and the one
already pushed is cleared on ntfy (`PUT <topic>/<sequence id>/clear`:
marked read and removed from the notification drawer; replacing and
clearing work on Android). Nothing is deleted. A server without
sequence IDs or clear (ntfy before 2.16) gets plain notifications: the
daemon logs one line (once for replacing, per failed clear) and keeps
going; it never checks the server's version and adds no cooldown beyond
`debounce_ms` and the digest. A failed clear is not a failed push
(`last_error`).

`notify.lang` (`en` default, `ja`, `ko`, `zh`) is the language of the
fixed phrases stagent writes — notification bodies such as `Turn
complete`, digest titles and the `notify.test` body — in events and
pushes alike. `reason` never changes; the app can build its own text from
it.

`notify.last_error` (in the `config.get` / `config.set` result and in
`stagent doctor`) holds, per channel (`ntfy`, `webhook`), the last failed
push: `{at (unix ms), status? (HTTP status; absent for a connection
error), error}`. A successful push to the channel removes its entry;
channels without a failure are absent and `last_error` is `{}` when none
failed. `error` is the HTTP status line (`429 Too Many Requests`) or the
connection error, never the ntfy topic or the webhook URL; `notify.test`'s
`unavailable` message is built the same way. Queued pushes and
`notify.test` both count. The daemon keeps the entries in
`state/notify-errors.json` (mode 0600), so they survive its restarts.

If the daemon restarts while the bridge is connected, daemon requests in
flight fail with `unavailable` and the bridge restores the watch itself:
events the app missed arrive as `event`, every current session as
`session.updated`, sessions that disappeared as `session.removed`, and the
current `unwrapped` list (empty after a restart) as `unwrapped.updated`. The
app needs no daemon-restart handling. Session requests go to the holders
directly and are unaffected.

### Notifications

| method | params |
|---|---|
| `event` | `Event` |
| `session.updated` | `Session` (full object; replaces the previous one) |
| `session.removed` | `{id}` |
| `output` | `{id, data, reset?, end?}` |
| `resize` | `{id, cols, rows}` |
| `closed` | `{id, exit_code}` |
| `transcript` | `{session_id?, path, messages[]}` |
| `unwrapped.updated` | `{unwrapped[]}` (the whole current list of `UnwrappedLaunch`; replaces the previous one) |

## Objects

`Session`: `id, harness (claude|codex|omp|other), command[], cwd, pid,
holder_pid, mode (passthrough|detached), attached, state (working|idle|
waiting_input|needs_approval|exited), state_source (hook|terminal|activity|
process), title?, conversation_id?, transcript_path?, last_message?, cols,
rows, started_at, last_activity_at, last_local_input_at?, focused?,
presence_file?, exit_code?`.
`mode` is fixed for the life of a session except for one transition
(`persist`): a `passthrough` session started with `--handoff` becomes
`detached` when its local terminal hangs up.

`attached` (always present) is true while at least one client is attached
to the session through `session.attach`, in either mode — an app tab,
`stagent attach`. A passthrough session's own local terminal is not a
client and does not count. The holder reports each change between no
client and some at once (`session.updated`, not held back like
`last_activity_at`); an ended session is never attached. The app shows a
session as connected to a terminal when `mode == passthrough || attached`.

`last_local_input_at` (unix ms) is when a terminal on the host last typed
or pasted anything into the session — the session's own local terminal
(that of a `passthrough` session, a PC's or an SSH tab's) or a `stagent
attach` (`session.input` with `local`); it is omitted until one has.
Bytes those terminals send on their own do not count:
replies to the program's queries (cursor position, device attributes, mode,
status and window reports, OSC/DCS/APC strings such as color replies and
XTVERSION) and focus and mouse reports. A read from the terminal counts when
anything is left once those are removed, so a bracketed paste counts. The
app's input (`session.input` without `local`) never touches it. The
holder reports it at most once per second, the first keystroke after a
quiet second at once, and watchers get each change at once
(`session.updated`, not held back like `last_activity_at`); `session.info`
on the holder has the current value. The app treats a keystroke within the
last 5 s as "typing at the PC" and holds back typing a prompt into the
agent meanwhile; presence (see "Push notifications") uses the same value.

`focused` is true while one of those terminals last reported focus in
(`ESC [ I`, sent once the program turned focus reporting on) and not focus
out since; a `stagent attach` that goes away and a local terminal that
hangs up lose it. `presence_file` is the holder's
`$CLAUDE_CLIENT_PRESENCE_FILE`, when set.

`harness` starts as the program the session runs (`other` for a shell).
Hooks of an agent running inside the session (by `STAGENT_SESSION_ID`)
set it to that agent along with `conversation_id` and `transcript_path`.
For a session that started as `other`, the daemon then checks every 2 s
whether that agent still runs in the session's foreground — recognized as
`stagent follow --session` does, anywhere in the session's process tree on
Windows — and once it does not, sets `harness` back to `other`, clears
`conversation_id`, `transcript_path` and `last_message`, drops the
hook-derived state (the terminal/activity state remains), cancels the
session's pending approvals (`approval_resolved` `by: cancelled`) and sends
`session.updated`. An agent the daemon never recognizes is given up after
five checks. The agent's next hook sets the harness again.

`Event`: `seq, time, session_id?, kind, data`. Kinds and `data`:

| kind | data |
|---|---|
| `session_started` | `{harness, command[], cwd, mode}` |
| `session_ended` | `{exit_code, hung_up?}` — `hung_up`: a client ended it with `session.signal hangup`, or (Windows) the console of a passthrough session closed |
| `state_changed` | `{from, to, source}` |
| `notification` | `{title, body, level (info\|warn), reason, count?, session_id?}` — reason `waiting_input\|needs_approval\|turn_complete\|exited\|terminal\|test\|digest`; `session_id` names the session it is about (absent for a digest, `notify.test` and approvals of no session); webhooks post this object |
| `approval_requested` | `Approval` |
| `approval_resolved` | `{request_id, by: "cancelled"}` |

`Approval`: `request_id, session_id, harness, tool_name?, summary,
created_at`. Only programs running in a stagent session raise approvals,
from their PermissionRequest hook; elsewhere (an IDE, `claude -p`, the
SDK) the hook returns at once and nothing is registered or pushed. claude's
hook is held, with no time limit, while its prompt is open; Codex shows its
prompt only after the hook returned, so its hook returns at once. Neither
tells when the prompt is answered: claude does not stop the hook (it runs
until the approved tool finished), and an Esc on Codex's prompt ends the
turn without Stop while Codex blinks its terminal title for as long as the
prompt is up, so no output activity marks the answer. The session's holder
therefore watches its screen (local IPC only): while a claude or Codex
approval of the session is pending, the daemon sends the holder
`holder.prompt_watch {id, gen, on: true}` (a new `gen` with every approval;
`on: false` once none is pending, again to a holder that re-registers).
The holder takes the permission menu — numbered options starting with `1.
Yes`, one under the cursor (claude's `❯`, Codex's `›`), a key hint below
them, claude's plan approval included — that is on the screen during the
watch as the watch's menu, identified by the dialog's text above the
options (from its top: claude's top rule, or for Codex the nearest line
above that starts in the first column, i.e. the transcript entry over the
dialog; that is the title, command or file with its preview, question; the
cursor, the options and the transcript above do not count). Once no menu
of that text has been on the screen for 400 ms — none, or another
prompt's — it sends `holder.prompt_gone {id, gen}`, once per `gen`.
claude shows parallel tool calls' prompts one after the other, so a watch
whose menu is still up when the next one starts keeps being watched (the
next one does not take that menu) and is reported when it goes. The
session's approvals registered up to that watch then close and their hooks
return; one registered later (the next prompt) stays. A holder that never
saw the menu (a resize forgets it until it is seen again at the new size),
one started before 0.4.0 (it ignores the notification) and, for Codex, one
of 0.5.0 or earlier (it knows only claude's menu) report nothing, and a
watch that saw no menu when the next one started is left to the next one's
report: the approval then closes with a later watch's report, when claude
stops the hook (the tool finished, or claude was interrupted), for Codex on
the next output activity (1 s after the hook or later), or at Stop. Two
prompts in a row for the very same command or file text look alike: the
first closes when the second is answered, the second when claude stops its
hook or at Stop. Any approval also closes at Stop, UserPromptSubmit and
when the agent leaves the session. When a session's last approval closes,
its hook-derived `needs_approval` goes and the terminal/activity state
shows again.

`UnwrappedLaunch`: `conversation_id, harness (claude|codex|omp), cwd, reason
(old_terminal|bypassed|ide|ssh|no_terminal), first_seen_at,
last_activity_at` (unix ms, host clock: the first and the latest hook of
the conversation). `reason`:
`old_terminal` — the shell lacks the wrapper's `STAGENT_SHELL_WRAPPER`
marker: a terminal opened before このホストの準備, or one that does not read
the shell's rc files (open a new terminal); `bypassed` — the shell has the
wrapper but the start went around it (`command claude`, a full path; start
`claude` as it is); `ide` — an IDE extension or a desktop app (Claude
Code's `CLAUDE_CODE_ENTRYPOINT` is neither `cli` nor `sdk-*`; the Codex
desktop app or IDE extension; cannot be shown); `ssh` (Windows) — started
in an SSH session, where the PowerShell wrapper does not wrap and the agent
ends with the connection (start it in a kept shell); `no_terminal`
(Windows) — no marker and no shell above the agent: a program started it
without a terminal (cannot be shown).

`Message`: `role (user|assistant|tool|system), text, tool_name?, time?`.

`Conversation`: `harness, id, cwd, title, updated_at, path,
live_session_id?`.

`Config`: see `wire.Config` — `notify {ntfy {enabled, server, topic,
token?}, webhook {enabled, url, headers?}, debounce_ms, digest_window_ms,
host_label, click_base, reasons {needs_approval, waiting_input,
turn_complete, exited, terminal}, lang, skip_when_claude_app_notifies},
retention {events_days, events_max, scrollback_days,
scrollback_total_mib, scrollback_session_mib}, idle_after_ms,
disable_handoff`. `disable_handoff` (default false) makes
`stagent run --handoff=auto` — what the shell wrappers run — end the
program with its terminal unless `STAGENT_HANDOFF` decides otherwise;
holders read it at every start, so it applies from the next start on, also
in shells that are already open. `notify.host_label` (no default: empty
means the host name) names this host in the title of every push (see
`notify.test`); the app sets it to the connection's name. `notify.click_base`
(no default: empty means no link) is the link a push opens when tapped
(see `notify.test`); it must be an absolute URL. `notify.reasons` selects
what is pushed (see "Push notifications"): booleans, defaults `true`,
`true`, `true` and `terminal` `false`; `exited` is `off`, `error`
(default: abnormal exits only) or `all`. `notify.lang` is `en` (default),
`ja`, `ko` or `zh`; the app sets it to its own language.
`notify.skip_when_claude_app_notifies` defaults to false. Missing keys take
their defaults; another `exited` or `lang` is `bad_request`.

## `stagent follow` (terminal chat view, follow protocol 1)

`stagent follow` streams the transcript of the coding agent (Claude Code,
Codex, omp) running in a terminal tab. The app runs it over an exec channel
**on the same SSH connection as the tab's shell**, with the command forms of
"Starting the bridge" (`follow` instead of `bridge`). It needs no daemon,
hooks or installation beyond the binary, and exits when stdin reaches EOF.

`stagent follow --session ID` (also `-session ID`, `--session=ID`; ID = 16
lowercase hex chars) follows the agent running inside stagent session ID
instead of the tab's: the process tree searched is the session's program
(its `pid` from the holder's `session.info`) rather than the tab found
below. Agent recognition, multiplexers inside that tree, transcripts,
`select` / `older` and all frames are the same. The holder is asked on
every scan (1 s, bounded by 1 s). When it gives no program to search the
`target` is `{mux: "none", reason: "no_terminal", diag, agents: [],
selected: null}` with `diag` `stagent session <ID>: holder not reachable:
<error>` (no such session, or it ended and its holder exited), `stagent
session <ID>: session ended (exit <N>)` or `stagent session <ID>: holder
reports no program pid`; follow keeps scanning and the usual target follows
once the holder answers. A holder answering without an agent in the tree is
the usual `no_agent`. An invalid ID, an unknown flag or a positional
argument print a message on stderr and exit 2; without `--session`
`stagent follow` behaves as described below.

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
| `stagent install --json` | `{ok, version, layout{root, bin, run_dir, data_dir, data_on_network_fs}, replaced_daemon?, changes[], notes[{code, text, args?}]}` — creates directories and the manifest and completes an update (below); idempotent |
| `stagent doctor --json` | `DoctorReport` |
| `stagent integrate --json --plan\|--apply [--harness claude,codex,omp] [--shell-wrapper] [--service] [--linger] [--terminal] [--execution-policy] [--wsl-keep-running] [--remove claude,codex,omp,shell-wrapper,service,linger,terminal,wsl-keep-running]` | `{applied, result, login_shell, changes[{id, target, action (create\|modify\|delete\|skip), summary, diff, error?, error_code?}], notes[{code, text, args?}]}` |
| `stagent uninstall --json --level stop\|unhook\|purge [--uploads] [--linger]` | `{level, steps[{action, target, ok, error?}], removed[], failed[{path, reason, sessions[]}], remaining[]}` |
| `stagent wsl-shutdown --json` | (WSL) runs `wsl.exe --shutdown`, which normally ends this command, see below |

Levels are cumulative: `unhook` includes `stop`, `purge` includes `unhook`.

`--linger` (Linux) turns on lingering, `loginctl --no-ask-password
enable-linger <uid>`, so the user's service manager — and with it the
scopes holders and the daemon move into — keeps running after the last
logout. It is the change `{id: "linger", target: "loginctl:linger",
action: "create", diff: ""}`, absent when lingering is on already; applied,
it is recorded in the manifest (`doctor`'s
`persistence.linger_enabled_by_stagent`). Off Linux it is a `skip`.
`--remove linger` (`action: "delete"`, `loginctl disable-linger`) and
`uninstall --level purge --linger` (step `disable-linger`) turn lingering
off only when the manifest records that stagent turned it on; otherwise
they do nothing, and lingering that was on before is never touched.
`unhook` and `purge` without `--linger` leave lingering as it is.

`--terminal` (macOS) adds `{ProcessName = stagent;}` to `noWarnProcesses`
of every Terminal.app profile (`Window Settings` of `com.apple.Terminal`)
that lacks it, so Terminal does not ask before closing a window running an
agent through the shell wrapper, nor at logout; the agent is handed off.
Terminal counts only `login`, the shell and `stagent` as a window's
processes (the agent runs on stagent's PTY). A profile without the key gets
Terminal's default list along (`screen`, `tmux`), since writing the key
replaces it. The preferences are read and written through cfprefsd
(`defaults export` / `defaults import`), not the plist file. A running
Terminal keeps the profiles it loaded at launch: the change applies once
Terminal is quit and opened again (at the latest at the next login), and
quitting does not write the old list back; when Terminal runs, `notes`
says so. It is the
change `{id: "terminal", target: "com.apple.Terminal", action: "modify"}`
whose `diff` shows each touched profile's list before and after; absent
when every profile has it or Terminal has no saved profiles; off macOS a
`skip`. The manifest records each profile stagent added to (`doctor`'s
`terminal.added_by_stagent`). `--remove terminal` and `uninstall --level
unhook` take out only those entries; a list the user changed since keeps
the change, a key stagent created that holds just the default list again
is removed, and a `stagent` entry stagent did not add stays. With nothing
recorded `--remove terminal` does nothing, on any OS.

`--execution-policy` (Windows) runs `Set-ExecutionPolicy -Scope
CurrentUser -ExecutionPolicy RemoteSigned -Force` in each installed
PowerShell whose effective policy keeps it from running the profile with
the wrapper (`Restricted`, `AllSigned`; `doctor`'s `powershell[]`), and
in no other. Each is the change `{id: "execution-policy-powershell" |
"execution-policy-pwsh", target: "powershell:ExecutionPolicy" |
"pwsh:ExecutionPolicy", action: "modify", diff: ""}`; none when every
PowerShell runs its profile. Applying reads the effective policy again: one
that a more specific scope (Group Policy) keeps is the error
`execution_policy_overridden` with PowerShell's message. It is not
recorded and nothing removes it. Off Windows it is a `skip`.

`--wsl-keep-running` (WSL) keeps WSL from shutting the distribution down
once no Windows-side client (a Windows terminal tab, `bash.exe` behind
Windows' sshd) uses it, which otherwise ends detached sessions and kept
shells some 15–40 s after the SSH connection or the PC's terminal closes
(processes inside WSL do not count). It sets `[general]
instanceIdleTimeout=-1` in the Windows user's `.wslconfig`, found with
`cmd.exe /c echo %USERPROFILE%` and `wslpath` through interop and written
through the drive mount (`/mnt/c/Users/<user>/.wslconfig`). The setting is
the user's, for every distribution, and WSL reads it only when it starts:
it applies after `wsl --shutdown` (`wsl-shutdown`) or a restart of the PC.
Exactly one line changes and every other byte is kept: a missing file is
created as `[general]` plus the line; under an existing `[general]` the
line goes right below the header; a non-negative `instanceIdleTimeout`
line is changed to `-1`; otherwise `[general]` and the line are appended
(after a blank line). Line endings follow the file (CRLF for a new one).
It is the change `{id: "wsl-keep-running", target: <path>, action:
"create"|"modify"}`, absent when the file sets a negative value already;
the manifest records it as a config entry (`doctor`'s
`persistence.wsl.keep_running_by_stagent`). When the Windows profile
cannot be reached (interop off, no drive mount) it is a `skip` with
`error_code` `wslconfig_unreachable` and target
`%USERPROFILE%\.wslconfig`; a file saved as UTF-16 is `unmanaged_file`;
off WSL a `skip`. `--remove wsl-keep-running` and `uninstall --level
purge` (always, no flag) undo only a recorded edit: a file unchanged since
is deleted when stagent created it or restored from the backup; one edited
since loses only stagent's line (with the `[general]` header and blank line
stagent added, once the section holds nothing else), or gets back the line
stagent changed. A line that no longer sets a negative value, and one the
user wrote before, are never touched; `stop` and `unhook` leave the file.
Either direction notes `wsl_restart`.

`wsl-shutdown` (WSL) runs `wsl.exe --shutdown` through interop. It stops
every WSL distribution of the user — this one and Docker Desktop's
included — so stagent and the exec channel running it normally end
without output (behind Windows' sshd the SSH connection stays open; the
next exec starts WSL again). A client takes an exit without an `{"error"}`
document as success. Off WSL, or when `wsl.exe` fails, it exits 1 with
`{"error": "..."}`; if `wsl.exe` returns and stagent still runs, it prints
`{"shutdown": true}`.

`--service` on macOS writes `~/Library/LaunchAgents/com.obutora.stagent.plist`
with `LimitLoadToSessionType` `Background` and `ProcessType` `Standard`
and bootstraps it into the user domain, `launchctl bootstrap user/<uid>`
(a load of an earlier version in `gui/<uid>`, which ends at logout, is
booted out first). The daemon keeps running after a logout either way
(see "Leaving the login session"); whether the user domain starts the
agent after a restart before anyone has logged in has not been verified.

`integrate` plans (default, `--plan`) or applies (`--apply`) per target
file. Its output, in both modes:

- `changes[]`: one entry per file that changes. A target that cannot be
  planned is a change with `action: "skip"`, empty `diff`, and `error` /
  `error_code`; nothing is written to it and the other targets go on. A
  change that fails while applying keeps its planned `action` and gets
  `error` / `error_code`. Files already in the wanted state produce no
  entry.
- `error` is the raw message; `error_code` classifies it:
  `not_writable` (permission denied creating, writing or renaming),
  `read_only_fs` (read-only file system), `utf16_profile` (a PowerShell
  profile saved as UTF-16; skipped, add the wrapper by hand),
  `unmanaged_file` (the file exists but its content is not something
  stagent edits safely: an owned target with foreign content, settings
  that are not valid JSON or have an unexpected `hooks` layout, a Codex
  `features` inline table), `linger_denied` (polkit refused
  `set-self-linger`: an administrator has to run `sudo loginctl
  enable-linger <user>`; `error` is loginctl's message),
  `execution_policy_overridden` (Group Policy keeps a PowerShell
  execution policy that does not run the profile; `error` is
  PowerShell's message), `wslconfig_unreachable` (WSL: the Windows user
  profile holding `.wslconfig` cannot be found, interop or the drive mount
  being off; edit the file on the PC), `io_error` (anything else).
- `result`: `nothing_to_do` when `--shell-wrapper` was asked for and no
  supported shell (bash, zsh, fish, PowerShell) was found (takes
  precedence); otherwise `ok` without any error (also when everything is
  already in place and `changes` is empty), `failed` when there are errors
  and no target was planned/applied cleanly or already in place, and
  `partial` in between. Files written successfully are not rolled back.
- `login_shell`: the base name of `$SHELL` (`""` when unset; on Windows
  sshd sets it to its `DefaultShell`, `cmd.exe` when that is unset), e.g.
  for telling the user which shell was found when the result is
  `nothing_to_do`.
- `applied` is true when `--apply` wrote every change (and the manifest).
- `notes[]`: remarks for the user (below).

`notes[]` of `install` and `integrate` are `{code, text, args?}`. `text`
is the remark in English, with the values of `args` filled in, and may be
reworded; `code` keeps its meaning, so a client can word the remark itself
from `code` and `args` (an object of strings). A client shows `text` for a
code it does not know.

| code | args | remark |
|---|---|---|
| `claude_restart` | – | running Claude sessions load the new hooks only after a restart |
| `claude_windows_hooks` | – | on Windows the hooks run `stagent.exe` directly; deleting the binary without `uninstall --level unhook` leaves hooks that error until removed (`doctor` lists them as orphans) |
| `omp_restart` | – | running omp sessions load the extension only after a restart |
| `codex_trust` | – | Codex asks to review and trust the new hooks at its next start; stagent writes no trust entries |
| `codex_notify_fallback` | `reason` | Codex is integrated through the notify program (turn complete only, no approvals); `reason`: `inline_features` (`features` is an inline table), `hooks_removed` (this Codex removed the hooks feature), `no_hooks_feature` (this Codex has none) |
| `codex_notify_kept` | `command` | Codex already has another notify program (`command`); stagent left it, so turn-complete events are not reported |
| `codex_features_kept` | – | removing left `[features] hooks` as it is: it changed after stagent enabled it |
| `no_shell` | – | no supported shell rc file was found; nothing to wrap (`result` `nothing_to_do`) |
| `powershell_no_bom` | `path`, `bin` | the profile `path` has no UTF-8 BOM and the wrapper names stagent by the non-ASCII path `bin`; Windows PowerShell 5.1 will not find it |
| `cmd_not_wrapped` | `shell` | (`--shell-wrapper`, Windows) the SSH default shell `shell` is cmd, which has no profile: the wrapper goes into the PowerShell profiles only |
| `binary_missing` | `path` | stagent is not installed at `path` yet; hooks and wrappers stay inactive until it is |
| `service_linger` | – | (`--service`, systemd) lingering is off, so the user service stops at the last logout |
| `launch_agent` | – | (`--service`, macOS) the LaunchAgent runs in the user domain and survives logout; starting before any login after a restart is unverified |
| `terminal_no_profiles` | – | Terminal.app has no saved profiles; nothing to change |
| `terminal_running` | – | Terminal.app is running and applies the change once it is quit and opened again |
| `wsl_restart` | – | (`--wsl-keep-running`, `--remove wsl-keep-running`) WSL reads `.wslconfig` when it starts: the change applies after `wsl --shutdown` or a restart of the PC |
| `not_applied` | `failed` | (`--apply`) the change ids that failed, comma-separated (`manifest: <error>` when the manifest could not be saved) |
| `legacy_daemon_stopped` | `addr` | (`install`) a daemon of a version before 0.3.0 at `addr` was stopped; sessions it had keep running but are no longer listed |
| `daemon_replace_failed` | `running`, `version`, `error` | (`install`) the running daemon (`running`) could not be replaced by this `version` |

`--shell-wrapper` writes the wrapper block to `~/.bashrc` (when it exists
or bash is the login shell), `$ZDOTDIR/.zshrc` (same for zsh), a fish file
in `~/.config/fish/conf.d/` (fish login shell or `~/.config/fish`
present) and, on Windows, the Windows PowerShell and PowerShell 7
profiles. On Linux and macOS, when bash is wrapped and `~/.bash_profile`
exists, the block also goes there (id `shell-bash-profile`): a login bash,
as macOS Terminal starts, reads only that file. New PowerShell profiles are
written as UTF-8 without BOM (an existing file keeps its encoding); the
block names stagent as `Join-Path $env:USERPROFILE
'.ssh-term\agent\bin\stagent.exe'`, so it is ASCII. Only when stagent lies
outside `%USERPROFILE%` is the path written literally, and if it has
non-ASCII characters and the profile has no BOM, `notes` warns that
Windows PowerShell 5.1 would misread it. `--remove shell-wrapper` and
`uninstall --level unhook` take the block out of every one of these files.

Removing (`--remove`, `uninstall --level unhook`) brings a file back to its
state before stagent when it is still exactly what stagent last wrote:
a file stagent created is deleted, together with the directories stagent
created for it (e.g. `~/.omp/agent/extensions`, and `~/.omp` when it did
not exist) once they are empty again; a file stagent edited is restored
from the backup it took at the first edit (`<file>.sshterm-bak-<UTC
time>`), and the backup is deleted. When the file changed since, only
stagent's elements are taken out and the backup stays, as the only copy
of the content before stagent: `doctor` lists it (`purge` level) and
`uninstall --level purge` deletes it. A directory holding anything else
stays.

`install` completes an update of the binary (the app runs it after
placing a new one):

- The shell wrapper blocks already in place are rewritten to this
  version's block; where bash has the block, an existing `~/.bash_profile`
  gets it too. A host without the wrapper gets none. Each rewritten file is
  an entry of `changes[]` in `integrate`'s form (with `error` /
  `error_code` when it could not be written); a second `install` has
  nothing to change.
- A running daemon of another version is stopped as `uninstall --level
  stop` stops it (holders take it as deliberate) and started again from
  this binary: through the login service when one is installed (`systemctl
  --user restart`, `launchctl kickstart -k`, `schtasks /Run`), detached
  otherwise or when the service manager refuses. The holders keep their
  sessions and register them with the new daemon. `replaced_daemon` is the
  version replaced, once the new daemon answers; a failure is a note, and
  `doctor` keeps reporting the mismatch. Run by a coding agent, `install`
  cannot stop the daemon (`daemon.shutdown` fails with `agent_refused`;
  the daemon is not killed instead): the note says so, and a person runs
  it again in a terminal. `uninstall --level stop` and `integrate
  --service` fail the same way.

Sockets live in `run_dir`. On Linux (without `STAGENT_HOME`) that is
`<TMPDIR or /tmp>/stagent-<uid>`, not `$XDG_RUNTIME_DIR`, which logind
removes at the user's last logout and with it every detached session's
socket. It is created with mode 0700; stagent refuses to use it (the
command fails with an error naming it) when it is a symlink or not a
directory, belongs to another user or is accessible by group or others.
One that belongs to another user is reported as `blocked` by `hello` and
`doctor`, and requests that need it fail with `foreign_owner`.
Holders and the daemon touch their sockets and `run_dir` every hour so tmp
cleaners keep them. `install` stops a daemon of an earlier version still
listening at `$XDG_RUNTIME_DIR/stagent/stagent.sock` (best effort, reported
in `notes`); sessions that version started stay reachable only through its
old directory until they end. With `STAGENT_HOME` (tests, isolated
installs) `run_dir` is `<root>/run`, or a `stagent-<uid>-<hash>` directory
in tmp when socket paths would be too long there.

`DoctorReport`:

```json
{
  "version": "0.1.0", "protocol": 1, "os": "linux", "arch": "amd64", "home": "/home/u",
  "host_id": "0123456789abcdef0123456789abcdef",
  "layout": {"root": "...", "bin": "...", "run_dir": "...", "data_dir": "...", "data_on_network_fs": false},
  "daemon": {"running": true, "pid": 123, "version": "0.1.0", "sessions": 2,
             "bootstrap_swapped": null, "bootstrap_error": null},
  "blocked": null,
  "harnesses": [
    {"id": "claude", "found": true, "path": "/usr/bin/claude", "version": "2.1.284",
     "config_path": "/home/u/.claude/settings.json", "integrated": true,
     "hooks_supported": true, "notes": [], "remote_control_at_startup": null}
  ],
  "shell_wrapper": {"installed": false, "files": [], "last_run_at": null, "last_run_survives_logout": null,
                    "last_run_bootstrap_error": null},
  "service": {"kind": "systemd|launchd|schtasks|none", "installed": false, "running": false},
  "persistence": {"run_dir": "/tmp/stagent-1000", "run_dir_survives_logout": true,
                  "kill_user_processes": false, "linger": true,
                  "linger_needed": false, "linger_reason": null, "linger_enabled_by_stagent": false,
                  "wsl": null},
  "terminal": null,
  "powershell": null,
  "redirection_guard": null,
  "notify": {"last_error": {"ntfy": {"at": 1767225600000, "status": 429, "error": "429 Too Many Requests"}}},
  "unwrapped": [{"conversation_id": "…", "harness": "claude", "cwd": "/home/u/api", "reason": "old_terminal",
                 "first_seen_at": 1767225600000, "last_activity_at": 1767225900000}],
  "orphans": [{"target": "/home/u/.claude/settings.json", "detail": "..."}],
  "problems": []
}
```

`shell_wrapper.files` lists the files that hold the wrapper block, and
`last_run_at` (unix ms, `null` when never) is when `stagent run
--handoff=auto` — which only the shell wrappers pass — last started a
program on this host, i.e. the last start through a wrapper.
`last_run_survives_logout` is whether that start outlives the user's
logout: on Linux, where it ended up after leaving the login session (see
"Leaving the login session"), judged with `kill_user_processes` and
`linger` as they are now, so turning lingering off makes it false for a
start under the service manager; on macOS, whether it swapped to the
per-user bootstrap port, so that the programs the agent starts can still
use the network after a logout from the GUI. `null` when it cannot be
told, on Windows, or the start was recorded by stagent before 0.4.0 (on
Linux, before 0.4.1).
`last_run_bootstrap_error` is why that swap failed (macOS), `null`
otherwise; a failure adds a `problems` line.

`daemon.bootstrap_swapped` is whether the running daemon swapped to the
per-user bootstrap port when it started (macOS), with
`daemon.bootstrap_error` saying why not (`null` otherwise; a failure adds a
`problems` line). Both are `null` when no daemon runs, off macOS, or the
daemon is a version before 0.4.0 (it does not report it).

`terminal` (macOS; `null` elsewhere) is Terminal.app's close confirmation
(`integrate --terminal`): `{"profiles": 3, "no_warn": false,
"added_by_stagent": false}`. `profiles` is the number of saved Terminal
profiles (0 when none or unreadable), `no_warn` whether every one has
`stagent` in `noWarnProcesses`, and `added_by_stagent` whether the
manifest records profiles stagent added it to.

`powershell` (Windows; `null` elsewhere) lists each installed PowerShell —
Windows PowerShell 5.1, then PowerShell 7 when `pwsh` is on `PATH`:
`[{"id": "powershell", "name": "Windows PowerShell 5.1", "path":
"C:\\WINDOWS\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
"execution_policy": "Restricted", "loads_profile": false}]`.
`execution_policy` is its effective policy (`Get-ExecutionPolicy`), `null`
when it cannot be read; `loads_profile` is false when that policy keeps the
profile with the wrapper from running (`Restricted`, `AllSigned`: the block
is not signed), `null` when unknown. A `false` adds a `problems` line
suggesting `integrate --execution-policy`.

`redirection_guard` (Windows; `null` elsewhere or when Windows cannot
tell) is whether `doctor` itself runs with RedirectionGuard
(`ProcessRedirectionTrustPolicy.EnforceRedirectionTrust`). sshd gets it
from its Image File Execution Options and every process started over SSH
inherits it, holders of kept shells included, even after leaving sshd's
job: such processes do not follow junctions a non-administrator created,
so tools reached through one (scoop's shims) fail in kept shells. stagent
treats it as a known limitation.

`host_id` is the host's id (see `hello`), absent until stagent created it;
`doctor` only reads it.

`blocked` is `hello`'s `blocked` (`{path, owner}`, or `null`); when set,
`problems` names it too.

`unwrapped` is the running daemon's list of agents started without a
stagent session (`UnwrappedLaunch`, the same as `watch`'s); `[]` when the
daemon does not run.

The `claude` item (only it; the others omit the key) has
`remote_control_at_startup`: the value of `remoteControlAtStartup` in the
user's `~/.claude/settings.json` — `true`, `false`, or `null` when the key
is absent, not a boolean, or the file is missing or unreadable. Managed
and project settings are not read, and stagent never changes the setting.

`persistence` tells whether detached sessions outlive the user's logout:
`run_dir_survives_logout` is false when `run_dir` is below
`$XDG_RUNTIME_DIR` (always true off Linux); `kill_user_processes` is
logind's `KillUserProcesses` and `linger` whether lingering is enabled for
the user (`null` when unknown or off Linux).

`linger_needed` says whether agents started on this host end at logout
unless lingering is turned on (`integrate --linger`). On WSL it is always
false: WSL keeps the user's login session for as long as the instance
runs, so lingering changes nothing there (`wsl` does). Elsewhere it is
false when
lingering is on; otherwise, once a start through a wrapper is recorded
and `last_run_survives_logout` is known, it is the opposite of that
(`linger_reason` `last_run`); else true when `kill_user_processes` is true
(`kill_user_processes`) or the user has a login session of type `x11` or
`wayland` now, whose terminals run under the user's service manager
(`graphical_session`). It is `null` when neither `kill_user_processes`
nor `linger` can be read (and off Linux; macOS needs no lingering).
`linger_reason` is `null` unless `linger_needed` is true; then `problems`
says so and suggests
`loginctl enable-linger`. `linger_enabled_by_stagent` is true when the
manifest records that `integrate --linger` turned lingering on. `hello`
reports none of this (it would ask logind at every connection).

`wsl` is `null` unless Linux runs in WSL (`/proc/sys/kernel/osrelease`
names Microsoft). Then it is
`{"distro": "Ubuntu", "config_path": "/mnt/c/Users/me/.wslconfig",
"instance_idle_timeout": 15000, "networking_mode": "nat",
"keep_running_needed": true, "keep_running_by_stagent": false}`:
`distro` is `$WSL_DISTRO_NAME` (`""` when unset); `config_path` the Linux
path of the Windows user's `.wslconfig`, existing or not (`null` when the
Windows profile cannot be reached); `instance_idle_timeout` is `[general]
instanceIdleTimeout` in ms, WSL's default `15000` when the file or key is
absent or not an integer; `networking_mode` is `[wsl2] networkingMode` in
lower case, `"nat"` when absent (both `null` when the file cannot be read,
e.g. UTF-16). `keep_running_needed` is true when WSL shuts the
distribution down once no Windows-side client uses it
(`instance_idle_timeout` ≥ 0; `problems` then says so and suggests
`integrate --apply --wsl-keep-running`), false when it is negative, and
`null` when unknown. It follows the file, which WSL reads only when it
starts: right after `--wsl-keep-running` it is false though WSL may not
have restarted yet. `keep_running_by_stagent` is true when the manifest
records stagent's edit and the file still has the line.

`notify.last_error` is the daemon's last push failure per channel (see
`config.get`), read from its file, so it is reported whether or not the
daemon runs; `{}` when none. Each failing channel adds a `problems` line
(`notify: the last push to ntfy failed at <RFC 3339 time>: <error>`).

## Command-line tools

These run in a terminal on the server; the app does not use them.

`stagent run [--detached | --handoff[=auto]] [--id ID] [--cols N --rows N]
[--cwd DIR] -- <cmd> [args...]` runs `<cmd>` as a session. Without
`--detached` it is mirrored on the terminal it was started in
(passthrough), which owns the size; when that terminal hangs up the
program gets SIGHUP. With `--handoff` (`persist`; passthrough only, a usage
error with `--detached`) the hangup — SIGHUP to `stagent run`, or its
terminal input failing with EOF/EIO — does not reach the program:
mirroring stops, the session's `mode` becomes `detached` (`holder.update`,
so watchers get `session.updated`), the holder starts answering terminal
queries, the local size is no longer followed and `session.resize` works
without `force`. Later hangups are ignored. A handed-off session stays
where `stagent run` started (see below). On Windows a closing console
still ends the process, so a handoff only happens when console input
reaches EOF.

The local terminal holds the program back, as any terminal does, only
while it takes output. A write to it that makes no progress for 2 s (a
Windows console with a selection, an SSH client gone without a hangup)
counts as a terminal that stopped reading: output to it is dropped, so
attached clients keep getting the program's output and the program does
not block, and once it reads again it is redrawn with a snapshot of the
screen. Lines that scrolled by meanwhile are missing from its own
scrollback (`session.scrollback` has them).

A `--detached` holder writes its diagnostics to stderr. When stderr is a
pipe or socket (`stagent run --detached` run over an SSH exec channel),
the holder points it at `log/<id>.log` once the program runs, after one
line naming that file: the reader goes away with the connection, and a
later diagnostic would otherwise end the holder with SIGPIPE (Windows has
none and keeps stderr). Errors before the program runs still reach the
original stderr.

`--handoff=auto` (also passthrough only) decides at every start, in this
order: the environment variable `STAGENT_HANDOFF` when it is exactly `0`
(no handoff) or `1` (handoff) — any other value is ignored; else
`config.json`'s `disable_handoff` (true: no handoff); else handoff. Plain
`--handoff` always hands off and no flag never does, whatever the
environment and config say. Each `--handoff=auto` start is recorded for
`doctor` (`shell_wrapper.last_run_at`, `last_run_survives_logout`,
`last_run_bootstrap_error`).

Leaving the login session (Linux): before it starts anything (the daemon,
the program), `stagent run` — and likewise `stagent daemon` — moves itself
into a new transient scope of the user's systemd service manager, with the
D-Bus call `systemd-run --user --scope` makes (`StartTransientUnit` of
`stagent-run-<pid>-<random>.scope` / `stagent-daemon-…` with its own pid),
and waits until `/proc/self/cgroup` has changed. It does so only when
logind's `KillUserProcesses` is yes (a login session's processes are killed
when it ends) or lingering is on (the service manager outlives every
logout); with `KillUserProcesses=no` and no lingering the login session
outlives the last logout while the service manager does not, so it stays.
A process already in a service unit (the daemon of `integrate --service`)
stays too. If the call fails the process runs on where it is. Everything
it starts inherits the scope. A wrapper start then records where it runs
from its cgroup — a login session's scope, under the service manager, or
outside the user's slice — and `doctor` judges from that and logind's
settings at the time it runs whether the start outlives logout: in a login
session's scope unless `KillUserProcesses` is yes, under the service
manager when lingering is on, and outside the user's slice always.

Leaving the GUI session's bootstrap namespace (macOS): a process started
from a terminal of the GUI login session keeps running after the user logs
out of the GUI, but the session's Mach bootstrap port it inherited no
longer reaches services such as configd, so programs it starts later
cannot resolve host names. So before it starts anything, `stagent run`
(passthrough, `--handoff`, `--detached`) and `stagent daemon` make the
three calls tmux makes (`bootstrap_get_root`,
`bootstrap_look_up_per_user` for the user, `task_set_bootstrap_port`) and
update libSystem's `bootstrap_port`, always. Everything they start
inherits the per-user port: the agent, its tools, hooks and a daemon a
holder starts. While the user is logged in the keychain stays reachable.
If a call fails the process runs on with the port it had: a detached
holder writes the reason to its log, the daemon to `daemon.log`, a wrapper
start records it (`last_run_bootstrap_error`) and the daemon reports it
(`daemon.bootstrap_error`). The calls go through `ebitengine/purego`, so
the binary stays `CGO_ENABLED=0`.

Starting in the GUI login session (macOS): the login keychain is unlocked
only for processes of the GUI login session — what counts is the audit
session, not the bootstrap port — so an agent started from an SSH session
(or a `user/<uid>` job) finds it locked, and claude reports itself logged
out. So while the user is logged in to the GUI, the bridge starts a
`session.spawn` holder from a one-off launchd job it loads into
`gui/<uid>` (label `com.obutora.stagent.spawn-<random>`, the plist and a
spec with the arguments, directory and environment written next to the
holder's log and deleted again). The job runs `stagent spawn-task <spec>`,
which starts the holder detached — outside the job, which the GUI logout
stops — and removes the job. The holder still swaps to the per-user
bootstrap port as above. When `launchctl bootstrap gui/<uid>` fails (no
GUI login), the holder is started directly, without the keychain, and its
log says why. claude then asks to log in; a `/login` there is saved to
`~/.claude/.credentials.json` (claude's fallback when it cannot write the
keychain), which later sessions started outside the GUI session read.

In a terminal (not `--detached`), just before the program starts, `stagent
run` prints one line to stderr when the daemon holds agent sessions on the
host — harness other than `other`, not ended, `mode` `detached` and no
client attached — naming their number and `stagent ls`, e.g. ``stagent: 2
agent sessions are held on this host; `stagent ls` lists them``. It prints
this whatever the handoff choice, and nothing when there are none or the
daemon does not answer within a fraction of a second (it is not started
for this).

A Codex CLI 0.156 or later (`codex --version`, asked with the session's
environment and working directory, 5 s at most) is started with
`--no-daemon` added right after the program, unless the command already has
it or is one Codex refuses it with (`codex agents`, `--remote`). From 0.157
Codex otherwise runs its threads in one shared `codex app-server
--managed-daemon` per `CODEX_HOME`, which keeps the environment of the codex
that started it, so the hooks of every later session would carry that
session's `STAGENT_SESSION_ID`. The session's `command` stays as given.

The shell wrappers (`integrate --shell-wrapper`; bash/zsh block, fish file,
PowerShell profile block) define functions `claude`, `codex` and `omp`
that run the real command as `stagent run --handoff=auto -- <name>
<args…>` when the shell is interactive, stdin and stdout are terminals, the
shell is not already inside a stagent session (`STAGENT_SESSION_ID` unset)
and the stagent binary exists. Non-interactive invocations run the real
command directly: claude with `-p` / `--print` anywhere in its arguments,
codex whose first argument is `exec` / `e`, omp with `-p` / `--print` or a
`--mode` (`--mode X` or `--mode=X`) other than `text`. The block also
exports `STAGENT_SHELL_WRAPPER=1`, marking shells that have the wrapper.
The wrapper itself does not look at `STAGENT_HANDOFF`; `stagent run
--handoff=auto` does.

`stagent ls [--json]` lists the daemon's sessions (`sessions.list`), most
recent `last_activity_at` first: a table of ID, MODE, STATE, LAST ACTIVITY
(relative), PC INPUT (`last_local_input_at`, relative; `-` when unset),
COMMAND and TITLE/CWD (the title the program set, else the working
directory), or with `--json` the `{sessions[]}` result on one line.
Without a running daemon the list is empty and the exit status 0.

`stagent attach [ID | --last] [--detach-key ctrl-X]` attaches the terminal
it runs in (stdin must be a terminal) to a session, talking to the holder
directly: the terminal goes into raw mode, the session is resized to the
terminal's size (`force`, again on every SIGWINCH), and a raw attach
(without `since`) streams the snapshot and then the output to stdout while
keystrokes go to `session.input` as `text` (bytes that are not UTF-8 arrive
as U+FFFD). Replies of the local terminal to the program's queries are
filtered out of the keystrokes — CPR `ESC [ r ; c R` (and `ESC [ ? r ; c
R`), DA `ESC [ ? … c` / `ESC [ > … c`, DECRPM `ESC [ ? … $ y`, OSC
10/11/12/4 colour replies and XTVERSION `ESC P > | … ESC \`; keys, mouse
reports and focus reports (`ESC [ I` / `ESC [ O`) pass — because the holder
answers queries itself. xterm's modified F3 (`ESC [ 1 ; m R`) cannot be told
from a CPR and is dropped too. With an ID the holder is asked directly, so
no daemon is needed. Without one it attaches the only live session and
fails, listing them, when there are none or several; `--last` takes the
live session with the newest `last_activity_at`. Neither ever picks the
session the command itself runs in (`STAGENT_SESSION_ID`), and naming it is
an error. The detach key (default Ctrl-], `--detach-key ctrl-X` for X in
`@`, `a`–`z`, `\`, `]`, `^`, `_`; not Ctrl-[, which is ESC) detaches and
prints `[detached]` once no second press follows within 300 ms; pressing it
twice in a row sends it to the program once. Every exit resets the input
and screen modes the program may have left on and restores the terminal.
Exit status: 0 detached; N when the session ends (`[session ended (exit
N)]`); 1 when the holder is unreachable or the connection is lost
(`[connection to session lost]`); 128+signal on SIGTERM / SIGHUP / SIGINT;
2 for usage errors, no terminal or no session to choose. Not available on
Windows.

## Harness hooks

Installed hook commands call `stagent hook <harness>` with the harness's
hook payload on stdin (`hook_event_name` inside the payload names the
event). Codex's `notify` program is `stagent hook codex notify` and gets the
JSON as its last argument. The omp extension runs `stagent hook omp` with
`{hook_event_name, session_id, transcript_path, cwd, message?}` on stdin,
using Claude's event names (`SessionStart`, `UserPromptSubmit`, `Stop`,
`SessionEnd`). A hook never fails the harness: when the daemon is
unreachable it exits 0 without output.
A hook run with `CLAUDE_CODE_BRIDGE_SESSION_ID` set (claude connected to
Remote Control) tells the daemon so (`notify.skip_when_claude_app_notifies`).
On Windows the commands run `stagent.exe` by its absolute path with
forward slashes. Claude Code runs them through Git Bash or cmd, so its
command quotes the path (`"C:/…/stagent.exe" hook claude`). Codex runs
them with the session's shell, PowerShell on Windows, where a quoted path
followed by arguments does not parse: its command leaves the path bare
(`C:/…/stagent.exe hook codex`, valid in every shell), or uses the call
operator (`& 'C:/…/stagent.exe' hook codex`) when the path holds a space
or another character PowerShell reads specially.

### Agents without a stagent session

Outside a stagent session (`STAGENT_SESSION_ID` unset) `stagent hook` also
tells the daemon how the agent was started (`hook.event` params, local IPC
only):

| param | value |
|---|---|
| `shell_wrapper` | `STAGENT_SHELL_WRAPPER` is set: the shell has the wrapper's rc block |
| `entrypoint` | `CLAUDE_CODE_ENTRYPOINT` (Claude Code: `cli`, `sdk-cli`, `sdk-ts`, `claude-vscode`, …) |
| `parent_tty` | the harness — the hook's nearest ancestor that is not a shell (the installed Codex command keeps `sh -c` in between) — reads a terminal: its stdin (`/proc/<pid>/fd/0`) on Linux, its controlling terminal (as `ps -o tty=`) on macOS; absent when unknown (Windows) |
| `parent_batch` | the harness's command line asks for a non-interactive run by the shell wrapper's rule (`claude -p` / `--print`; `codex exec` / `e` as the first argument; `omp -p` / `--print` or `--mode` other than `text`) |
| `ssh` | `SSH_CONNECTION` is set |
| `terminal_ancestor` | (Windows; absent elsewhere or when unknown) a shell — `powershell`, `pwsh`, `cmd`, `bash` or `sh` — runs above the harness, the hook's nearest ancestor that is not one of them (Claude Code runs hooks through Git Bash) |
| `originator`, `subagent` | (codex) `originator` and whether `source` is a `subagent` thread, from the `session_meta` line that starts the conversation's rollout file: the payload's `transcript_path`, else `$CODEX_HOME/sessions/*/*/*/rollout-*-<thread-id>.jsonl` for the notify program. Seen (Codex 0.160): `codex-tui` (the terminal UI), `codex_exec` (`codex exec`), `Codex Desktop` (the desktop app) |

`parent_batch` covers what the terminal cannot tell: `codex exec` or `omp
-p` typed in a terminal read it like an interactive start.

A hook the daemon finds no session for is recorded per conversation (the
payload's `session_id`; none without it) as an `UnwrappedLaunch`, in memory
only, and the watchers get `unwrapped.updated`:

| hook | `reason` |
|---|---|
| codex `subagent`, or an `originator` other than `codex-tui` / `codex_cli_rs` (terminal UI) and `Codex Desktop` / `codex_vscode` | not recorded (non-interactive: `codex exec`, the SDK, another conversation's thread) |
| codex `originator` `Codex Desktop` / `codex_vscode` | `ide` |
| Claude Code `entrypoint` `sdk-*` | not recorded (non-interactive) |
| Claude Code `entrypoint` other than `cli` | `ide` |
| codex / omp (and Claude Code without `entrypoint`) with `parent_tty` false or `parent_batch` | not recorded (non-interactive) |
| otherwise, on Windows with `ssh` | `ssh` |
| otherwise, `shell_wrapper` set | `bypassed` |
| otherwise, on Windows with `terminal_ancestor` false | `no_terminal` |
| otherwise | `old_terminal` |

Windows never knows the terminal (`parent_tty` is absent), so every other
launch counts; `ssh` and `terminal_ancestor` take its place there. A codex
without `originator` (an older Codex, an unreadable rollout file) is
judged like before.

Every hook of a recorded conversation updates `last_activity_at` (watchers
get it at most once a minute unless something else changed). The record
goes away at the conversation's `SessionEnd` (Claude Code; omp's
`session_shutdown`), 12 hours after its last hook (Codex sends no end), when
a stagent session carries the conversation, and with the daemon; nothing is
written to disk. Recording changes nothing else about the hook.
