# stagent

Server-side companion of the [SSH Term](https://apps.apple.com/app/id6759213045)
app. This repository holds its source code and its release binaries, so you
can read exactly what the app installs on your server and check that the
binaries were built from this code.

## What it does

SSH Term's **chat view** shows the conversation of the coding agent (Claude
Code, Codex, Oh My Pi) running in a terminal tab, also when it runs inside
tmux, zellij, screen or herdr. For that the app runs `stagent follow` over an
SSH exec channel on the terminal's own connection. It:

- finds the tab's shell through the per-connection SSH server process, or
  among the user's terminal sessions below tailscaled (Tailscale SSH), or
  by the shell's `SSH_CONNECTION`, and the pane a multiplexer client in it
  shows,
- finds the agent process in that shell or pane and the conversation file it
  writes (`~/.claude`, `~/.codex`, `~/.omp`),
- streams new messages of that file as JSON lines until the app closes the
  channel.

It only reads the process list, on macOS the login records
(`/var/run/utmpx`), and those conversation files, and asks the
multiplexer which pane its client shows (`tmux list-clients`, …, run as the
client's own binary). It only considers processes of the user it runs as:
another user's processes are never taken for the tab, an agent or a
multiplexer, so it never runs their binaries or passes on their
environment. It opens no network port, starts no background process and
changes no configuration. Input typed in the chat view goes to the terminal
itself, not through stagent. The wire format and these rules are documented
in [PROTOCOL.md](PROTOCOL.md) ("stagent follow").

The same binary also contains commands for an upcoming Agent Mode (`run`,
`daemon`, `bridge`, `hook`, `integrate`); the chat view does not use them.

## Persistent terminal sessions

When the app's persistent sessions are enabled, a terminal tab can run its
shell inside stagent instead of directly on the SSH connection, so the shell
(and whatever runs in it) survives a dropped connection, a sleeping phone or
closing the app — without tmux. On the server this runs:

- `stagent bridge`, started by the app over an SSH exec channel; it ends
  with the connection.
- one `stagent run --detached` process per session (the *holder*): it owns
  the session's pseudo terminal, keeps an emulated copy of the screen and
  the session's recent output on disk
  (`~/.ssh-term/agent/state/sessions/<id>/`, 8 MiB per session by default),
  and serves the app over a socket only your user can open. It runs your
  login shell (`$SHELL -l`) and exits when the shell exits.
- `stagent daemon`, the session list, started on demand. While a coding
  agent started inside a shell session runs, it reads that session's
  process tree every 2 s to notice when the agent quits, so the list shows
  the session as a plain shell again.

When the app reconnects it re-attaches the tab and receives only the output
it missed (or a redraw of the screen when that is not possible). Sessions
can be listed, re-opened and ended from the app, and from any terminal on
the server:

- `stagent ls [--json]` lists the sessions with their state, last
  activity and when someone last typed in a passthrough session's own
  terminal.
- `stagent attach [ID | --last] [--detach-key ctrl-]]` attaches your
  terminal to a session; Ctrl-] detaches (press it twice to send it).
- `stagent run --handoff -- <command>` runs a command in your terminal as a
  session that survives that terminal: when the terminal closes, the
  session continues detached and the app can pick it up. Before the
  command starts, `stagent run` tells you in one line how many agent
  sessions are held on the host (see `stagent ls`), if any.
