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
- `stagent daemon`, the session list, started on demand.

When the app reconnects it re-attaches the tab and receives only the output
it missed (or a redraw of the screen when that is not possible). Sessions
can be listed, re-opened and ended from the app, and from any terminal on
the server:

- `stagent ls [--json]` lists the sessions.
- `stagent attach [ID | --last] [--detach-key ctrl-]]` attaches your
  terminal to a session; Ctrl-] detaches (press it twice to send it).
- `stagent run --handoff -- <command>` runs a command in your terminal as a
  session that survives that terminal: when the terminal closes, the
  session continues detached and the app can pick it up. Shell wrappers
  installed with `stagent integrate --shell-wrapper` add `--handoff` when
  the environment has `STAGENT_HANDOFF=1`.

Sessions also survive logging out. On Linux their sockets live in
`/tmp/stagent-<uid>` (mode 0700), not in `$XDG_RUNTIME_DIR`, which is
removed at logout. On systems where logind kills a user's processes at
logout (`KillUserProcesses=yes`) the bridge starts holders through
`systemd-run --user --scope` so they leave the SSH login session; they
then still end with your last logout unless lingering is enabled
(`loginctl enable-linger`). `stagent doctor` reports this under
`persistence`.

## Installation and removal

The app installs the binary to `~/.ssh-term/agent/bin/stagent` the first time
the chat view is opened: the server downloads the asset for its OS/arch from
this repository's Releases page (or the app downloads and uploads it over
SFTP) and it is verified against the SHA-256 pinned inside the app.

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
