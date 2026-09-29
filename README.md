# stagent releases

Prebuilt binaries of **stagent**, the server-side companion of the
[SSH Term](https://apps.apple.com/app/id6759213045) app's Agent Mode.

This repository only hosts release assets. SSH Term downloads the binary for
your server's OS/arch from the Releases page and verifies it against the
SHA-256 pinned inside the app before installing it to
`~/.ssh-term/agent/bin/`.

| asset | platform |
|---|---|
| `stagent-linux-amd64` / `stagent-linux-arm64` | Linux |
| `stagent-darwin-amd64` / `stagent-darwin-arm64` | macOS |
| `stagent-windows-amd64.exe` / `stagent-windows-arm64.exe` | Windows 10 1809+ |

Each release also contains `SHA256SUMS`.

Uninstall from the app (Agent Mode → Setup → Cleanup) or on the server with
`~/.ssh-term/agent/bin/stagent uninstall --level purge`.