- Shell wrappers installed with `stagent integrate --shell-wrapper` make
  `claude`, `codex` and `omp` typed in an interactive terminal run as
  `stagent run --handoff=auto -- …`, so they survive the terminal by
  default. Non-interactive runs (`claude -p`, `codex exec`, `omp -p`,
  `omp --mode json`) are left alone. To have the agent end with the
  terminal instead, set `STAGENT_HANDOFF=0` in that shell, or turn on
  `disable_handoff` in `config.json` (what the app's switch sets);
  `STAGENT_HANDOFF=1` overrides the config.
- An app that starts the agent in a shell reading its profile goes
  through the wrapper too (Orca runs it in such a PowerShell on Windows).
  `stagent integrate` puts Codex's hooks into `~/.codex`: a codex started
  with its own `CODEX_HOME`, as Orca's is, runs none of them. Started
  through the wrapper it is listed without what the hooks report
  (waiting for approval); otherwise it is neither listed nor counted
  among the agents the list cannot show.

Sessions also survive logging out. On Linux their sockets live in
`/tmp/stagent-<uid>` (mode 0700), not in `$XDG_RUNTIME_DIR`, which is
removed at logout. On systems where logind kills a user's processes at
logout (`KillUserProcesses=yes`), or where lingering is on, `stagent run`
and `stagent daemon` first move themselves into a new scope of your
systemd user manager (the D-Bus call `systemd-run --user --scope` makes),
so they and the agent leave the login session. That scope still ends with
your last logout unless lingering is enabled (`loginctl enable-linger`,
or `stagent integrate --linger`), and so do agents started from a
terminal of a graphical (GNOME etc.) session. `stagent doctor` reports
whether lingering is needed under `persistence.linger_needed`.

On macOS, sessions started from a terminal survive logging out of the GUI
as they are, but the GUI session's Mach bootstrap port they inherit stops
working then, and the tools an agent starts afterwards (`curl`, `git`,
MCP servers) could no longer resolve host names. So `stagent run` and
`stagent daemon` first switch to your per-user bootstrap port, as tmux
does; the keychain stays usable while you are logged in. If that fails
they start anyway and `stagent doctor` says why
(`shell_wrapper.last_run_bootstrap_error`, `daemon.bootstrap_error`).
The login keychain is unlocked only inside the GUI login session, not in
SSH sessions, so while you are logged in to the GUI the app's sessions
start through a one-off launchd job in that session (`gui/<uid>`), and
claude uses the login it has there. Without a GUI login they start from
the SSH session and claude asks you to `/login`; that login is kept in
`~/.claude/.credentials.json`.
`stagent integrate --terminal` keeps Terminal.app from asking before it
closes a window running an agent, or at logout (`stagent` in each
profile's `noWarnProcesses`; removing it takes out only that entry).

On Windows the wrapper goes into the profiles of Windows PowerShell 5.1
and PowerShell 7 (cmd has none), and wraps only in a console of the
desktop: an SSH session is not interactive for PowerShell, and an agent
started there ends with the connection (start it in a kept shell
instead). PowerShell runs the profile only under an execution policy
other than `Restricted` / `AllSigned`; `stagent doctor` reports each
one's policy (`powershell[]`) and `stagent integrate --execution-policy`
sets `RemoteSigned` for your user where needed. Processes started over
SSH, kept shells included, run with Windows' RedirectionGuard
(`redirection_guard` in `doctor`): tools reached through a junction you
created, such as scoop's shims, do not start there.

In WSL, logging out is not what ends sessions: WSL shuts a distribution
down some 15–40 s after the last Windows terminal tab or SSH connection
using it closes (processes inside WSL do not keep it running), and the
sessions end with it. `stagent integrate --wsl-keep-running` sets
`[general] instanceIdleTimeout=-1` in your Windows `.wslconfig`, which
applies to all your distributions once WSL restarts (`stagent
wsl-shutdown`, or restarting the PC); WSL, Windows restarts and signing
out of Windows still end them. `stagent doctor` reports it under
`persistence.wsl`; lingering is never needed in WSL.

## Installation and removal

The app installs the binary to `~/.ssh-term/agent/bin/stagent` the first time
the chat view is opened: the server downloads the asset for its OS/arch from
this repository's Releases page (or the app downloads and uploads it over
SFTP) and it is verified against the SHA-256 pinned inside the app.

An app that pins a newer release updates the binary the same way: silently
on a host prepared for it (shell wrapper in place), otherwise when you tap
the host's button in the app. `stagent install`, which the app runs after
placing the binary, then rewrites the shell wrapper blocks already in place
and replaces a running daemon of the old version; the sessions keep
running and stay listed.

Remove it from the chat view's menu (**Remove stagent from the server**), or
on the server:

```sh
~/.ssh-term/agent/bin/stagent uninstall --level purge
```

## Release assets

| asset | platform |
|---|---|
| `stagent-linux-amd64` / `stagent-linux-arm64` | Linux |
| `stagent-darwin-amd64` / `stagent-darwin-arm64` | macOS |
| `stagent-windows-amd64.exe` / `stagent-windows-arm64.exe` | Windows 10 1809+ |

Each release also contains `SHA256SUMS`.

## Building and verifying a release

Builds are reproducible (v0.2.0 and later; earlier releases predate the
published source): `CGO_ENABLED=0`, `-trimpath`, no VCS stamp and an
empty build id, so the same source and Go toolchain always produce the same
bytes. To check a release, build the tag's source with the Go version named
in its release notes and compare:

```sh
scripts/release.sh          # writes dist/ and dist/SHA256SUMS
diff dist/SHA256SUMS <(curl -sL https://github.com/obutora/stagent/releases/download/v0.3.0/SHA256SUMS)
```

Releases are published with `scripts/release.sh --publish`, which commits
the exact source it built to this repository, rebuilds that commit and
checks it gives the same bytes, and tags that commit — so a release's tag
is always the source of its binaries.

Tests: `go test ./...` (the e2e tests use tmux, zellij and herdr when they are
on `PATH`).

## License

[MIT](LICENSE)
