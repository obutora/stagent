# stagent changelog

Each release's section is its GitHub release notes (`scripts/release.sh`
publishes it with the binaries).

## 0.9.0

### Added

- The GitHub screen works on native Windows hosts, which have no `sh` for
  the app's scripts: on Windows the bridge announces `github` and serves
  `github.repos` (the git working trees the user worked in last, newest
  first, with the fields of the app's sh script; candidates are the
  working directories of the user's processes under their SSH connections
  and holders, read from the PEB, the sessions' `cwd` and the latest 50
  conversations' `cwd`; at most 20 are inspected) and `github.activity`
  (gh's open pull requests and issues, the branch's pull request, the
  repository's description; `gh: missing` with `package_manager: winget`
  or `unauthenticated`). Both run git and gh on their own queue, so the
  daemon and spawns never wait for them, and answer agent descendants
  too. Linux and macOS bridges neither announce nor serve them. A bare
  PowerShell a holder starts (a `shell: true` session of a host whose
  sshd runs PowerShell) gets a prompt wrapper that keeps the process's
  working directory at the shell's location, so `Set-Location` / `sl` /
  `cd..` move the repository the GitHub screen sees; the session keeps
  its command as given. The shell wrapper's PowerShell profile block has
  the same prompt wrapper, for the terminals sshd starts in PowerShell
  (an update rewrites the block on hosts that have it) (#502).

## 0.8.0

### Added

- Pushes can go to Discord, Slack and Telegram: `notify.chat {enabled,
  url}`, a chat destination next to the generic webhook (both can be on
  and both get every push). The one-line `url` names the service — a
  Discord webhook (`https://discord.com/api/webhooks/<id>/<token>`,
  `discord://<id>/<token>`), a Slack Incoming Webhook
  (`https://hooks.slack.com/services/…`, `slack://<a>/<b>/<c>`) or a
  Telegram bot and chat (`tgram://<bot token>/<chat id>`) — and stagent
  builds each service's message itself: the reason's emoji, the
  host-labelled title and the body, as a Discord embed (yellow for a
  warning, no mentions), Slack text (`&<>` escaped, no unfurling) or
  Telegram plain text (no link preview). `config.set` refuses any other
  `url`, Slack Workflow webhooks included, with `bad_request`. The `url`
  is a secret: it never shows in `last_error` (channel `chat`),
  `notify.test` errors, `stagent doctor` or the log. The bridge announces
  `push_chat` (#471).
- `notify.test {channels?: ["ntfy" | "webhook" | "chat"]}` tests only the
  channels named; without `channels`, every enabled one as before. An
  unknown name is `bad_request`, named channels that are all off
  `not_configured` (#471).
- `notify.click_page`: the open page (`https://sshterm.iru-yo.com/open`,
  set by the app) that chat destinations and the generic webhook link to,
  with `h=<host_id>` and `s=<session id>` in its fragment
  (`<click_page>#h=…&s=…`). A push of one session gets an "Open in SSH
  Term" link (in `notify.lang`): a link button on Discord
  (`?with_components=true`) and Telegram (inline keyboard), a link at the
  end of the text on Slack; a digest, `notify.test` and a push of no
  session get none. The generic webhook's JSON gains `click_url` (the
  host only, `#h=…`, for a digest and `notify.test`); its other keys stay.
  No `click_page`, no link. ntfy's `Click` still uses `notify.click_base`.
  `config.set` refuses a `click_page` that is not an absolute https URL
  without query and fragment with `bad_request` (#472).
- On Discord and Telegram, the chat destination replaces and resolves
  like ntfy: the next message of a session is sent first, then the
  older one deleted (Discord `DELETE …/messages/<id>`, Telegram
  `deleteMessage`; one Telegram no longer deletes after 48 hours is
  marked resolved instead), and when the session settles its message is
  edited, not deleted: ☑️ in place of the reason's emoji, "(resolved)"
  after the title in `notify.lang` (ja `（解決済み）`), the body as it
  was and the same "Open in SSH Term" button (Discord: grey embed,
  components sent again; Telegram: `editMessageText` with `reply_markup`
  sent again). A digest's message is marked resolved once all its
  sessions have settled and is never deleted by a later message of one
  of them. The unresolved messages survive a daemon restart in
  `state/chat-messages.json` (mode 0600: ids, sent time and the SHA-256
  of `notify.chat.url`, never the URL or its token); a session gone over
  the restart has its message marked resolved at once. Changing
  `notify.chat.url` or removing `chat` forgets the messages of the old
  URL without editing them. A failed delete or edit is never retried nor
  a failed push (`last_error`): a 404 lets the message go, a 429 or a
  connection error lets it go with one log line. Slack messages are only
  sent, and the generic webhook still gets no clear (#473).
- A failed push to the chat destination says why: `last_error.chat` gains
  `kind` (`revoked`, `unreachable`, `rate_limited`, `rejected`, `server`,
  `network`; Telegram by `error_code`) and its `error` adds the service's
  short reason after the status line (`404 Not Found: Unknown Webhook
  (10015)`; Discord's `message` and code, Telegram's `description`,
  Slack's error body; 200 characters at most, dropped whole if it holds
  the URL or a token). A queued push answered 429 with a wait of 5 s or
  less is sent once more after the wait (`notify.test` is not); only a
  failed resend is recorded. Pushes keep going after a failure, and the
  log shows only the first of a run of the same kind. `config.set`
  changing `notify.chat.url` removes `last_error.chat`; `stagent doctor`
  names the kind and, for `revoked`, says to set a new URL from the app
  (#474).

## 0.7.2

### Fixed

- A chat message whose turn to be written comes after an approval menu
  came up is no longer typed into the menu. The holder looked for a menu
  only when `session.input` arrived, then wrote the message's text from
  its input queue without looking again, so a menu shown in between could
  take the text's first characters as its keys. The queue now looks again
  right before writing the text and drops the whole message (text and
  Enter) if a menu is up; `session.input` answers a chat message only once
  its text is written, and with `menu_open` when it was dropped, so the
  app sends it again after the menu as it does for one refused on arrival
  (#423).
- A chat message sent while Codex shows its update notice or folder trust
  prompt at startup is no longer typed into it. The holder refused chat
  messages only on approval menus (`1. Yes…`), so the message's Enter
  picked `1. Update now` (Codex updated itself) or `1. Trust and continue`
  (the folder was trusted). `session.input` with `paste` and `submit` now
  fails with `menu_open` while any menu is on the screen — numbered
  options with one under the cursor and a key hint below them, as the app
  reads menus — and an Enter that would land on one is held as for an
  approval (#440).
- A numbered prompt sent to Codex no longer reads as a menu while Codex
  works on it. Codex's status line (`• Working (3s • esc to interrupt)`)
  blinks its bullet to `◦`, which the holder did not take for a
  transcript entry, so the line's `esc` read as a menu's key hint under
  the prompt's numbered lines and chat messages were refused with
  `menu_open` (#440).
- Windows: a `\\.\pipe\stagent-<SID>` another user created first with a
  DACL that refuses the user is now reported with its owner — `blocked`
  in `hello` and `stagent doctor`, `foreign_owner` for requests — instead
  of failing as a plain access denied. stagent reads the owner by name
  (`READ_CONTROL` only); a pipe that refuses that too still fails as
  before, and one the user owns, or `BUILTIN\Administrators` owns (an
  administrator's elevated daemon dialed unelevated), keeps its access
  denied (#422).

## 0.7.1

### Security

- On macOS, stagent now also refuses connections from processes in a
  sandbox (Seatbelt), as Claude Code and Codex run their commands. The
  macOS sandbox has no PID namespace, so a sandboxed command allowed to
  reach `~/.ssh-term/agent/run` (`sandbox.network.allowUnixSockets`)
  could double fork (or `setsid`) out of the agent's process tree, and
  so past the parent walk of 0.7.0, while staying sandboxed. Such a
  connection, and one handed to such a child, now gets `agent_refused`
  as a holder or the daemon answers an agent's; hooks and reads are
  still served. stagent asks `sandbox_check`, which Apple does not
  document; if a macOS lacks it or it answers otherwise, this check is
  left out and the parent walk alone decides, as in 0.7.0 (ADR 0004,
  #415).

## 0.7.0

### Fixed

- A Codex approval answered with Esc no longer keeps the session in
  `needs_approval` (and in `approvals.list`) until the next turn ends.
  Codex ends the turn without Stop on Esc, and blinks its terminal title
  while the prompt is up, so the output never went quiet for the answer
  to show as new activity. The holder now watches for Codex's approval
  menu as it does for claude's (`holder.prompt_watch`): the approval
  closes as `cancelled` once the menu leaves the screen, however it was
  answered — Esc, Yes, on the PC or from SSH Term (#320).
- A chat message sent from the app no longer answers an approval menu.
  The app checked its own copy of the screen, which lags behind the
  host's, and the holder wrote the message's Enter without looking, so a
  menu that came up meanwhile took it as `1. Yes`. `session.input` with
  `paste` and `submit` now fails with the new error `menu_open`, writing
  nothing, while claude's or Codex's approval menu is on the screen; a
  menu that shows up between the paste and the Enter holds the Enter until
  the menu has been gone for 400 ms, and input typed after it went drops
  the Enter, leaving the message in the input box. Answers to menus
  (`text`, `keys`) are never held (#402).
- `stagent follow` on Linux with the Codex or omp sessions directory
  behind a symlink (a symlinked `~/.codex` or `CODEX_HOME`, or a home
  under `/home -> /var/home`): a fresh agent no longer shows a
  transcript another agent has open. The open file's path, which Linux
  gives with symlinks resolved, now comes back spelled under the
  sessions directory, as the directory listing spells it, so the other
  agent's claim on it matches (#406).

### Security

- With the home on a network file system, scrollback lives in
  `/var/tmp/stagent-<uid>/sessions`; stagent now refuses that parent
  directory unless it is a real directory (not a symlink) owned by the
  user and closed to others, as it does for the run directory in `/tmp`.
  Another user who created it first could otherwise swap `sessions` to
  read or forge scrollback. Segment files are also opened with
  `O_NOFOLLOW`, so a planted symlink can no longer make the holder
  truncate and overwrite the file it points to (#405).
- A pasted chat message can no longer leave bracketed paste and be typed
  as keys: stagent removed `ESC[201~` from pastes only once, so
  `ESC[20ESC[201~1~` (e.g. copied from a malicious page) became the end
  marker. It is now removed until none is left (#404).
- stagent no longer talks to a daemon or session that another user
  serves. The daemon already refused to start in a `/tmp/stagent-<uid>`
  someone else created first, but hooks, the bridge, `stagent follow`,
  `attach`, `ls` and `install` still connected to whatever socket was
  there, as they did on Windows to a `\\.\pipe\stagent-<SID>` another user
  created. Every connection now checks the owner of the socket (the
  server's uid) or pipe (its owner SID; `BUILTIN\Administrators`, the
  owner of an elevated administrator's daemon, counts as the user's own)
  and refuses another user's. Such a blocked location is reported with its
  owner: as `blocked` in `hello` and `stagent doctor`, and as the new
  error `foreign_owner` for requests that need it (#403).
- stagent now refuses connections from coding agents' process trees
  (Linux, macOS). A holder refuses every connection from a process that
  claude, codex or omp started, found by walking the connecting process's
  parents, and the daemon refuses them `config.set`, `presence.set`,
  `notify.test` and `daemon.shutdown`; hooks, holders and reads are still
  served. An agent whose sandboxed commands run without asking could
  otherwise type approvals or "user instructions" into its own session
  with `session.input`, or commands into another kept shell outside its
  sandbox. A connection whose process already exited, or whose parents
  cannot be followed, is refused too. A process that leaves its parents
  (a double fork, such as `setsid -f`) escapes the walk, so this holds agents
  sandboxed in their own PID namespace (bubblewrap), not macOS sandboxes.
  The new error `agent_refused` says why, and `stagent attach` shows it;
  `stagent install` run by an agent no longer replaces the daemon (nor
  kills it), so run it in your own terminal, and `stagent bridge` run by
  one answers `session.spawn` with it instead of starting a holder it
  could not reach. Locations set with `STAGENT_HOME` and Windows are not
  checked (ADR 0004, #401).
- Sessions started before the update keep running their old holder, which
  has none of the above (no `agent_refused`, no `menu_open`), until they
  are opened again; nothing restarts them.

## 0.6.0

### Added

- WSL を動かし続ける: `integrate --wsl-keep-running` (WSL) sets
  `[general] instanceIdleTimeout=-1` in the Windows user's `.wslconfig`
  (found through interop, written through `/mnt/c`), so WSL no longer
  shuts the distribution down — ending detached sessions and kept shells —
  some 30 s after the last SSH connection or Windows terminal using it
  closes. One line changes and every other byte is kept; the edit is
  recorded in the manifest. `--remove wsl-keep-running` and `uninstall
  --level purge` undo only that edit (restoring the original file, or
  taking out just stagent's line when the file changed since); a line the
  user wrote is never touched. A Windows profile that cannot be reached is
  `error_code` `wslconfig_unreachable`. Either direction notes
  `wsl_restart`: WSL reads the file when it starts (PROTOCOL.md).
- `stagent wsl-shutdown --json` (WSL) runs `wsl.exe --shutdown` so the
  setting applies now; it stops every distribution, this command included.
- `doctor` reports `persistence.wsl` on WSL: the distribution,
  `.wslconfig`'s path, `instance_idle_timeout`, `networking_mode`,
  `keep_running_needed` (with a `problems` line when WSL stops the
  distribution when idle) and `keep_running_by_stagent`.

### Changed

- `doctor` reports `persistence.linger_needed: false` on WSL: WSL keeps
  the user's login session for as long as the distribution runs, so
  lingering changes nothing there.

### Fixed

- Codex 0.157 or later: the hooks of every stagent session reach their own
  session. Codex otherwise runs its threads in one shared `codex app-server
  --managed-daemon` that keeps the environment of the codex that started
  it, so later sessions' hooks carried the first session's
  `STAGENT_SESSION_ID` and their states showed on that session. The holder
  starts a Codex CLI 0.156 or later with `--no-daemon` (not `codex agents`,
  `--remote`, or a command that already has it) (#319).

## 0.5.0

### Added

- Kept shells on Windows: `session.spawn {shell: true}` no longer fails
  with `unsupported`. It runs `SHELL` as sshd sets it (its
  `DefaultShell`; cmd.exe when that is unset), or `%ComSpec%` when
  `SHELL` is empty, without `-l` (Windows PowerShell 5.1 exits on it).
  The Windows `hello` adds the capability `persist_shell`; Linux and macOS
  keep relying on `persist` (PROTOCOL.md).
- `doctor` reports, on Windows, each installed PowerShell (5.1, and 7
  when `pwsh` is on `PATH`) with its effective execution policy
  (`powershell[]`: `execution_policy`, `loads_profile`), and whether it
  runs with RedirectionGuard (`redirection_guard`), which every process
  started over SSH inherits from sshd: tools reached through a junction a
  user made (scoop's shims) fail in kept shells. A PowerShell that does
  not run its profile (`Restricted`, `AllSigned`) adds a `problems` line.
- `integrate --execution-policy` (Windows) sets `RemoteSigned` for the
  current user in each PowerShell that does not run its profile, and only
  there. Group Policy keeping the old policy fails the change with
  PowerShell's own message (`error_code` `execution_policy_overridden`).
- `integrate --shell-wrapper` notes `cmd_not_wrapped` on Windows when
  sshd's default shell is cmd: the wrapper goes into the PowerShell
  profiles only. `login_shell` is that shell (`cmd.exe`).
- Agents started without a stagent session on Windows get their own
  reasons: `ssh` for one started in an SSH session (the PowerShell
  wrapper does not wrap there and the agent ends with the connection), and
  `no_terminal` for one without the wrapper's marker and with no shell
  (PowerShell, pwsh, cmd, bash) above it, i.e. started by a program
  rather than a terminal. The hook sends `ssh` and, on Windows,
  `terminal_ancestor` to the daemon.

### Changed

- PROTOCOL.md describes `terminate` and `hangup` on Windows as the code
  does them: ^C, then `TerminateProcess(…, 1)` after 3 seconds (no
  Ctrl-Break). A child with a console of its own (`start ""`) outlives a
  hung-up shell.
- Codex conversations are told apart by the `originator` of their rollout
  file (all OSes): the terminal UI (`codex-tui`) is judged as before, the
  desktop app (`Codex Desktop`) and the IDE extension count as `ide`, and
  `codex exec`, SDK runs and subagent threads are not recorded. The notify
  program, which gets no `transcript_path`, finds the file by its
  `thread-id` under `$CODEX_HOME/sessions`.
- Windows: Codex is integrated through its hooks, as elsewhere, instead of
  the notify program (Codex runs hooks on Windows now, and the Codex app
  may already use `notify` itself, which stagent never replaces). The
  hook command leaves the path bare, or uses PowerShell's call operator,
  because Codex runs it with PowerShell; the note `codex_notify_fallback`
  no longer has the reason `windows`.

### Fixed

- Windows: the `esc` key (and a lone Esc in `text`) of `session.input`
  reaches Claude Code. Behind a pseudo console in win32-input-mode it is
  now sent as that mode's Esc key event; Claude Code ignored the bare
  byte, so the app's Esc button could not cancel its prompts.
- Windows: a `submit` after `text`/`paste` waits 250 ms instead of 30 ms.
  Codex gets no bracketed paste there and took the Enter as a line break
  of the pasted text, so chat prompts stayed unsent in its input box.
- Output that only sets or resets DEC private modes (`CSI ? … h/l`) no
  longer counts as activity: Oh My Pi on Windows re-enables bracketed
  paste every second, which kept its session `working` while it waited.
- Removing the Codex integration takes out the trust tables Codex wrote
  for stagent's hooks together with the blank line before each, so
  `config.toml` reads as it did before (one blank line was left per
  table).
- Windows: closing the console window of a passthrough session (the
  PowerShell wrapper's `stagent run`) counts as a hangup: `session_ended`
  carries `hung_up` and the exit (code 1) is no abnormal one, so it is no
  longer pushed. An agent that exits with an error on its own still is.
- Codex integrated through the notify program (it had no hooks then)
  switches to hooks once it has them: `doctor` reports such a Codex as not
  `integrated`, and `integrate --harness codex` removes stagent's notify
  line while it adds the hooks, so a turn is no longer reported twice.
  Removing the integration does not put the line back; a notify program
  of the user's stays as it is.
- A passthrough session (the shell wrappers' `stagent run`) whose
  terminal stops reading without hanging up — a Windows console with a
  selection, an SSH client gone silently — no longer stops its output
  reaching the app. Before, the app's terminal froze while the program
  went on (its conversation still reached the chat), the frozen screen
  kept the session `working`, and the program blocked on output and
  stopped reading input. A write to such a terminal that makes no
  progress for 2 s now drops its output; once it reads again it is
  redrawn from the screen (#281).
- Windows: `stagent bridge` exits when the SSH session process that runs
  it (`sshd -z`) ends, even if its stdin never reaches EOF. Killing that
  process left the connection's bridge running (about 12 MB each) with no
  app (#270).
- Windows: `doctor` and `integrate --execution-policy` read Windows
  PowerShell's execution policy when stagent runs under PowerShell 7 (sshd's
  default shell pwsh). The `PSModulePath` PowerShell 7 hands down made
  Windows PowerShell load PowerShell 7's `Get-ExecutionPolicy`, which fails
  there, so a RemoteSigned 5.1 was reported as Restricted (`problems` and
  the app's status line) (#177).

## 0.4.2

### Fixed

- Removing integrations (`integrate --remove`, `uninstall --level unhook`)
  leaves the home as it was before them: the directories stagent created
  for a file it then deletes go too once empty (`~/.omp/agent/extensions`,
  `~/.omp`), and a `<file>.sshterm-bak-<time>` backup is deleted once its
  content is restored. A backup of a file changed since stagent's edit
  stays until `uninstall --level purge` (PROTOCOL.md).
- A holder started with `stagent run --detached` over an SSH exec channel
  (stderr a pipe) no longer dies of SIGPIPE when it logs after the
  connection closed — when `install` replaces the daemon, say. Once the
  program runs, its stderr goes to `log/<id>.log`, like a holder the app
  starts.
- macOS: claude started in a session from the app (`session.spawn`) is
  logged in while you are logged in to the GUI. The login keychain is
  locked for SSH sessions, so the bridge now starts the holder through a
  one-off launchd job in the GUI login session (`gui/<uid>`); without a
  GUI login it starts as before, and a `/login` there is kept in
  `~/.claude/.credentials.json` (PROTOCOL.md).

## 0.4.1

### Changed

- `install` and `integrate` `notes[]` are objects `{code, text, args?}`
  instead of strings: `text` is the English remark, `code` (with `args`)
  lets the app word it in its own language (codes in PROTOCOL.md). Apps
  that read the objects need a host of 0.4.1 or later.

### Fixed

- `doctor` judges whether the last wrapper start survives logout with
  logind's current `KillUserProcesses` and lingering: `stagent run`
  records where it ended up (`placement`), so `linger_needed` is true
  again after `integrate --remove linger`.

## 0.4.0

The bridge protocol stays 1: 0.3.0 apps keep working with a 0.4.0 host.

### Changed

- **Push notifications no longer include a normal exit or a program's own
  terminal notification by default.** Pushed by default: `needs_approval`,
  `waiting_input`, `turn_complete` and abnormal `exited` (an exit code other
  than 0, or a lost session). A normal `exited` and `terminal` (OSC
  9/99/777, BEL) stay in-app `notification` events; `notify.reasons`
  brings them back (`"exited": "all"`, `"terminal": true`).
- Hooks of programs not started through stagent (an IDE, `claude -p`, the
  SDK) notify in-app only; they are no longer pushed, and their approvals
  are neither held nor registered.
- Approvals: only claude running in a stagent session holds its
  PermissionRequest hook, with no time limit. claude keeps the hook after
  the prompt is answered (until the approved tool finished), so the
  session's holder watches its screen: once claude's permission menu has
  left it — answered anywhere (the PC, Remote Control, SSH Term) — the
  approval closes as `cancelled`, the hook returns, its `needs_approval`
  goes and its push is cleared. In sessions whose holder still runs 0.3.0
  (held from before the update) the approval closes only when claude ends
  the hook or at Stop. Codex's hook returns at once; its approval closes
  on output or Stop.
- `config.set` applies a JSON Merge Patch (RFC 7396) and stores only the
  values it names; unknown keys are kept.
- `notify.test` answers `not_configured` (and sends nothing) when no
  channel is enabled.
- `stagent run --handoff=auto` (what the shell wrappers run) keeps the
  program running detached when its terminal closes, unless
  `STAGENT_HANDOFF=0` or `disable_handoff`. Before it starts, a terminal
  session prints how many agent sessions are held on the host.
- Shell wrappers (bash/zsh, fish, PowerShell) always use `--handoff=auto`,
  export `STAGENT_SHELL_WRAPPER=1` and leave non-interactive runs
  (`claude -p`, `codex exec`, `omp -p`) unwrapped; bash's block also goes
  into an existing `~/.bash_profile`. PowerShell builds `$bin` from
  `$env:USERPROFILE` (ASCII).
- `stagent install` finishes an update: it rewrites existing wrapper blocks
  once and replaces a daemon of another version (`replaced_daemon`);
  held sessions re-register and stay listed.
- A shell session promoted to the agent its hooks report returns to
  harness `other` once that agent is gone.
- A session the app ended with `session.signal hangup` is not an abnormal
  exit, on every OS: the holder records the hangup (`holder.ended` and
  `session_ended` carry `hung_up`).
- Linux: `stagent run` and `stagent daemon` move into a scope of the user's
  systemd when `KillUserProcesses=yes` or lingering is on, so agents survive
  logging out; the bridge no longer starts holders through `systemd-run`.
- macOS: `stagent run` and `stagent daemon` move to the user's per-user
  bootstrap port, so what they start keeps the network, keychain and DNS
  after a GUI logout. `integrate --service` installs into `user/<uid>`.

### Added

- `notify.reasons` (`needs_approval`, `waiting_input`, `turn_complete`,
  `terminal`: booleans; `exited`: `off` / `error` / `all`), `notify.lang`
  (`en` / `ja` / `ko` / `zh`: the fixed phrases and digest titles) and
  `notify.skip_when_claude_app_notifies` (no pushes for sessions whose
  hooks run with `CLAUDE_CODE_BRIDGE_SESSION_ID`).
- Presence: no push while someone is at the session — a keystroke at its
  terminal on the host within 60 s, or within 10 min while that terminal
  has the focus (`Session.focused`, from focus reports), or the holder's
  `$CLAUDE_CLIENT_PRESENCE_FILE` (`Session.presence_file`) exists. A
  passthrough session's push waits 15 s and is dropped when the session
  settles meanwhile.
- `presence.set {foreground}`: no push while an app watching the host is in
  the foreground; the bridge restores it after a daemon restart.
- A session's ntfy notification is replaced by its next one (sequence ID)
  and cleared (`PUT <topic>/<sequence id>/clear`) once the session
  settles: its approval closed, it went back to working, it ended. Servers
  without sequence IDs or clear (ntfy before 2.16) get plain notifications
  and one log line.
- `notify.host_label` titles every push with the host
  (`<label> · <harness> · <project>`, digests `<label>: …`).
- `notify.last_error`: the last failed push per channel, kept across daemon
  restarts, in `config.get` / `config.set` and `stagent doctor`.
- `host_id` (random, in `hello` and `doctor`) and `notify.click_base`: ntfy
  pushes link to `click_base` with `h=<host_id>` and `s=<session id>`.
  `NotificationData.session_id`.
- `Session.attached` (a client is attached) and
  `Session.last_local_input_at` (last keystroke at a terminal on the host:
  its own passthrough terminal or `stagent attach`, which now sends
  `session.input` with `local`); `stagent ls` shows it as PC INPUT.
- Agents started without a stagent session: hooks report how they started
  (`shell_wrapper`, `entrypoint`, `parent_tty`, `parent_batch`), and the
  daemon lists them in memory (`watch`'s `unwrapped[]`,
  `unwrapped.updated`, `doctor`'s `unwrapped`) with a reason
  (`old_terminal`, `bypassed`, `ide`).
- `integrate`: `result`, `login_shell` and per-change `error_code`;
  `--linger` (Linux; undone only if stagent enabled it) and `--terminal`
  (macOS Terminal's `noWarnProcesses`).
- `doctor`: `shell_wrapper.last_run_at` / `last_run_survives_logout`,
  `persistence.linger_needed` / `linger_reason`, macOS `bootstrap_swapped`
  and `terminal`, claude's `remote_control_at_startup`.

### Removed

- `approval.respond` (now `unknown_method`), the hook's allow / deny
  output, `approval_timeout_sec` and `Approval.expires_at`.
