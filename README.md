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
diff dist/SHA256SUMS <(curl -sL https://github.com/obutora/stagent/releases/download/v0.2.1/SHA256SUMS)
```

Releases are published with `scripts/release.sh --publish`, which commits
the exact source it built to this repository, rebuilds that commit and
checks it gives the same bytes, and tags that commit — so a release's tag
is always the source of its binaries.

Tests: `go test ./...` (the e2e tests use tmux, zellij and herdr when they are
on `PATH`).

## License

[MIT](LICENSE)
