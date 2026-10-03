# stagent changelog

Each release's section is its GitHub release notes (`scripts/release.sh`
publishes it with the binaries).

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
