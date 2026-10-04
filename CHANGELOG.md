# stagent changelog

Each release's section is its GitHub release notes (`scripts/release.sh`
publishes it with the binaries).

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
