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
`agent_refused`, `not_clean`, `busy`, `in_use`, `paste_by_hand`.

`not_clean`: `task.remove` found uncommitted changes and no `force`.
`busy`: `task.remove` found live sessions of the task and no
`stop_sessions`, could not stop them, or found the task still
`preparing` or already `removing`; `schedule.run_now` found the
schedule's previous run still running, and `schedule.remove` with
`stop_run` could not stop it. `in_use`: another number's
`in_place` task uses that source checkout (see "Tasks"). `paste_by_hand`:
`host_screen.input` could not paste a text into an iOS simulator; it is
on the simulator's clipboard for the user to paste (see "Host screen").

`foreign_owner`: a request that needs the daemon or a session's holder
(any daemon or session method, `session.spawn`) found the stagent location
it would use — the run directory, a socket or a named pipe — owned by
another user (see `hello`'s `blocked`). Nothing was sent there. The
message names the location and its owner.

`agent_refused`: the local connection came from a process that a coding
agent (claude, codex, omp, pi, copilot) started — stagent walks its parents — or, on
macOS, from a process in a sandbox (Seatbelt, which coding agents run
their commands in), or from one it could not check: it exited first, or
its parents were more than 256 deep or kept changing (ADR 0004). A
holder answers whatever such a connection asks first with it and closes
the connection; the daemon refuses `config.set`, `presence.set`,
`notify.test`, `daemon.shutdown`, the methods that change tasks
(`task.register`, `task.progress`, `task.forget`) and those that change
schedules (`schedule.create`, `schedule.update`, `schedule.remove`,
`schedule.pause`, `schedule.resume`, `schedule.run_now`), and serves the rest (`hook.event`,
`holder.*`, reads, `task.list`, `schedule.list`, `schedule.runs`,
`schedule.preview`, `schedule.output`). The daemon decides
when the connection is made. The message says why. Only Linux and macOS
check, and only without `STAGENT_HOME`. `stagent bridge`, started by
sshd, has no agent among its parents, so the app does not see this error
unless the bridge itself runs under an agent; such a bridge answers
`session.spawn` with it before starting a holder (which would refuse the
bridge and keep running out of its reach). It serves the read-only
`github.*` methods and `host_screen.*` all the same.

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
  `github` is announced only on Windows, where the bridge serves the
  GitHub screen, `host_screen_ios` only on macOS, where the host
  screen shows iOS simulators (the other kinds work on every OS and need
  none; a missing tool, such as Xcode, is told by `host_screen.list`),
  and `schedules` only on Linux and macOS, where the daemon runs
  schedules (定期実行).
  `push_chat` is the one exception that does not depend on the OS: an
  older stagent stores `notify.chat` like any unknown config key and
  silently never sends to it, so the app configures chat destinations
  only on hosts that announce it. Whether a key is present is checked only
  for `host_id`.

## Methods

### Bridge

| method | params | result |
|---|---|---|
| `hello` | `{protocol, client}` | `{protocol, version, os, arch, home, capabilities[], host_id?, blocked?}` |
| `ping` | – | `{}` |
| `session.spawn` | `{command[]?, shell?, cwd?, cols, rows, env?{}, task_id?}` | `{session}` — a detached session; app owns its size |
| `task.create` | `{repo, kind, number?, name?, title?, url?, base?, branch?, command[]?, cols, rows, env?{}, in_place?, switch_branch?, setup?}` | `{task, session?, existed}` — see "Creating a task"; empty or absent `command` = a terminal (the login shell; `cmd.exe` on Windows) |
| `task.preview` | `{repo, worktree_include?[], name?, branch?}` | `{repo, copies[{path, source, bytes}], links[{path}], skipped[{entry, source, reason}], total_bytes, truncated, setup?, orca_error?, name_problem?}` — see "Files to copy" and "Creating a task" |
| `task.remove` | `{id, force?, stop_sessions?, skip_archive?, dry_run?}` | `{changes[]?, changes_truncated?, unpushed?, sessions[]?, kept_branch?}` — see "Removing a task" |
| `worktree.list` | `{}` | `{worktree_folder?, watch_folders[], repos[{path, name, default_branch?, github, worktrees[{path, branch?, head?, missing?, locked?, lock_reason?, task_id?, sessions[]?, changes, unpushed, merged}]}], folders[{path, repo}]}` — see "Worktrees" |
| `worktree.sizes` | `{}` | `{entries[{path, bytes, modified_at?}], total}` — see "Worktrees" |
| `worktree.prs` | `{}` | `{prs[{path, number, state, url, merged}]}` — see "Worktrees" |
| `worktree.watch` | `{add?, remove?}` | `{watch_folders[]}` — see "Worktrees" |
| `worktree.remove` | `{path, force?, stop_sessions?, dry_run?}` | `{changes[]?, changes_truncated?, unpushed?, sessions[]?, kept_branch?, bytes?, entries[{name, bytes, dir}]?, removed_parent?}` — see "Worktrees" |

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
shell (`$SHELL -l -i`, then `$SHELL -l`, each 5 s at most, 10 s in all;
captured once per bridge), not the
narrower one of the SSH exec channel, so agents on a PATH set in
`~/.bash_profile` / `~/.zprofile` / `~/.zshrc` are found. A bare command
name still missing from that PATH is looked up in the usual per-user tool
directories (`~/.bun/bin`, `~/.local/bin`, npm/pnpm/volta/nvm/fnm/mise,
Homebrew). `env` entries override both.

A program that ends at once can take its holder with it before the bridge
reaches the holder; the answer is then the session as the daemon lists it
(ended, or about to be), as for `task.create` and `task.remove`'s session,
rather than an `internal` error.

`task_id` ties the new session to that task (`Session.task_id`, see
"Tasks"): the way to add a session to an `in_place` task (the task
detail's agent and terminal). A worktree task's sessions are those in its
worktree anyway, so there it changes nothing; an unknown id ties nothing.
The holder is started with `stagent run --task <id>` and registers with it;
the daemon decides.

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

A holder whose command is a bare `powershell.exe` or `pwsh.exe` (no
arguments; Windows only) starts it with `-NoExit -EncodedCommand <hook>`:
after the profiles, the prompt function is wrapped so that each prompt
sets the process's working directory to the last file system location
(`Set-Location` alone does not change it, and the GitHub screen reads
it). The wrapped prompt runs first and still sees the command's `$?`
and `$LASTEXITCODE`. `Session.command` stays the bare shell. The
PowerShell profile block of the shell wrapper has the same hook, for the
terminals sshd starts (see "Command-line tools"); wrapping twice changes
nothing.

On Linux a holder that must leave the login session it was started from
(here the bridge's SSH session) to outlive it moves itself into a new scope
of the user's service manager as the first thing `stagent run` does — see
"Leaving the login session" under Command-line tools. The bridge starts
holders directly there. On macOS, while the user is logged in to the GUI,
the bridge starts them in the GUI login session so that the agent can use
the login keychain — see "Starting in the GUI login session".

Capabilities: `screen_mode`, `spawn`, `hooks`, `transcript`, `push`,
`push_chat`, `persist`, `worktrees`, on Windows `persist_shell` and
`github`, on macOS `host_screen_ios`, and on Linux and macOS `schedules`.
`push_chat` announces the chat destination (`notify.chat`) and
`notify.test`'s `channels`: an older stagent stores `notify.chat` like
any unknown key without sending to it, so an app configures chat
destinations only on hosts that announce it. `persist` announces everything
this document marks with it: `session.spawn` `shell`, attach resume
(`since`, `offset`, `resumed`, `output.end`), the `hangup` signal,
input-mode restoring snapshots and sessions changing from `passthrough`
to `detached` (handoff). An app that relies on them treats a bridge
without `persist` as needing an update. Windows bridges before 0.5.0
announced `persist` but refused `shell` with `unsupported`; on Windows
`persist_shell` announces that `shell` works, and an app treats a
Windows bridge without it as needing an update. `github` (Windows only)
announces the `github.*` methods below. `host_screen_ios` (macOS only,
since 0.11.0) announces the host screen's `ios` kind below: its `list`
section and `start` of a simulator. `schedules` (Linux and macOS, since
0.17.0) announces the `schedule.*` methods (see "Schedules"); a stagent
before it answers them `unknown_method`, and on any other OS the daemon
answers them `unsupported`. `worktrees` (every OS, since 0.18.0)
announces the `worktree.*` methods (see "Worktrees"); a stagent before it
answers them `unknown_method`.

#### Creating a task (`task.create`, `stagent task run`)

`task.create` makes a task (see "Tasks") and starts its first session,
the prepared one, on the spawn queue; refused with `agent_refused` to a
coding agent's process tree before anything starts (ADR 0004). The task's
preparation runs in that session, not in the bridge (ADR 0006):

1. `repo` is made canonical (`git rev-parse --show-toplevel`, symlinks
   resolved; `bad_request` when it is no git checkout). `kind` is `issue`
   or `pr` with `number` positive, or `name` (a named task, since 0.14.0, #877) with `name`
   and no `number` (`bad_request` otherwise). A named task ignores
   `title` and `url` and cannot be `in_place` (`bad_request`); its `name`
   must be valid (below, `bad_request` otherwise) and `config.json` must
   have `worktree_folder` (`bad_request` "task.create: worktree_folder is
   not set" otherwise).
2. Branch and base: an issue's `branch` is the given one (checked with
   `git check-ref-format --branch`, `bad_request` when invalid) or
   `<number>-<slug>`, the slug being the title lower-cased with every run
   of characters other than `a-z0-9` one `-`, trimmed of `-`, cut to 40
   characters (`issue-<number>` alone when nothing is left); a named
   task's the given one (checked the same way) or its `name`; `base` is
   the given one or the default branch the checkout knows (`origin/HEAD`,
   else origin's `main` or `master`; `bad_request` without one). A PR's
   are its head and base branch from `gh pr view <number> --json
   headRefName,baseRefName` (`unavailable` with gh's message when that
   fails); `branch` and `base` are ignored. An `in_place` task without
   `switch_branch` records the checkout's current branch. A task that
   exists keeps its own unless the app gives an issue's or named task's
   `branch` / `base`.
3. The daemon registers it (`task.register`, atomic by `repo` + `number`,
   or `repo` + `name`):
   - `ready` (or `missing`, `removing`): nothing starts; `{task, existed:
     true}`.
   - `preparing`: nothing starts; `{task, session, existed: true}` with
     its live prepared session (no `session` while that one has not
     registered yet).
   - `failed` (or interrupted): prepared again as below (`existed: false`).
   - `in_place` on a checkout another number's `in_place` task uses:
     `in_use`.
4. A worktree task works in its place (`task.worktree`): with
   `worktree_folder` set, `<worktree_folder>/<repo's name>/<name>` for a
   named task and `<worktree_folder>/<repo's name>/<number>-<slug>` for an
   Issue or PR (the default branch above, whatever `branch` is); without
   it, an Issue's or PR's is `<parent of repo>/<repo's name>-<number>`.
   A new named task's place must not exist or be an empty directory
   (`bad_request` before anything is registered); a task that exists
   keeps the place it has. The bridge makes that directory, empty (with
   any missing parents), and starts the prepared session there; an
   `in_place` task's session starts in `repo`. So the session's `cwd` is
   the task's place from the start (its `task_id` follows). The session
   runs `<stagent> task run [--switch-branch] [--setup] <task id> --
   <command…>` (`command`, or the login shell `[$SHELL, "-l"]` — on
   Windows `[%ComSpec%]`, `cmd.exe` — when it is empty); its `harness` is
   that of `<command…>`. `env`, `cols` and `rows` are as for
   `session.spawn` (the login shell's environment; git and gh are looked
   up like a bare command). The reply is `{task, session}` (`existed:
   false`) once the holder answers.

A named task's `name` is 1–64 characters without whitespace, control
characters or any of `/ \ : * ? " < > |`, does not start with `.` or `-`,
and is a branch name `git check-ref-format --branch` prints back as it is
(the app checks all but git). It names the task, its default branch and
its directory under `worktree_folder`.

`task.preview` with a non-empty `name` (since 0.14.0, #877) also answers `name_problem`, why a
new named task of `repo` could not take that name with `branch` (default
`name`) — the first of: `invalid` (not a valid name), `task` (a task of
`repo` has that name), `folder` (`<worktree_folder>/<repo's name>/<name>`
exists), `branch` (the local branch `refs/heads/<branch>` exists; a
`task.create` would take it as it is), `no_folder` (`worktree_folder` is
not set). It is absent when the name is free. The app asks while the name
is typed; `task.create` checks again.

`stagent task run` reads the task (`task.list`) and runs its stages,
announcing each with `task.progress {id, stage}` and a `[stagent] <stage>`
line in the terminal, where the commands it runs (`$ git …`, `$ gh …`)
and their output show:

- `fetch`: `git fetch origin <base>`. Failing (offline, no `origin`) is
  only a line in the terminal.
- `branch`:
  - Issue: `gh issue develop <number> --base <base> --name <branch>` in
    `repo` (gh links the branch to the issue on GitHub, or uses the linked
    branch of that name, and fetches it); a local branch tracking
    `origin/<branch>` follows. When gh fails (permissions, network,
    unauthenticated) the branch is made locally from `origin/<base>` (else
    the local `<base>`) only, with one line saying so.
  - Named: as an issue's without gh: the local `<branch>` as it is, else
    one tracking `origin/<branch>` when the checkout knows it, else `git
    branch --no-track <branch>` from `origin/<base>` (else the local
    `<base>`).
  - PR: `git worktree add --detach <worktree>`, then `gh pr checkout
    <number> --branch <head>` inside it (gh fetches fork PRs and sets
    their push remote). A branch of that name checked out in another
    worktree fails, naming it.
  - `in_place`: with `--switch-branch` (`switch_branch`) the issue's branch
    is made as above and `git switch`ed to in `repo`, a PR's checked out
    with `gh pr checkout` in `repo`; without it the checkout stays as it
    is.
- `worktree` (worktree tasks): `git -C <repo> worktree add <worktree>
  <branch>` into the empty directory. A worktree of `repo` already there
  on `<branch>` is taken as it is; one on another branch fails, as does a
  non-empty directory that is no worktree of `repo` and a branch checked
  out in another worktree (the error names its path). A worktree git
  still lists without its directory is pruned first. On Windows every git
  call gets `-c core.longpaths=true`.
- `copy` (worktree tasks): the shared directories are linked and the
  files to copy copied from `repo` into the worktree (see "Files to
  copy"). It never fails: what cannot be linked or copied is a line in
  the terminal, and setup and the agent follow.
- `setup`: `scripts.setup` of the `orca.yaml` in the task's place (see
  below); always for a worktree task, for an `in_place` one only with
  `--setup` (`setup`). No file or no script: nothing to run.
- `agent`: reported once `<command…>` is found, then run: `exec`ed on
  Linux and macOS (the session's program becomes the agent), a child on
  the same console on Windows (batch files through `cmd.exe` as for
  `session.spawn`). The task is `ready`. When the agent cannot be started
  after all (`exec` failing, e.g. a script's missing interpreter), that is
  reported as `task.progress {id, stage: agent, error, session_id}` from
  the prepared session: the task is `failed` with stage `agent`.

An `in_place` task has no `worktree` and `copy` stages. A failing stage
prints the error, reports it (`task.progress {id, stage, error}`: the
task is `failed` with that `stage`), starts no agent and exits non-zero
(1; 127 when `<command…>` is not found): the session ends as an abnormal
exit, its output kept in the scrollback. A worktree directory left empty
is removed (`stagent task run` leaves it first). `task.create` on the
failed task runs every stage again, each safe to repeat: the branch and
the worktree on it are taken as they are, only what is missing is
copied or linked (nothing is overwritten), setup runs again.

`orca.yaml` (Orca's per-repository file at a checkout's root; stagent
only reads it): `scripts.setup`, `scripts.archive` (strings) and
`worktree.sharedDirectories` (strings) are used; other keys are ignored.
A file over 256 KiB, invalid YAML, a repeated key or a document that is
not a mapping makes the file unusable: setup fails with that error (the
source checkout's broken file only loses its `sharedDirectories`: a
warning line, and copying goes on). A value of another type, or a string
over 64 KiB, is dropped alone. A script runs with `ORCA_ROOT_PATH` (the
source checkout), `ORCA_WORKTREE_PATH` (the task's place, also its
working directory) and `ORCA_WORKSPACE_NAME` (that directory's name)
added to the session's environment, its input and output on the
terminal:

- Linux, macOS: the login shell — `$SHELL` when it is `sh`, `bash`,
  `zsh`, `dash`, `ksh`, `mksh`, `ash` or `yash`, else `/bin/sh` — as
  `<shell> -l -c 'set -e␤<script>'`: the first failing command fails it.
- Windows: a `.cmd` runner in `%ComSpec%` (`cmd.exe /d /s /c`), as Orca's:
  `@echo off`, `setlocal EnableExtensions DisableDelayedExpansion`, then
  for each line (blank lines and `::` / `rem` comments left out) `call
  <line>` and `if errorlevel 1 exit /b %errorlevel%`. Being `call`ed, a
  line that is an `if` or `for` statement does not run (cmd: `'if' is not
  recognized…`, errorlevel 1), in Orca too. A script starting
  with `#!` runs in Git for Windows' `bash.exe` (`bin\bash.exe` or
  `usr\bin\bash.exe` of the installation git runs from; never
  System32's) with `set -e` first and the `ORCA_*` paths written
  `/c/…`; without that bash it fails before running anything.

#### Files to copy (`task.preview`, the `copy` stage)

A new worktree gets from its source checkout (`repo`) copies of the files
to copy (コピーするファイル) and links to the shared directories
(共有ディレクトリ). The `copy` stage and `task.preview` resolve them with
the same code, against `repo` as it is now:

- Sources (`source`): `include` — `repo`'s `.worktreeinclude`, read as
  Orca does: one literal path per line from the root (`\` read as `/`, a
  leading `./` and a trailing `/` dropped, a repeated path taken once);
  blank lines and lines starting with `#` are ignored; a line with `*`,
  `?` or `[`, starting with `!`, absolute, or with a `..` or `.git` component is
  skipped (`invalid`); a file over 256 KiB is skipped whole (one skip,
  entry `.worktreeinclude`, `invalid`); past 1,000 valid lines the rest
  are skipped (`limit`). `app` — `config.json`'s
  `repos.<repo>.worktree_include` (see `config.set`): git pathspec globs
  from the root (`*`, `?`, `[…]`, `**`; case-sensitive; a directory
  selects everything in it; no negation), each listed with `git ls-files
  -o -i --exclude-standard -z -- ':(glob)<pattern>'`. The two have no
  precedence; a path both select is copied once (as `include`'s, which
  is resolved first).
- Only what exists and is gitignored is copied. A tracked path (for a
  directory: one holding a tracked file) is skipped silently in the
  terminal (`tracked`); an untracked path that is not gitignored is
  skipped with a warning (`not_ignored`); a `.worktreeinclude` path that
  does not exist (`missing`) and a pattern that selects nothing
  (`no_match`) get one line each. An app pattern's matches are listed file
  by file, a `.worktreeinclude` line as it is (a directory whole).
- Shared directories: every one of `worktree.sharedDirectories` of
  `repo`'s `orca.yaml` (paths as in `.worktreeinclude`); one inside
  another listed one is dropped (the outer one links). Only those that
  exist, are directories and are gitignored (untracked) are linked
  (`links`), the rest passed over silently. Anything either source lists
  inside a linked directory is skipped (`under_link`).
- Limits: 2 GiB and 50,000 filesystem entries for one worktree. A copy
  that would pass either is skipped with a warning (`limit`); later ones
  that fit are still copied.

The `copy` stage first links each shared directory at the same path in
the worktree — a symbolic link on Linux and macOS; on Windows a junction,
else a directory symbolic link (Developer Mode or elevation; on a network
share, where a junction would not resolve, only this, with elevation) — then
copies, in order: a file, or a directory with what is in it (a top-level
symbolic link as what it points to, links inside a directory as links),
each file through a temporary file renamed into place. A destination
that exists is left as it is (the terminal says `already there`, the
only use of that reason): retried, the stage links and copies only what
is missing. When a shared directory cannot be linked (on Windows neither
way works) the terminal gets one warning line and what the sources list
inside it is copied as ordinary files instead; neither this nor any
copy error reaches `Task.error` or the app. `stagent task run` reads the
app's patterns from `config.json` itself (only the daemon writes it).

`task.preview` answers, for `repo` made canonical (`bad_request` when it
is no git checkout), what a new worktree would get if every shared
directory linked: `copies` in order (`bytes`: a directory's files
summed), `links`, `skipped` with `reason` `tracked`, `not_ignored`,
`missing`, `no_match`, `under_link`, `invalid` or `limit`, `total_bytes`
of every copy. `copies` and `skipped` hold 500 rows at most, `truncated:
true` when either was cut. `worktree_include`, when present (an empty
list too), is used instead of the saved patterns — an unsaved trial; an
invalid pattern there is skipped (`invalid`). `setup` is `repo`'s
`orca.yaml` `scripts.setup` (absent with no file or no script);
`orca_error`, one line, says that file is unusable (its
`sharedDirectories` are then ignored). git runs with the login shell's
environment, as for `session.spawn`, on the bridge's own `preview` queue
(never delaying the daemon and spawn queues); agent descendants may call
it (ADR 0004).

#### Removing a task (`task.remove`, `stagent task remove`)

`task.remove` is 片付け (ADR 0006) on a queue of its own (neither the
daemon's nor the spawn queue), git and gh run in the login shell's
environment as for `task.create`; refused with `agent_refused` to a coding
agent's process tree, a dry run included (ADR 0004). An unknown `id` is
`not_found`; a `removing` task is `busy`, and so is a `preparing` one
unless `stop_sessions` (see below).

`dry_run: true` changes nothing and reports what is in the way:

- `changes[]`: the worktree's `git status --porcelain` lines (`XY path`,
  a rename `XY from -> to`): tracked files changed, staged or not, and
  untracked files git does not ignore. Ignored files (a copied `.env`) and
  the shared directories' links are not changes. At most 200 lines, then
  `changes_truncated: true`. None when git does not list the worktree.
- `unpushed`: `git rev-list <branch> --not --remotes --count` in `repo`
  (omitted: 0 or no such branch). It refuses nothing: the branch is kept.
- `sessions[]`: ids of the task's live sessions (`Session.task_id`;
  agents and kept shells).

An `in_place` or `missing` task's dry run is `{}`: its removal only
forgets the record.

The removal:

- A `preparing` task (any kind) without `stop_sessions` is `busy`. With
  it, its live sessions are stopped first as in stage 1 (sessions bound to
  the task meanwhile too); the task is then `failed` (interrupted) and the
  removal goes on as for that; still `preparing` is `busy`.
- `in_place`: the record is forgotten (`task.forget`), nothing else.
- `missing`: the record is forgotten and `git worktree prune` runs in
  `repo`; no archive.
- Otherwise `changes` without `force` is `not_clean`, live sessions
  without `stop_sessions` `busy`, both before anything changes. Then:
  1. `stop` (the task becomes `removing`; another removal already there
     makes it `busy`): each live session gets `session.signal hangup`,
     those still live after 5 s `kill`; sessions bound to the task
     meanwhile are stopped the same way (up to 3 rounds). One still live
     after that makes it `busy` and the task goes back as it was (with
     that error). Ended sessions stay listed.
  2. `archive`: `scripts.archive` of the source checkout's `orca.yaml`, run
     like setup (see above) with the worktree as its working directory;
     not with `skip_archive`, nor when git does not list the worktree. An
     unusable `orca.yaml` fails this stage.
  3. `worktree`: the shared directories (`worktree.sharedDirectories` of
     the source checkout's `orca.yaml`) that are links in the worktree
     (symbolic links; on Windows also junctions) are unlinked — only the
     link, nothing under it — then `git -C <repo> worktree remove
     [--force] <worktree>` (`--force` with `force`). A worktree git no
     longer lists is pruned (`git worktree prune`) and its directory
     removed only when empty; when a git before 2.18 cannot remove a
     worktree whose directory is gone, it is pruned instead.
  4. `branch`: `git branch -d <branch>` in `repo`. When git refuses (a
     squash merge leaves the branch unmerged for git), `gh pr view
     <branch> --json state,headRefOid` in `repo` (a PR task: `gh pr view
     <number>`, which also finds a fork's pull request): `MERGED` with
     `headRefOid` the local branch's commit deletes it with `git branch
     -D`; anything else, gh failing included, keeps it, named in
     `kept_branch`. Remote branches are never deleted.
  5. The record is forgotten: `task.forget {id, kept_branch?}`, so
     watchers get `task.removed {id, kept_branch?}`.

  Each stage is `task.progress {id, stage}` (`Task.stage`, state
  `removing`). A failing stage — the archive script, `git worktree
  remove` (a process stagent does not know holding a file, …) — takes the
  task back to the state it had (`ready`, or `failed`) with its one-line
  `error` and the failed `stage`, keeps the record and is answered `internal` with that line
  (git's own error for `worktree remove`). `task.remove` again retries;
  `skip_archive` leaves a failing archive out.

With `scripts.archive` to run (stage 2 applies), stages 2–5 run in a new
detached session (like `session.spawn`'s, in the app's session list) whose `cwd`
is the source checkout (Windows cannot remove a worktree that is a
process's working directory), running `<stagent> task remove [--force]
<task id>`: its terminal shows each stage (`[stagent] <stage>`), the
commands and their output; it exits non-zero on a failure. The session is
not tied to the task. Without archive the bridge runs stages 3–5 itself
and no session starts. Either way the reply comes when the removal has
finished: `{kept_branch?}` once the daemon reported `task.removed`, or
the error above once the task left `removing`; a session ending before
either fails the stage it was at. When the bridge goes away first the
removal carries on in that session.

Every removal unties the task's sessions, ended ones still listed
included (`session.updated` without `task_id`).

#### Worktrees (`worktree.*`)

Since 0.18.0 (capability `worktrees`, every OS, #1122, ADR 0016) the
bridge lists every linked worktree git knows on the host — a task's or
not: Orca's, `claude --worktree`'s, made by hand — with the other
folders left in the worktree folder (置き場のほかのフォルダ), and removes
them (worktree を消す). git and gh run in the login shell's environment
as for `task.remove`. As with `task.remove` (ADR 0006) the work stays
off the daemon's and spawn queues: `worktree.list` and `worktree.watch`
share a queue of their own, `worktree.sizes` and `worktree.prs` have one
each (a disk walk or gh never holds up the list), and `worktree.remove`
runs on `task.remove`'s queue, one removal at a time.

The inventory (目録), made again for each call: candidate directories
are config.json's `worktree_repos`, the task records' `repo` and (not
`in_place`) `worktree`, `<worktree_folder>/*` and
`<worktree_folder>/*/*` that have a `.git` (file or directory) directly
in them, `<watched folder>/*` likewise, and the sessions' (ended ones
too) `cwd` that exist; names starting with `.` are not matched by `*`.
Each candidate is taken to the nearest directory at or above it with a
`.git`; unless that is a source checkout or worktree already found, `git
worktree list --porcelain` runs there, whose first entry is the source
checkout. A repository around others (dotfiles kept in git at `~`) so
hides none inside it. A candidate git fails for is skipped silently. A
repository without linked worktrees is found (and remembered) but not
listed, measured or asked gh about.

The other folders (only with `worktree_folder` set): a directory `X`
directly in the worktree folder that is neither a worktree nor a source
checkout is `<repo name>` when a repository found has that base name or a
worktree or source checkout is directly in it — then each directory `Y`
in it that is neither is `{path: Y, repo: <X's name>}` — else `{path: X,
repo: ""}`. Files and names starting with `.` are ignored; non-git
folders outside the worktree folder are never listed.

`worktree.list` changes nothing on the host (it never prunes; `git
status` runs with `GIT_OPTIONAL_LOCKS=0`) except config.json's
`worktree_repos`: when the source checkouts found (sorted) differ from
it, the bridge writes them through the daemon's `config.set`, so a
repository whose source checkout is gone drops out of both. It answers
`worktree_folder` (canonical; absent when unset), `watch_folders`
(config.json's `worktree_watch`), `folders` (path order) and `repos` in
the order of their source checkout's path: `path` (canonical), `name`
(its base name), `default_branch` (origin/HEAD, else origin's main or
master, else — no origin — the local main or master; absent when
unknown), `github` (a remote's URL is on github.com) and `worktrees` in
git's order, the source checkout itself not among
them:

- `path` (canonical), `branch` (absent: detached), `head`.
- `missing`: its directory is gone (git's `prunable`). It is listed, not
  pruned.
- `locked`, `lock_reason` (`git worktree lock --reason`; absent when
  none).
- `task_id`: the task (not `in_place`) whose `worktree` it is.
- `sessions[]`: ids of the live sessions whose `cwd` is inside it, or of
  `task_id`'s task.
- `changes`: the number of `task.remove`'s dry-run `changes` lines (0
  when missing); `unpushed`: commits of `branch` on no remote.
- `merged`: `head` is an ancestor of `refs/remotes/origin/<default
  branch>` (else `refs/heads/<default branch>`); false without a default
  branch.

`worktree.sizes` measures the source checkouts, the worktrees that are
not missing and the folders: `entries[]` in that order (repositories as
listed, each source checkout then its worktrees; then the folders),
`bytes` the disk usage — on Linux and macOS `st_blocks × 512`, a file
with several hard links counted once; on Windows the files' sizes —
following no symbolic link or junction (a shared directory's link counts
as itself), and not counting a directory below that is itself one of
them (a worktree in `.claude/worktrees/` of the source checkout is not
in the checkout's bytes); what cannot be read is skipped. `modified_at`
is the newest modification time in it (RFC 3339, UTC, whole seconds;
absent when nothing could be read). `total` adds the worktrees and the
folders, not the source checkouts. Up to 4 are measured at once.

`worktree.prs` asks gh, in the source checkout, `gh pr view <branch>
--json number,state,url,headRefOid` for every worktree with a branch of
a repository with `github` (a PR task's worktree by the task's number,
as `task.remove` asks: gh finds a fork's pull request by number only; up
to 4 at once; nothing for the others). A branch gh finds nothing for, or
gh missing or failing, is left out.
`merged` is `state` `MERGED` with `headRefOid` the worktree's `head`
(the rule `task.remove` deletes a branch by), so a squash-merged branch
with no later commit is merged.

`worktree.watch {add}` adds a watched folder (見るフォルダ): `~` expanded,
made canonical; not an existing directory is `bad_request`. `{remove}`
removes the same path. Exactly one of them. Either writes
`worktree_watch` through the daemon's `config.set` and answers
`{watch_folders}` as stored. Refused with `agent_refused` to a coding
agent's process tree (ADR 0004).

`worktree.remove {path}` looks `path` (`~` expanded, canonical) up in
the inventory: not in it is `not_found`; a source checkout or a locked
worktree is `bad_request` (never removed). Refused with `agent_refused`
to a coding agent's process tree, a dry run included.

- A task's worktree (`task_id`) is `task.remove {id, force,
  stop_sessions, dry_run}` as above: archive, the record forgotten
  (`task.removed`), its result.
- Another worktree: `dry_run` answers `changes` (at most 200, then
  `changes_truncated`), `unpushed` and `sessions` (live sessions inside
  it), as `task.remove`'s. Otherwise `changes` without `force` is
  `not_clean` and live sessions without `stop_sessions` `busy`; the
  sessions are stopped as `task.remove` stops a task's (hangup, kill),
  one still live is `busy`; then `task.remove`'s stages 3 and 4 run
  without archive: the shared directories' links are unlinked, `git
  worktree remove [--force]` (a missing worktree goes too, alone; a git
  before 2.18 falls back to `git worktree prune`), and the branch is
  deleted only by `git branch -d` or a pull request merged at its commit,
  else kept and named in `kept_branch`. Remote branches are never
  touched. A failure is `internal` with its one line.
- A folder: one with a source checkout or worktree anywhere in it is
  `bad_request` (it would go with it). `dry_run` answers `bytes`
  (measured as by `worktree.sizes`) and `entries[]`, its 5 largest
  entries `{name, bytes, dir}`. Otherwise it is deleted with everything
  in it (read-only files and directories made writable when the first
  try fails).

After a removal, when `path`'s parent is `<worktree_folder>/<repo name>`
(directly in the worktree folder) and empty but for a macOS `.DS_Store`,
it is removed too and named in `removed_parent`.

#### GitHub screen (Windows only)

On Linux and macOS the app finds the working trees and asks gh with POSIX
sh scripts over plain exec; native Windows (sshd DefaultShell cmd or
PowerShell) has no `sh`, so there the bridge answers the same questions
(ADR 0007). Only a Windows bridge serves them and announces `github`;
elsewhere they are `unknown_method`. They are read-only, so a bridge
running under a coding agent serves them too (ADR 0004). They have their
own queue: a slow gh never delays daemon, `session.spawn` or session
requests. git and gh run with the environment `session.spawn` starts
sessions with (the bridge's own on Windows), PATH supplemented with the
same tool directories; git is always called with `-c core.longpaths=true`.

| method | params | result |
|---|---|---|
| `github.repos` | – | `{repos[], git_missing}` |
| `github.activity` | `{path, host, repo, branch?}` | `{gh: "ready"\|"missing"\|"unauthenticated", auth_message?, package_manager?, description?, is_private, pulls?\|pulls_error?, issues?\|issues_error?, current?}` |
| `github.status` | `{items[{repo, kind: "issue"\|"pr", number, branch?}]}` | `{items[{repo, kind, number, branch?, state, pr_state?}]}` |

`github.repos` lists the git working trees the user worked in last,
newest first. Candidate directories: the working directory (read from the
process's PEB) of the user's processes running under one of their SSH
connections (`sshd` / `sshd-session`) or under a holder — shells, agents
and what runs in them — counting as now (a PowerShell's moves with
`Set-Location` included when it has the working-directory hook, see
`session.spawn`'s `shell` and the shell wrapper); each
session's `cwd` at its
`last_activity_at`; the `cwd` of the 50 most recently updated
conversations at their `updated_at`. A directory counts once, with its
newest time; task records are not candidates. Of the 200 newest
directories, those inside a working tree are resolved to its top level
(`git rev-parse --show-toplevel`) and the 20 newest working trees are
inspected; one without a remote is dropped. Each item has the fields of
the app's sh script (`@@REPO`):

| key | value |
|---|---|
| `path` | top-level directory |
| `remote_url` | `origin`'s URL, else the first remote's |
| `ssh_host?` | an ssh remote's real host name (`ssh -G <host>`'s `hostname`; the remote may name an `~/.ssh/config` alias) |
| `branch` | current branch, or the abbreviated commit when `detached` |
| `detached` | HEAD is detached |
| `default_branch?` | from `refs/remotes/origin/HEAD` |
| `changes` | lines of `git status --porcelain` (changed and untracked files) |
| `ahead?`, `behind?` | commits against the upstream branch; absent without one |
| `last_active_at?` | RFC 3339 (UTC) |

Remotes that are not on GitHub are listed too (the app drops them).
`git_missing: true` (and no repos) when git is not on PATH.

`github.activity` asks gh about one repository: `repo` is gh's `-R` value
(`OWNER/REPO`, `HOST/OWNER/REPO` on GitHub Enterprise), `host` the host
gh must be signed in to, `path` the working tree (gh's working directory),
`branch` a feature branch whose pull request is wanted. A `repo`, `host`
or `branch` that is malformed or starts with `-` is `bad_request`.
Without gh, `gh: "missing"` with `package_manager: "winget"` when winget
is on PATH. When `gh auth status --hostname <host>` fails,
`gh: "unauthenticated"` with its output on one line in `auth_message`.
Otherwise these gh calls run in parallel, as the app's sh script does:

- `gh repo view <repo> --json description,isPrivate` → `description`
  (trimmed; absent when empty) and `is_private`;
- `gh pr list -R <repo> --state open --limit 30 --json
  number,title,author,headRefName,isDraft,updatedAt,url,reviewDecision,labels`
  → `pulls`;
- `gh issue list -R <repo> --state open --limit 30 --json
  number,title,author,updatedAt,url,labels,blockedBy,blocking`, again
  without `blockedBy,blocking` when that fails (older gh and GitHub
  Enterprise) → `issues`;
- with `branch`: `gh pr view <branch> -R <repo> --json
  number,title,url,state,isDraft,reviewDecision,statusCheckRollup,author,updatedAt,headRefName`
  → `current`, absent when it fails (no pull request for the branch).

`pulls`, `issues` and `current` are gh's JSON as is. When a list fails,
`pulls_error` / `issues_error` holds gh's error output on one line instead;
exactly one of each pair is present. gh runs with
`GH_NO_UPDATE_NOTIFIER=1` and `GH_PROMPT_DISABLED=1`.

`github.status` reports whether issues and pull requests (a host's tasks)
are still open — `github.activity` lists open ones only. Each item names
one by `repo` (gh's `-R` value, as above), `kind` (`issue` or `pr`) and
`number`; an issue item may add `branch`, echoed in its answer. A pull
request item gets the pull request's `state`, an issue item the issue's
`state` and, with `branch`, `pr_state`: the state of that branch's pull
request (`gh pr view <branch>`), absent when there is none. States are gh's (`OPEN`, `CLOSED`,
`MERGED`). The gh calls (`gh issue view` / `gh pr view <number> -R <repo>
--json state`) run in parallel; an item gh cannot answer (no gh, not
signed in, not found) is left out of `items`, the rest keep their order.
An unknown `kind`, a `number` below 1, or a `repo` or `branch` that is
malformed or starts with `-` is `bad_request`.

#### Host screen

The host screen (ホストの画面, ADR 0008) shows in an app tab what a
browser an agent drives draws, an Android emulator, or an iOS simulator
(on macOS). Each such tab has its own SSH connection and its own
`stagent bridge` (after `hello`), separate from the hub bridge: frames
would otherwise delay every other notification. Served on every OS
without a capability, except the `ios` kind (capability
`host_screen_ios`, macOS only); a stagent before 0.11.0 answers
`unknown_method`, and the app offers the stagent update (as for an older
`hello.version`). A bridge under a coding agent serves them too (ADR
0004): they change no setting, and an agent can drive its browser anyway.

| method | params | result |
|---|---|---|
| `host_screen.list` | `{cdp_ports?: int[]}` | `{browser: Section<Browser>, android: Section<Emulator>, ios?: Section<Simulator>}` |
| `host_screen.start` | `{target: Target}` | `{}` |
| `host_screen.ack` | `{seq}` | `{}` |
| `host_screen.pause` | – | `{}` (since 0.12.0) |
| `host_screen.resume` | – | `{}` (since 0.12.0) |
| `host_screen.stop` | – | `{}` |
| `host_screen.input` | `Input` | `{}`; an iOS simulator's `text`: `{via, app?}` (since 0.13.0) |

| notification | params |
|---|---|
| `host_screen.frame` | `{seq, data, width, height}` — `data`: one JPEG, base64; `width`, `height`: the target's size in its own coordinates (CSS px for a browser, px for an Android emulator, pt for an iOS simulator), which change with its viewport, rotation or orientation |
| `host_screen.state` | `{state: "live"\|"gone", title?, url?}` |

`list` has one key per kind of target this stagent serves; 0.11.0
serves `browser` and `android`, and `ios` on hosts announcing
`host_screen_ios`.

- `Section<T>`: `{items: T[], missing?: {code, message}, note?: {code, message}}`. `missing`
  tells why the kind's tool lists nothing (then `items` is `[]`):
  `adb_not_found` (`android`: adb is not installed), `xcode_not_found`
  (`ios`: xcrun cannot find simctl, no Xcode is selected) or
  `simctl_failed` (`ios`: simctl failed, e.g. Xcode's license is not
  accepted yet; `message` has its error). The app shows `message` for a
  code it does not know.
- `Section<T>` also has `note?: {code, message}`: a remark on why the
  section may lack targets the person sees running, told even with
  `items` (and alongside `missing`). `wsl` (`browser` and `android`, since
  0.12.0): stagent runs in Windows' WSL (`/proc/sys/kernel/osrelease`
  names `microsoft`, or `/proc/sys/fs/binfmt_misc/WSLInterop` exists),
  where only WSL is searched, so the browsers and emulators on the Windows
  side are not listed. The app shows `message` for a code it does not
  know.
- `Browser`: `{pid?, tool, headless?, port?, manual?, connectable, reason?,
  pages: [{target_id, title, url}]}`. `tool` is `agent-browser`,
  `playwright-mcp`, `chrome-devtools-mcp`, `playwright`, or `null` when
  unknown. `connectable: false` comes with `reason`: `pipe_only` (see
  below; no `port`, `pages: []`). `manual: true`: `port` is one of
  `cdp_ports`.
- `Emulator`: `{serial, avd, connectable, reason?}`. `serial` is adb's
  (`emulator-<console port>`), `avd` the virtual device's name (`""` when
  unknown). `reason: "no_grpc"` with `connectable: false`: the emulator
  has no gRPC port (started with `-port` only, say) and cannot be shown.
- `Simulator`: `{udid, name, runtime, connectable, reason?}`. `runtime`
  is readable (`iOS 26.2`). `connectable` is `false` with `reason:
  "companion_not_found"` when idb_companion is not installed.
- `Target`: `{kind: "browser", port, target_id}` / `{kind: "android",
  serial, avd?}` / `{kind: "ios", udid}`. An emulator is shown by its
  `serial`; its `avd` (the item's, since 0.12.0) tells it again when it
  restarts on another serial (see "Coming back"). Without `avd` (an app
  before 0.12.0), the AVD on `serial` when `start` runs is followed.

**Browsers listed.** Processes of the user stagent runs as (another
user's browser is never listed: a CDP port has no authentication) whose
executable is Chromium-family (Chrome, Chrome for Testing,
chrome-headless-shell, Chromium, Edge, Brave) and whose arguments have
`--remote-debugging-port`; their child processes (`--type=`) are not.
The port is `N` of `--remote-debugging-port=N`, or for `0` the first line
of `DevToolsActivePort` in its `--user-data-dir` (relative to the
process's working directory when relative; such a browser without
`--user-data-dir` is not listed). A browser is listed only when
`http://127.0.0.1:<port>/json/version` answers as CDP; stagent never
tries ports it did not find this way (9222 and the like). `pages` are the
`type: "page"` entries of `/json/list` (`id`, `title`, `url`), in its
order. `headless`: `--headless` is among the arguments or the executable
is a headless shell. `tool` is told by the `--user-data-dir` path:
`agent-browser-chrome` → `agent-browser`, `ms-playwright-mcp` →
`playwright-mcp`, `chrome-devtools-mcp` → `chrome-devtools-mcp`,
`playwright_chromiumdev_profile` → `playwright`. Items are ordered by pid;
no browser gives `items: []`. On Linux, Chromium writes its command line
over its arguments as one string (the executable and arguments joined by
spaces, one NUL at the end of `/proc/<pid>/cmdline`); such a command line
is split into switches at ` --`, so a `--user-data-dir` keeps its spaces,
and when the last switch runs on with arguments after it (`about:blank`)
the `--user-data-dir` cut before each space is tried for
`DevToolsActivePort`, the longest first.

**Pipe only.** A browser started with `--remote-debugging-pipe` and
without `--remote-debugging-port` (Chrome DevTools MCP and Playwright do
so by default) opened no port: it is listed with `connectable: false`,
`reason: "pipe_only"`, its `pid`, `tool` and `headless`, no `port` and
`pages: []`. The app tells what to add to the tool's own configuration;
stagent never changes a tool's configuration. With both arguments the
browser is listed by its port as above.

**Ports named by hand.** `cdp_ports` are ports on the host's 127.0.0.1
the app names for a browser stagent cannot find by its arguments (say a
container's Chromium published on a host port); no host or address is
taken. Each is checked with `http://127.0.0.1:<port>/json/version`; one
that answers as CDP is listed with `manual: true` (and its pages), also
without any process found for it. A port of a browser found above is
listed once, that browser's item with `manual: true`. A port that does
not answer as CDP (nothing listening, or something else) is not listed:
the app tells the user it could not be checked. Ports named by hand come
after the browsers found, in the order given; repeats are listed once.

**Showing a page.** `start` connects to the page target's WebSocket on
`127.0.0.1:<port>` and answers `{}`; then come `host_screen.state
{state: "live", title, url}`, one frame of `Page.captureScreenshot` and
the frames of `Page.startScreencast` (JPEG, quality 70; Chrome sends one
when the page is redrawn). Title and URL changes (a navigation, a new
title) are sent as another `state: "live"`. When the page target closes
(or its WebSocket drops) `state: "gone"` is sent and the stream ends; no
frame follows. stagent sends only page-level requests and never changes
the agent's viewport.

**Emulators listed.** The `emulator-<port>` entries of `adb devices -l`
(physical devices — USB serials, `ip:port`, mDNS names — are never
listed), ordered by port. adb takes any process listening on an odd port
of 5555–5585 for `emulator-<port-1>`: an entry that is `offline` and has
no discovery file (below) is not an emulator, is not listed and gets no
`emu avd name`; `start` on it finds no emulator there (see the errors).
One `offline` with a file
(booting) or in another state without one (`-port` only, say) is
listed. adb is the first found of: the PATH
of the user's login environment (as `session.spawn` gets it),
`$ANDROID_HOME/platform-tools`, `$ANDROID_SDK_ROOT/platform-tools`, and
the default SDK (`~/Library/Android/sdk` on macOS, `~/Android/Sdk` on
Linux, `%LOCALAPPDATA%\Android\Sdk` on Windows); none →
`missing: {code: "adb_not_found"}`. Each is matched by its serial with
the file a running emulator advertises itself in,
`avd/running/pid_<pid>.ini` (keys `port.serial`, `avd.name`,
`grpc.port`, `grpc.token`) under `$XDG_RUNTIME_DIR` on Linux,
`~/Library/Caches/TemporaryItems` on macOS and `%LOCALAPPDATA%\Temp` on
Windows. On Linux without `$XDG_RUNTIME_DIR` (a session without logind,
`su`) the emulator writes it under `/run/user/<uid>` when that exists,
else under its user directory (`$ANDROID_EMULATOR_HOME`, else
`$ANDROID_PREFS_ROOT` or `$ANDROID_SDK_HOME`, their `.android` when it is
a directory, else `~/.android`); stagent reads both (since 0.13.1, #705).
An emulator that stops
leaves its file behind: a file whose `<pid>` is no running process
(signal 0 on Linux and macOS, opening the process on Windows) is not read,
and left as it is (since 0.13.0, #679). An emulator without such a file
or without `grpc.port` there is `no_grpc` — also one started with `-port`
only on the serial of an ended emulator's file; its `avd` is then the
answer of `adb -s <serial> emu avd name`.

**Showing an emulator.** `start` reads the emulator's discovery file
and connects to its gRPC port on the loopback (`[::1]`, then
`127.0.0.1`), with `authorization: Bearer <grpc.token>` when the file
has a token, reads the display's size (`getStatus`, `hw.lcd.width` and
`hw.lcd.height`) and answers `{}`; then come `host_screen.state {state:
"live", title: <avd>}` and the frames of `streamScreenshot`: RGB888 at
half the display's resolution (width and height both half the longer
side, so either orientation fits), encoded by stagent to JPEG, quality
70. The emulator sends an image about 20 times a second whether the
display changed or not: an image with the same size, rotation and pixels
as the last one encoded is dropped before encoding, so a still display
sends no frame (#634). `width` and `height` are the display's px as the
image shows it: they swap when the emulator turns. An image of the
display turned off sends no frame. When the emulator stops (its
screenshot stream ends and `getStatus` no longer answers) `state:
"gone"` is sent (see "Coming back"). There is no `adb screencap`
fallback.

One stream per bridge connection: `start` while streaming stops the
current stream first (the tab's "pick another"); `stop` ends it (without
one it does nothing). Closing the bridge ends it too.

**iOS simulators** (macOS, `host_screen_ios`). Listed: the booted
simulators of `xcrun simctl list devices --json` whose runtime is iOS
(iPhone and iPad; watchOS, tvOS and visionOS are not), ordered by name,
then runtime. `start` runs, for the stream, `idb_companion --udid <udid>
--only simulator --grpc-domain-sock <socket> --idle-shutdown-time 5
--terminate-offline true --log-level info` (#630; idb_companion is looked
up on the login shell's `PATH`, then the usual tool directories such as
`/opt/homebrew/bin`), with its socket in a new directory under the temp
directory that only the user can enter (no TCP port), waits until it
describes the simulator and opens its `video_stream` (MJPEG at scale
0.5 — half the simulator's pixels —, quality 0.5 and `fps` 0: frames
sent when the screen is redrawn; ADR 0008 "通信量と電池"); it answers
`{}`, then come `host_screen.state {state: "live", title}` (`title`: the
simulator's name) and the frames. Every payload is one JPEG, sent as
is; `width` × `height` is the simulator's screen in points (its
`describe`), turned like the picture. stagent stops the companion
(SIGTERM, a kill after 2 seconds) with the stream: on `stop`, the next
`start` and when the bridge exits; `--idle-shutdown-time` ends it 5
seconds after its last request closed should stagent die first (while
the video stream is closed for a pause, stagent asks it `describe`
every 5/3 seconds so it stays). When the simulator shuts down, its
companion exits (`--terminate-offline`) and `state: "gone"` is sent
(see "Coming back"), paused or not.

**Coming back** (since 0.12.0, #633). An Android emulator or iOS
simulator that stops does not end the stream: after `state: "gone"`
stagent looks for it again every 3 seconds — for an emulator, the
emulators `adb devices -l` lists (all, when adb fails) whose discovery
file names the target's AVD and a gRPC port, on any serial (a restart
may change it), lowest first; for a simulator, the booted simulator
`udid` in `xcrun simctl list devices --json`. Once found, stagent
connects again (an emulator's `grpc.port` and `grpc.token` read anew; a
simulator's companion started again) and sends `state: "live"` with the
title, then frames as after `start` (a full frame first, `seq`
continuing). Another AVD on the same serial, or another simulator, is
not the target. `start` of an emulator or simulator that is not running
answers `{}` all the same and sends `state: "gone"` (or `live` at once
for an AVD already running on another serial), then looks for it the
same way: an app reconnecting after the host restarted it takes the same
path. Looking stops (and a companion started meanwhile is stopped) with
`stop`, the next `start` and when the bridge exits. A browser page that
closes stays gone.

**Flow control.** `seq` grows by one per frame over the bridge
connection (a new stream continues it). The app answers every frame it
received with `host_screen.ack {seq}`. At most 10 frames a second are
sent (since 0.12.0): two frames are at least 100 ms apart. While two
frames wait for their ack, or less than 100 ms passed since the last
frame, no frame is sent: of the frames taken meanwhile only the newest
is kept, and sent as soon as an ack frees a place and 100 ms passed.
When two frames stay unacknowledged for 2 seconds the capture is
paused; the next ack sends the kept frame and resumes it, beginning
with a full frame: a fresh `Page.captureScreenshot` (the screencast
only sends redrawn pages), a new `streamScreenshot` (whose first image
is the whole display), or for a simulator the newest frame again, then
a new video stream (it sends redrawn screens only). An
ack for a seq not waiting (repeated, or of a replaced stream) is
answered `{}` and changes nothing; acks keep being answered after
`gone` until `stop` or the next `start`. A `live` after `gone` (see
"Coming back") waits for no frame sent before it.

**Pause** (since 0.12.0, #634; ADR 0008 "通信量と電池"). The app sends
`host_screen.pause` when the tab is not seen (another tab opened, the
app in the background, the phone's screen off) and `host_screen.resume`
when it is seen again. `pause` stops capturing and drops the kept frame:
a browser gets `Page.stopScreencast` (its WebSocket stays open), an
Android emulator's `streamScreenshot` is cut, an iOS simulator's video
stream is closed (its companion keeps running). `resume` captures again
as after a stall: one full frame sent at once (`Page.captureScreenshot`;
the emulator's first image; the simulator's newest frame), then the
stream (`Page.startScreencast`, a new `streamScreenshot`, a new video
stream). Acks are answered while paused. A pause and a stall both hold
the capture: it resumes once neither does (a `resume` while stalled
leaves it to the next ack). A target that comes back while paused (see
"Coming back") sends `state: "live"` and stays paused. `pause` of a
paused stream and `resume` of a stream not paused do nothing and answer
`{}`; the next `start` begins unpaused, so a tab not seen sends `pause`
after `start`'s answer. A stagent before 0.12.0 answers
`unknown_method` (and sends at most as many frames as the acks allow).

**Input.** `host_screen.input` operates the target being shown. Points are
fractions of the frame (`0` at its left and top edge, `1` at its right
and bottom one); stagent turns them into the target's coordinates, so the
app needs no target's size beyond the frame's.

- `{type: "tap", x, y}`
- `{type: "drag", points: [{x, y}], duration_ms}` — 2 to 1000 points, in
  order, the moves spread over `duration_ms` (0–10000). The app sends it
  once, when the finger that long-pressed and moved is lifted.
- `{type: "scroll", x, y, dx, dy}` — at `x`, `y`; `dx`, `dy` (±10) are how
  far the two fingers moved, as fractions of the frame's width and height
  (the page moves with them).
- `{type: "text", text, enter?, x?, y?}` — `text` may be empty with
  `enter`. `x`, `y` are the user's cursor (0–1, like a tap): an iOS
  simulator opens its edit menu there (both 0 or missing: no cursor).
  The other kinds ignore them.
- `{type: "key", key: "back"|"home"|"recents"}`
- `{type: "navigate", action: "back"|"forward"|"reload"|"url", url?}` —
  `url` for `action: "url"`.

What each kind of target takes (another type → `unsupported`):

| kind | takes |
|---|---|
| `browser` | `tap`, `drag`, `scroll`, `text`, `navigate` (not `key`) |
| `android` | `tap`, `drag`, `scroll`, `text`, `key` (not `navigate`) |
| `ios` | `tap`, `drag`, `scroll`, `text` (any script), `key: "home"` (not `back`, `recents`, `navigate`) |

`host_screen.input` answers `{}`, but for a `text` with `text` on an iOS
simulator: `{via, app?}` (since 0.13.0, #649). `via` is how it went in:
`cmd_v` (pasted with ⌘V), `edit_menu` (pasted with the edit menu's
Paste), `keys` (typed); `app` is the name of the app in front (absent on
the home screen, or when the companion cannot tell). The app shows, after
an `edit_menu`, how to let the app paste from other apps (below).

A browser gets only `Input.*` and `Page.navigate`,
`Page.getNavigationHistory`, `Page.navigateToHistoryEntry`,
`Page.reload` (never `Emulation.*`, `Target.createTarget`,
`Browser.close`, nor an answer to a dialog). A point is CSS px from the
metadata of the latest screencast frame: `x × deviceWidth /
pageScaleFactor`, `(y × deviceHeight − offsetTop) / pageScaleFactor` (before
the first, of the `Page.captureScreenshot` frame's visual viewport).
`tap`: `Input.dispatchMouseEvent` `mousePressed` then `mouseReleased`
(left button, `clickCount: 1`). `drag`: `mousePressed` at the first point,
`mouseMoved` through the others, `mouseReleased` at the last. `scroll`: one
`mouseWheel` with the deltas the other way (`-dx`, `-dy` in CSS px).
`text`: `Input.insertText` (any script, Japanese included), then for
`enter` an Enter `Input.dispatchKeyEvent` (`keyDown`, `keyUp`). `navigate`:
`back` and `forward` go to the neighbouring entry of
`Page.getNavigationHistory` with `Page.navigateToHistoryEntry` (nothing
without one), `reload` is `Page.reload`, `url` is `Page.navigate`. An
input answers once the browser took it; a person's and an agent's inputs
both go in (no lock).

An Android emulator gets touches through gRPC `sendTouch` (one finger,
pressure 1024 while down, 0 to lift) and keys and text through `adb -s
<serial> shell input`, never gRPC `sendKey` (an AVD with
`hw.keyboard=no` drops its keys) nor a change to the AVD. A point is the
display's px in its natural orientation (`hw.lcd.width` ×
`hw.lcd.height`): the frame's fractions turned back by the rotation of
the latest `streamScreenshot` image. `tap`: down and up at the point.
`drag`: down at the first point, moves through the others, up at the
last. `scroll`: one swipe from `x`, `y` to `x + dx`, `y + dy` (kept on
the frame) in 150 ms (the content moves with the finger). `key`:
`input keyevent KEYCODE_BACK`, `KEYCODE_HOME`, `KEYCODE_APP_SWITCH`
(`recents`). `text`, ASCII or not, is put on the clipboard with gRPC
`setClipboard` and pasted with `input keyevent KEYCODE_PASTE`; never
`input text`, which goes through the IME (a Japanese Gboard turns ASCII
into kana). The emulator's clipboard is overwritten each time. `enter`
adds `input keyevent KEYCODE_ENTER` after the paste; `enter` with no
`text` sends only `KEYCODE_ENTER` and leaves the clipboard alone. An
input answers once adb or the emulator took it (adb's keys take a few
hundred ms).

An iOS simulator gets HID events on its companion's `hid` stream, opened
by the first input and kept open for the stream (reopened once when the
companion ended it; no program is started per input). A point is the
fraction times the frame's `width` and `height` (pt). `tap`: a touch down
and up. `drag`: a touch down at the first point, again at each next one
spread over `duration_ms`, and up at the last; cut short (the bridge
ending), the touch is lifted where it is. `scroll`: one swipe, a touch
moving from `x`, `y` by `dx`, `dy` (its end kept on the screen) in 10
steps 16 ms apart, then lifted without a pause (the content may glide on
as after a fast swipe). `key: "home"`: the HOME button down and up. An
input answers once its events are sent; `stop` waits up to a second for
the companion to perform them before stopping it.

`text` on an iOS simulator (since 0.13.0, #649) is pasted from the
simulator's clipboard (`xcrun simctl pbcopy`, overwritten each time), as
a paste skips the Japanese input (a `ja` simulator types ASCII keys
through romaji: `example.com` → 「えぁmpぇ。cおm」). How depends on
the app in front — the root element of the companion's
`accessibility_info` gives its pid and name, `list_apps` its bundle id —
and on its 「ほかのアプリからペースト」 (Paste from Other Apps, iOS 16+),
which stagent reads from the simulator's
`data/Library/TCC/TCC.db` (`kTCCServicePasteboard`, `sqlite3
-readonly`) and never changes:

- **Allowed** (`auth_value` 2): ⌘V at the caret (`cmd_v`), ASCII too.
  iOS then shows "<App> pasted from CoreSimulatorBridge" for about 2.65 s
  and drops every key meanwhile, so no key (the `enter`'s Return, the
  next text, another ⌘V) goes until 3 s after the ⌘V: such an input
  answers that much later. Taps are not held.
- **No row** yet (the app never asked), whatever the text: first a ⌘V,
  which iOS refuses but which adds the row, and with it the app's item
  in Settings; then as not allowed below, the text copied again for the
  edit menu (which does not paste what iOS just refused).
- **Not allowed** (ask, deny, no row, TCC.db unreadable, no app in
  front), text all ASCII (tab, newline included): the 英数 key (LANG2,
  HID 0x91) leaves the Japanese input, then the text's keys of a US
  keyboard (`keys`; Shift held for capitals and the upper symbols;
  newline is Return, tab is Tab).
- **Not allowed**, any other text: a tap at `x`, `y` (the caret goes
  there; on the caret the edit menu shows), and a second one if 1 s
  passes without the menu (the first only focused the field or moved the
  caret); then a tap on the menu's Paste (`edit_menu`): the element with
  traits `MenuItem` labelled Paste in the simulator's language (the
  `Paste` of UIKitCore's `Localizable.strings` of its runtime, for its
  first `AppleLanguages`), in Japanese 「ペースト」 or in English
  `Paste`. Without the menu's Paste (or without `x`, `y`) the text
  stays on the clipboard and the input fails `paste_by_hand`: the user
  long-presses the field in the simulator and chooses Paste.

`enter` adds Return after the text (after an `edit_menu`, 0.5 s later,
once the paste inserted). What the user sees and accepts: UIKit may add
a space before or after letters and digits it pastes (smart insert, as
when a person pastes); in a simulator whose hardware keyboard is not
connected (the software keyboard shows), LANG2 does nothing and ASCII
keys go in unconverted but uncommitted (underlined), so the first Return
only commits them. Pasting is faster and surer once the user allows the
app in the simulator: Settings → アプリ (Apps) → 〈App〉 →
ほかのアプリからペースト → 許可 (Allow); an app without the item
(Safari) stays on the edit menu.

**Order.** `start`, `stop` and `input` run in order on one queue: inputs
keep the app's order, and none reaches a stream being replaced. `list`
runs on another (probing ports never delays them). `ack`, `pause` and
`resume` take no queue: they are answered at once in the order received,
never behind a slow `start` or `input`, so frames keep coming and a
`resume` shows the target at once. After an `input` ends `unavailable`, the inputs that follow
fail `unavailable` at once without reaching the target, until it sends a
frame or another `start`: a page held by a dialog does not hold the queue.

**Errors.** `list`: a port of `cdp_ports` outside 1–65535 →
`bad_request`. `start`: an unknown `kind` → `bad_request`; `ios` on a
host not announcing `host_screen_ios` → `unsupported`; a `port` outside
1–65535 or an empty `target_id` → `bad_request`; no CDP answering
`/json/list` on the port, or no page target `target_id` there →
`not_found`; the page's WebSocket cannot be opened → `unavailable`.
`android`: an empty `serial` → `bad_request`; an emulator that is
`no_grpc` → `unavailable`; the port refusing the token → `unavailable`;
no gRPC answering on the port of its discovery file while `adb devices`
lists `serial` (in the `device` state) → `unavailable` (`no_grpc`, since
0.13.0, #679). An emulator not running (no discovery file and not in
`adb devices`, a discovery file with no gRPC answering on its port and
`serial` not in `adb devices`, or another AVD on `serial`) is not an
error: `{}`, then `gone` (see "Coming back"); only without an `avd` to
look for (an app before 0.12.0, and no emulator on `serial`) →
`not_found`. `ios`: an empty `udid` →
`bad_request`; no Xcode (xcrun cannot find simctl) → `not_found`;
idb_companion not installed, or it exits or does not describe a booted
simulator within 10 seconds → `unavailable`; a simulator `udid` not
booted, or simctl failing to list (a timeout, say), is not an error:
`{}`, then `gone`. `ack`,
`pause` and `resume` without a stream (none started, or after `stop`) →
`bad_request`.
`input`: without a stream (also while `gone`) → `bad_request`; a shape
wrong for its type (a point outside 0–1, the text's `x`, `y` too, an
unknown `type`, `key` or `action`, a drag of fewer than two points,
`text` without `text` or `enter`, `url` without `url`) → `bad_request`;
a type the target does not take → `unsupported`; the browser refusing
the call (an invalid URL) → `bad_request`; the page not answering within
10 seconds (gone, a dialog open) or `Page.navigate` failing to load
(`errorText`) → `unavailable`; for a simulator, its companion's `hid`
stream failing again after reopening, or `simctl pbcopy` failing →
`unavailable`; a text an iOS simulator could not paste (no edit menu
with Paste at the cursor; the text is left on its clipboard) →
`paste_by_hand` (since 0.13.0); an input after one that ended
`unavailable`, before the next frame → `unavailable` (see Order).

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
current position), `P - since <= 1 MiB` and the bytes `[since, P)` are
still retained; a size change after `since` does not matter, since those
are the bytes the client would have received had it stayed attached
(holders of stagent 0.18.2 and earlier also require that the session size
did not change at or after `since`). The result then has `resumed: true`
and the first `output` is `{reset: false, data: bytes [since, P), end: P}`
— omitted when `since == P` — so the client's terminal continues exactly
where it stopped, scrollback included. Otherwise `resumed` is false and
the first `output` is the usual `reset: true` snapshot. Absent `since`
never resumes. Screen mode ignores `since` and its frames carry no `end`.

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
continue`); or claude's workspace trust prompt, whose options have no
numbers (the dialog titled `Accessing workspace:` with `❯ No, exit` /
`Yes, I trust this folder` and `Enter to confirm · Esc to cancel`; its
Enter would pick No and end claude) — nothing of it is written and the
request fails with `menu_open` (the app's copy of the screen lags behind
the host's). The trust prompts are answered in the terminal; stagent
writes no trust. claude and Codex hold the initial prompt given on their
command line until the folder is trusted and send it themselves. The
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
| `watch` | `{since}` | `{sessions[], approvals[], unwrapped[], tasks[], seq, missed[], truncated}` then `event` / `session.updated` / `session.removed` / `unwrapped.updated` / `task.updated` / `task.removed` notifications |
| `sessions.list` | – | `{sessions[]}` |
| `conversations.list` | `{harness?[], limit?}` | `{conversations[]}` |
| `transcript.get` | `{session_id? \| harness+conversation_id? \| path?, limit?, before?}` | `{path, harness, messages[], cursor}` |
| `transcript.subscribe` | `{session_id? \| path?}` | `{}` then `transcript` notifications |
| `transcript.unsubscribe` | same | `{}` |
| `approvals.list` | – | `{approvals[]}` |
| `config.get` | – | `{config, notify: {last_error}}` |
| `config.set` | `{config}` — a JSON Merge Patch (RFC 7396) of `Config` | `{config, notify: {last_error}}` (the effective config, with defaults applied) |
| `notify.test` | `{channels?: ["ntfy" \| "webhook" \| "chat"]}` | `{}`; pushes a test right away to the enabled channels among `channels` (absent or empty: every enabled channel) and emits a `notification` event (reason `test`); `bad_request` for a channel name not in the list; `not_configured` (nothing sent, no event) when none of them is enabled; `unavailable` with the failure when a channel failed |
| `presence.set` | `{foreground}` | `{}`; the app is (not) in the foreground (see "Push notifications") |
| `task.list` | – | `{tasks[]}` — every task, in the order they were created (see "Tasks") |
| `schedule.list` | – | `{schedules[]}` — every schedule as `ScheduleEntry`, in the order they were created (see "Schedules"; `schedules`) |
| `schedule.create` | `{schedule}` | `ScheduleEntry` of the new schedule; refused with `agent_refused` to a coding agent's process tree |
| `schedule.runs` | `{id}` | `{runs[]}` — the schedule's runs, newest first |
| `schedule.preview` | `{schedule}` | `SchedulePreview` `{argv[], cwd, next[], next_offsets[], tz_offset, warnings[], cwd_kind?, task_id?, host_clock?}` — what the schedule would run and when, nothing saved |
| `schedule.update` | `{id, schedule}` | `ScheduleEntry` — what the schedule runs and when, replaced; refused with `agent_refused` to a coding agent's process tree |
| `schedule.remove` | `{id, stop_run?}` | `{}` — the schedule and its runs removed (`stop_run`: its running run stopped first); refused to a coding agent's process tree |
| `schedule.pause` / `schedule.resume` | `{id}` | `ScheduleEntry`; refused to a coding agent's process tree |
| `schedule.run_now` | `{id}` | the run started (`trigger: now`); `busy` while the previous run runs; refused to a coding agent's process tree |
| `schedule.output` | `{id, run_id, session_id?, before?, max_bytes?}` | `{data, start, end, first, gone?}` — a page of a try's output (`session_id`; omitted: the newest try) from the disk, as `session.scrollback`; `gone: true` once it is no longer kept |

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
must be http(s), a chat URL one of the formats under "Chat
destinations", and `notify.click_page` empty or an absolute https URL
with no query and no fragment); otherwise the call fails with
`bad_request` and nothing
is written. The result, like `config.get`, is the effective config with
defaults filled in. Example: `{"config": {"disable_handoff": true}}`
turns handoff off for `stagent run --handoff=auto` without touching any
other setting.

`repos` holds settings per source checkout, keyed by its canonical path
(the `repo` of `Task` and `task.preview`): `{"repos": {"<repo>":
{"worktree_include": [<pattern>…]}}}` sets one checkout's app patterns
of files to copy (see "Files to copy") and `{"repos": {"<repo>": null}}`
removes them; other checkouts' entries stay. `config.set` refuses
(`bad_request`, nothing written) an entry it sets whose key is not an
absolute path or not clean (`filepath.Clean` would change it; a trailing
separator) and a pattern that is empty, starts with `!`, is absolute or
has a `..` or `.git` component. Whether the path exists does not matter.

`worktree_folder` (since 0.14.0, #877) is the worktree folder (worktree の置き場): one folder
per host under which task worktrees go, `<worktree_folder>/<checkout
name>/<name>` for a named task and `<worktree_folder>/<checkout
name>/<number>-<slug>` for an Issue or PR (see "Creating a task").
`config.set` expands a leading `~` (`~`, `~/…`, `~\…`) with the daemon
user's home and stores the path cleaned (`filepath.Clean`); the result
must be absolute, else `bad_request` "worktree_folder must be an absolute
path" and nothing is written. `""` removes it, as `null` does. Whether it
exists does not matter (`task.create` makes it), and changing it never
moves existing worktrees.

`worktree_watch` (since 0.18.0, #1122) lists the watched folders (見るフォルダ,
see "Worktrees"): `config.set` expands a leading `~` of each with the
daemon user's home and stores them cleaned and each once; one that is
not absolute is `bad_request` and nothing is written. `worktree_repos`
(since 0.18.0) lists the source checkouts `worktree.list` found; the
bridge writes it, and `config.set` refuses a path in it that is not
absolute and clean. For both an empty list removes the key, as `null`
does. Whether the paths exist does not matter here.

Every push (ntfy, webhook, chat, including `notify.test`) is titled with the
host it comes from: `notify.host_label`, or the host name when it is empty
(no prefix when that is unknown too). A single notification reads
`<label> · <title>` (e.g. `開発機 · claude · api`), a digest
`<label>: <title>`. A notification about a session tied to a task
(`Session.task_id`) puts the task first, its number or, for a named
task, its name: `<label> · #N <title>` (e.g. `開発機 · #493 claude ·
agent-orchestra-493`), `<label> · <name> <title>`; a digest of several
notifications does not. The in-app `notification` event keeps the plain
title.

A notification about one session carries its `session_id`. On ntfy its
sequence ID (`X-Sequence-ID`) is the session id — or, for an id outside
ntfy's rule (1–64 of `[A-Za-z0-9_-]`; stagent's never are), the first 32
hex digits of its SHA-256: a newer notification of the session replaces
the older one. A digest and `notify.test` have no sequence ID.

#### Tasks

A task (タスク; `Task` below) is an Issue or PR, or a name the user gave
(`kind: name`, a named task, since 0.14.0), being worked on in a worktree of a source
checkout (元のチェックアウト) or, for an Issue or PR `in_place`, in the
checkout itself. The daemon keeps them in `state/tasks.json` (mode 0600,
apart from `config.json`); only the daemon writes it, and only `uninstall
--level purge` removes it. A task's key is its checkout's canonical path
(`repo`) and its `number` — or, for a named task, its `name` (exactly,
case-sensitive): one task per number, one per name, and a name never
matches a number. `repo` is made by
stagent — `git rev-parse --show-toplevel` with symlinks resolved, on
Windows with an upper-case drive letter; a linked worktree chosen as the
checkout stays itself — and the app uses it as it is.

`task.list` answers every task in the order they were created; `watch`'s
`tasks[]` is the same list, and `task.updated` (the whole `Task`, added or
changed) and `task.removed {id, kept_branch?}` follow the changes. On every `task.list`
the daemon checks each `ready` task: when its worktree directory is gone,
or `git worktree list` of `repo` no longer lists it (git answering with an
error counts too; no git at all checks nothing), the task becomes
`missing`. A `preparing` task whose prepared session (`session_id`) ended,
or did not register within 30 s of the task being prepared (or of the
daemon starting, for its holder to come back), becomes `failed` with
`error: "interrupted"`; `task.list` and `watch` see it so, and the daemon
checks again when it starts. A task still `removing` when the daemon
starts is `ready` again with `error: "interrupted"`, keeping its `stage`.

`Session.task_id` is the task a session belongs to, decided by the
daemon: a worktree task's sessions are those whose `cwd` is its
`worktree` or inside it (the innermost worktree wins), checked when the
session registers and, for the live sessions, when the task is
registered (`session.updated` follows). An `in_place` task takes only its
prepared session and the sessions `session.spawn {task_id}` started for
it, never one by its `cwd`. A task that is forgotten (`task.forget`) unties
its sessions, ended ones still listed included (`session.updated`).

Local IPC only (the bridge and `stagent task`, never forwarded from the
app; refused with `agent_refused` to a coding agent's process tree):

| method | params | result |
|---|---|---|
| `task.register` | `{repo, kind, number, name?, title, url, branch, base, worktree, in_place?, session_id}` | `{task, existed, session?}` |
| `task.progress` | `{id, stage, error?, session_id?}` | `{}` |
| `task.forget` | `{id, kept_branch?}` | `{}` |

`task.register` adds a task, atomically by its key; `repo` and `worktree`
are canonical paths (`worktree` is `repo` when `in_place`), `session_id`
the session the caller starts to prepare the task. Without `existed` the
task is new — or was `failed` (or interrupted) and is `preparing` again
with this `session_id`, `title`, `url` and, when given, `branch` and
`base` — and the caller starts that session. With `existed` nothing is to
be started: the task is `ready` (or `missing`, `removing`), or still
`preparing`, then with its live prepared session in `session` (absent
while that session has not registered yet). An `in_place` task on a
checkout another number's `in_place` task uses is `in_use`.
`bad_request`: `repo` not absolute, `kind` not `issue`/`pr`/`name`, an
Issue or PR with a `number` not positive or a `name`, a named task
without `name`, with a `number` or `in_place`, no `session_id`, or a
worktree task's `worktree` not absolute or equal to `repo`.

`task.progress` reports a stage. While `preparing`, the stages `fetch`,
`branch`, `worktree`, `copy`, `setup` set `stage`; `agent` without
`error` makes the task `ready` (no `stage`); any stage with `error` makes
it `failed`, keeping that `stage` and the error (one line, at most 300
bytes). `agent` with `error` and the task's `session_id` fails a `ready`
task the same way (its agent could not start). `stop` or `archive` starts
removing a task that is not `preparing`: `removing` with that `stage`;
`stop` without `error` on a `removing` task is `busy` (another removal is
under way); then `stop`, `archive`, `worktree`, `branch` set `stage`, and
an `error` takes the task back to the state it had (`ready` after a
daemon restart), keeping the error and that `stage`. Any other
combination is `bad_request`; an unknown `id` `not_found`. Each change is
a `task.updated`.

`task.register`, `task.progress` and `task.forget` change nothing and
answer `internal` when `tasks.json` cannot be written.

`task.forget` deletes the record (`task.removed {id, kept_branch?}`,
passing on `kept_branch`), the last step of `task.remove`; an unknown
`id` is `not_found`.

#### Schedules

A schedule (定期実行; `Schedule` below, since 0.17.0) is a command or a
coding agent the daemon runs at the times it names. The daemon serves
`schedule.*` on Linux and macOS only (capability `schedules`); elsewhere
it answers them `unsupported`. It keeps the schedules in
`state/schedules.json` (`{schedules: [Schedule]}`, mode 0600, written
like `tasks.json`) and each one's runs in
`state/schedule-runs/<schedule id>.jsonl`; only the daemon writes them.
Members a later stagent adds to a `Schedule` (its `when` included) or to
a run are kept as they are when this one writes them back, and
`schedule.create` stores the members of its `schedule` it does not know
the same way.

**`Schedule`**:

| member | |
|---|---|
| `id` | 16 lowercase hex characters (the daemon's) |
| `name` | not empty, at most 200 characters, no line break |
| `kind` | `command` or `agent` (see "Agent schedules") |
| `command` | a `command` schedule's shell string (`$SHELL -c`); not empty, no NUL. Not stored for `agent` |
| `harness`, `prompt`, `permission` | an `agent` schedule's (see "Agent schedules"); not stored for `command` |
| `cwd` | the directory it runs in (absolute; a leading `~` given to the methods is the home directory) |
| `when` | when it runs (below) |
| `tz` | the IANA time zone of the times (`Local` is not one); the app's default is the phone's |
| `state` | `active` or `paused` (the daemon's) |
| `created_at` | unix ms (the daemon's) |
| `timeout_min?` | a run's time limit in minutes, `0` none (omitted: 60, which `schedule.create` stores; the app offers 15, 30, 60, 120, 360 and none); 0 … 10080 |
| `retries?` | how many times a run that `failed` or `timed_out` runs again, 0 … 3 (omitted: 0) |
| `retry_delay_min?` | minutes after such a run ended its retry starts, 1 … 60 (omitted: 5, which `schedule.create` stores; the app offers 1, 5 and 15) |
| `stop_after_failures?` | pause the schedule when this many of its times in a row failed, 0 never (omitted: 0; the app offers 3, 5 and 10); at most 100 |
| `notify?` | which of its runs are notified: `problems` (omitted or `""`), `always` or `off` (see "Notifications") |
| `failed_in_row?` | how many of its times in a row failed (the daemon's) |
| `auto_stopped?` | `true`: `stop_after_failures` paused it (the daemon's) |
| `problem_sent?` | a problem was notified and no success since (the daemon's) |
| `last_at?` | unix ms, the daemon's; see "Missed times" |

The members marked as the daemon's are ignored by `schedule.create` and
`schedule.update` (`update` keeps them).

`when` is one of

| `when` | runs |
|---|---|
| `{every: "daily", time: "HH:MM"}` | every day |
| `{every: "weekdays", time}` | Monday to Friday |
| `{every: "weekly", time, days: [0…6]}` | on those weekdays (0 Sunday … 6 Saturday; at least one) |
| `{every: "hours", hours: 1…23, minute?: 0…59}` | at `minute` past every hour `h` of the day with `h % hours == 0` (6: 0:00, 6:00, 12:00, 18:00) |
| `{every: "once", date: "YYYY-MM-DD", time}` | once, on that date; then it has no next time (and stays). A date and time already past when it is created or updated never run |
| `{every: "cron", cron: "0 3 1 * *"}` | at the times the cron expression names (below) |

The times are on the wall clock of the schedule's `tz`, never the
host's: a 9:00 `Asia/Tokyo` schedule runs at 0:00 on a host whose clock
is UTC. A wall-clock time a daylight saving change skips runs at the time
it turns into (America/New_York on its spring day: 2:30 → 3:30 EDT); one
it repeats runs once, the first time (on its autumn day 1:30 EDT, not
1:30 EST). This holds for every `when`, `cron` included: each wall-clock
time of the day runs at most once, so `*/30 * * * *` runs at 1:00 and
1:30 EST, then 3:00 and 3:30 EDT on the spring day, and at 1:00 and 1:30
EDT, then 2:00 EST on the autumn day (the repeated 1:00 and 1:30 EST do
not run).

**Cron expressions** have five fields separated by spaces — minute
(0–59), hour (0–23), day of the month (1–31), month (1–12 or `jan` …
`dec`), weekday (0–7, 0 and 7 Sunday, or `sun` … `sat`; names in any
case) — each a comma-separated list of `*`, `n` or `n-m`, each
optionally followed by `/step` (`n/step` is `n` to the field's last
value). When neither the day of the month nor the weekday starts with
`*`, a day runs when either matches (`0 6 1 * 5`: the 1st and every
Friday); otherwise when both do. Nicknames (`@daily`) and seconds are not
supported. `bad_request` says what is wrong (`when.cron "61 * * * *":
minute 61 is not 0 … 59`); an expression whose day never comes (`0 0 31
2 *`) is one.

**UTC offsets.** The daemon gives every time with the UTC offset of the
schedule's `tz` at it (`tz_offset`, `next_offset`, `next_offsets`:
seconds, east positive); the app shows `unix ms + offset` as that wall
clock and does no time zone arithmetic of its own (today and tomorrow
are told by `ScheduleEntry.tz_offset`).

**`ScheduleEntry`** (a schedule as `schedule.list` answers it):
`{schedule, next_at?, next_offset?, tz_offset, last_run?, retry_at?,
host_clock?}` — `next_at` (unix ms) omitted when it never runs again or
is `paused`, `next_offset` the offset at `next_at` (omitted with it),
`tz_offset` the offset now, `last_run` its newest run (omitted before
the first; `result: running` while it runs, even when times skipped
meanwhile are recorded after it), `retry_at` (unix ms) when its newest
run waits to be retried.

**`HostClock`** `{tz, at, offset, tz_offset}`, in `ScheduleEntry` and
`schedule.preview` as `host_clock`, when the host's own clock is not the
schedule's `tz`'s at its next time (`at`, unix ms; for a `paused`
schedule the next time it would have): `tz` names the host's zone (its
IANA name — `$TZ`, else what `/etc/localtime` links to — else its
abbreviation, `UTC`), `offset` and `tz_offset` are the host's and the
`tz`'s UTC offsets then, in seconds. Omitted when the offsets are the
same (a host on `Asia/Tokyo` and a `Japan` schedule). The app says it
once ("this host is on UTC: 9:00 is its 0:00"), not per time.

**`ScheduleRun`** is one time of a schedule — a time that came, or one
`schedule.run_now` — and one line of its `.jsonl`; its retries go on in
the same line. Its members: `run_id` (16 hex; the same for all its
tries), `scheduled_at` (unix ms, the time it was due; `run_now`'s is
when it was asked), `trigger`, `harness?` (an agent run's), `cwd` (the
schedule's `cwd` when the time started), `retries?` (the schedule's
`retries` when the time started: the m of "retry n/m"; omitted when 0,
and on a skipped line), `tz_offset` (the offset at `scheduled_at`; its
`started_at` and `ended_at` are shown with it too), `auto_stopped?`
(when this time failing paused the schedule, `failed_in_row` then), then
the members of its newest try, and `tries?` (the tries before it, oldest
first).

**`ScheduleTry`** (a try: the first run of the time, or a retry):
`retry?` (which retry, 1 …; omitted for the first try), `started_at`,
`ended_at?`, `result`, `exit_code?`, `session_id?` (its session), `error?`
(one line, at most 300 bytes), `summary?`, `conversation_id?`,
`denials?`, `unjudged?`, `outside_gui?`, `auth_failed?` (see below).

`trigger` is one of

| `trigger` | |
|---|---|
| `scheduled` | its time came |
| `late` | its time came over a minute before it started (see "Missed times") |
| `now` | `schedule.run_now` |

`result` is one of

| `result` | |
|---|---|
| `running` | its try runs |
| `succeeded` | a command's exit code 0; an agent run that ended well |
| `failed` | a non-zero exit code (`exit_code`), or none: it could not start or its session was lost (`error` says) |
| `timed_out` | stopped at its time limit (see "Time limit, retries and stopping") |
| `denied` | an agent run went to its end, but operations were refused (`denials`) |
| `undetermined` | its end is not known: it was running when the daemon started again and its session did not come back |
| `skipped_overlap` | not run: its time came while the previous run still ran |
| `skipped_paused` | not run: a once schedule's time passed while it was paused |

An app shows a `trigger` or `result` it does not know as such, not as a
success.

**Methods.**

`schedule.list` answers every schedule as a `ScheduleEntry`, in the
order they were created. `schedule.runs {id}` answers its runs newest
first. An unknown `id` is `not_found` for every method that takes one.

`schedule.create {schedule}` takes the members above but the daemon's
and answers the new `ScheduleEntry`. `bad_request` says which member is
wrong: `name`, `kind`, `command`, `harness`, `prompt`, `permission`,
`cwd` (not absolute, or not a directory that exists), `tz`, `when`,
`timeout_min`, `retries`, `retry_delay_min`, `stop_after_failures` or
`notify` outside what the tables allow. `internal` when
`schedules.json` cannot be written (nothing changes).

`schedule.preview {schedule}` takes the same `schedule` and answers
`SchedulePreview` `{argv, cwd, next, next_offsets, tz_offset, warnings,
cwd_kind?, task_id?, host_clock?}`: what a run would execute (`argv`: a
command schedule's `$SHELL` of the daemon, `-c` and the command; an agent
schedule's command line, see "Agent schedules"), in which directory
(`cwd`, `~` expanded), the next three times (`next`, unix ms) with their
offsets (`next_offsets`, as long as `next`), the offset now and the
warnings that apply (`[{code, message}]`, below). It saves nothing; the
app shows it before saving and builds neither the command nor the times
itself. It checks `kind`, `command`, `harness`, `prompt`, `permission`,
`tz`, `when` and that `cwd` is absolute (`bad_request`), not `name`, the
limits, `notify` nor that `cwd` exists. When `cwd` is a directory,
`cwd_kind` says what it is: `task` (in the worktree of a task that is not
`in_place`, the innermost one as for `Session.task_id`, symlinks
resolved; `task_id` names it), `checkout` (in another Git work tree: a
checkout the user works in) or `directory` (outside Git). The warning
codes:

| `code` | |
|---|---|
| `cwd_missing` | `cwd` is not a directory (`schedule.create` refuses it) |
| `codex_outside_git` | a codex schedule outside Git: its runs pass `--skip-git-repo-check` |
| `writes_checkout` | an agent schedule with `write` or `all` in a `checkout` |
| `claude_gui_login` | a claude schedule on a macOS host without `${CLAUDE_CONFIG_DIR:-~/.claude}/.credentials.json` (see "Without a GUI login") |

`schedule.update {id, schedule}` replaces what `schedule.create` takes,
checked the same way (`bad_request`); `id`, `state`, `created_at`, the
daemon's members and the members this stagent does not know stay, unless
`schedule` carries the unknown ones. A time that came and was not
compared yet is dealt with as the schedule was before (run, recorded as
skipped, or ending a waiting retry); then `last_at` is now and the next
time is the new schedule's. A running run goes on as it started.

`schedule.pause {id}` makes it `paused`: its times pass without a run
and are not made up for (a once schedule's records `skipped_paused`),
and a retry it waits for is dropped (see "Retries"). `schedule.resume
{id}` makes it `active` from its next time on: `last_at` is now,
`failed_in_row` 0, `auto_stopped` dropped. Both answer the
`ScheduleEntry`.

`schedule.run_now {id}` starts a run now (`trigger: now`), paused or
not, and answers the `ScheduleRun`; `busy` while the schedule's previous
run is still running. A retry the schedule waits for is dropped first.

`schedule.remove {id, stop_run?}` removes the schedule and its record.
With `stop_run` a running run's session gets `session.signal hangup`,
`kill` 5 s later and is waited for 3 s more; the answer comes after
(`busy`, nothing removed, when it did not end: the run goes on as
before). A run stopped so is not notified, counted or retried. Without
`stop_run` the run goes on and its end is recorded nowhere. `internal`
when `schedules.json` cannot be written (nothing changes).

Two apps changing a schedule: the later change wins. A coding agent's
process tree may list, read runs, preview and read output
(`schedule.list`, `schedule.runs`, `schedule.preview`,
`schedule.output`); `schedule.create`, `schedule.update`,
`schedule.remove`, `schedule.pause`, `schedule.resume` and
`schedule.run_now` are refused to it with `agent_refused`.

**`schedule.changed {id, entry?}`** goes to the watchers whenever a
schedule or its newest run changes — created, updated, paused, resumed,
a try started, skipped or ended — with its `ScheduleEntry` as
`schedule.list` would answer it now (`entry`), or without `entry` when
it was removed.

**When it runs.** Every 30 s the daemon compares the wall clock with each
active schedule's next time and starts the ones that came. The clock is
compared again and again rather than waited for with one long timer (a
timer may not advance while the machine sleeps). A time that comes while
the previous run is still running does not run: it is recorded with
`result: skipped_overlap`.

**Missed times.** The daemon keeps each schedule's last time in
`schedules.json` (`last_at`, unix ms): the newest of its times it dealt
with — ran, skipped — or when the times before stopped counting (it was
created, updated or resumed); its next time is counted from it. Times
after it that came while the host slept, was off or the daemon was not
running are missed: when the daemon starts again (or at its next
comparison after a sleep), however many there were, they run once, as
the newest of them (`scheduled_at`), with `trigger: late` when the run
starts over a minute after that time. There is no limit past which a
missed time is dropped. A daemon started again never runs a time at or
before `last_at` again. Times that pass while a schedule is `paused` are
not missed: they are not made up for. A once schedule whose time passes
while it is paused does not run and is recorded with `result:
skipped_paused`; resumed, it has no next time.

A skipped line (`skipped_overlap`, `skipped_paused`) has `trigger:
scheduled`, `ended_at` at once and no `session_id`.

**A run** is a detached session. Right before it the daemon takes the
login shell's environment (`$SHELL -l -i`, then `$SHELL -l`, as the
bridge does for `session.spawn`; 5 s in all for both, the daemon's own
environment when that fails) and starts `stagent run --detached --id
<session id> --cols 120 --rows 40 --cwd <cwd> --schedule <id> --run
<run_id> -- <command…>` the way the bridge starts a holder (on macOS in
the GUI login session when there is one), `<command…>` being `$SHELL -c
'<command>'` (`$SHELL` of that environment, `/bin/sh` when unset) or an
agent schedule's wrapper. Its `Session` has `schedule_id` and `run_id`;
it belongs to no task, and the app lists it under its schedule only, not
among the kept shells or the host's agents. Attaching from the app
resizes it as usual.

The try ends with its session's exit (`holder.ended`) — a command run
with exit code 0 `succeeded`, any other `failed` with `exit_code` — or
with its session lost without one (`failed`, `error: "session lost"`).
A holder that exits before registering, or does not register within
30 s, fails the try with `error` (the end of its log). A try still
`running` when the daemon starts again ends `undetermined` unless its
session comes back within 30 s. Only the newest try's session ends the
time. The newest 200 lines are kept per schedule (the 201st drops the
oldest).

**Agent schedules** (`kind: "agent"`) run a coding agent instead of a
command: `harness` (`claude` | `codex` | `omp`), `prompt` (not empty, no
NUL) and `permission` (`read` | `write` | `all`; omitted or `""` is
`read`, which `schedule.create` stores). Nobody is there while it runs,
so the agent never waits on an approval: what its permission does not
allow is refused and the run goes on to its end. Each permission is the
harness's own flags:

| `permission` | claude | codex | omp |
|---|---|---|---|
| `read` (読むだけ) | `--permission-mode manual` | `--sandbox read-only` | `--approval-mode=always-ask` |
| `write` (ディレクトリの中を書き換える) | `--permission-mode acceptEdits` | `--sandbox workspace-write` | `--approval-mode=write` |
| `all` (何でも・確認なし) | `--permission-mode bypassPermissions` | `--dangerously-bypass-approvals-and-sandbox` | `--approval-mode=yolo` |

The same permission lets the three do different things. Measured on a
Linux host (claude 2.1.296, omp 18.8.9; codex from its sandbox rules,
its login having expired there):

- `read`: claude reads files and runs read-only commands (`ls`); writes
  (Write, Edit) and other commands are refused. codex runs commands in a
  read-only sandbox; writes and the network fail. omp has its reading
  tools only; `write`, `edit` and `bash` are refused.
- `write`: claude changes files in its directory and runs file commands
  there (`touch`, `rm`); other programs (`python3 …`) and writes outside
  the directory are refused. codex runs any command in its sandbox and
  writes in its directory (and the temporary directory); writes outside
  it and the network fail. omp's `--approval-mode=write` allows its
  `write` and `edit` tools — **outside the directory too** (a file in
  another directory was written) — and refuses `bash`, even `ls`.
- `all`: anything, anywhere, network included, without asking.

A run's command is the agent's non-interactive mode with JSON output,
the prompt last after `--` (so one starting with `-` stays the prompt):
`claude -p --output-format json --permission-prompts none
--permission-mode <mode> -- <prompt>`, `codex exec --json <sandbox>
[--skip-git-repo-check] -- <prompt>` (the flag when `cwd` is in no Git
work tree: no `.git` in it or above), `omp -p --mode json
--approval-mode=<mode> [--allow-home] -- <prompt>`. omp started in the
home directory moves to a temporary directory (`~/tmp`, `/tmp`,
`/var/tmp`), so when `cwd` is the home directory `--allow-home` keeps it
in `cwd`. Nothing asks the agent not to keep its conversation. The
daemon builds the command for every try (and for `schedule.preview`'s
`argv`) and the holder runs it under `stagent schedule agent --result
<file> -- <command…>` (`<file>` in the log directory): that wrapper
starts the agent with an empty standard input, shows its JSON on the
terminal as text (replies, the commands and tools it runs, `[stagent]
refused: …` for each refused operation, a last line with the result;
`session.scrollback` reads it as it reads any session), leaves the
outcome in `<file>` and exits with the agent's exit code (128+n for
signal n; signals it gets are passed on). For the session's harness the
wrapper is looked through, as `stagent task run` is (`harness` is the
agent's; codex gets `--no-daemon`).

The result: `failed` when the exit code is not 0 or the output says so
(claude's `is_error` or a `subtype` other than `success`; codex's
`turn.failed`, or an `error` event with no `turn.completed`; omp's
assistant message ending with `stopReason` `error` or `aborted`), with
`error` saying why; else `denied` when operations were refused (claude's
`permission_denials`; codex's items with `status: declined` and file
changes that `failed`; omp's tool results that require an approval no
one can give), counted in `denials`; else `succeeded`. Output that is
none of that JSON is judged by the exit code alone (0 `succeeded`, else
`failed`) with `unjudged: true`, as is a try whose wrapper left no
outcome (it was killed), but for one stopped at its time limit
(`timed_out`, never `unjudged`). An agent try also records
`conversation_id` (claude's `session_id`, codex's `thread.started`
`thread_id`, omp's `session` `id`: `claude --resume <id>`, `codex resume
<id>`, `omp -r <id>` continue it in the run's `cwd`) and `summary` (its
last reply, at most 8 KiB).

`transcript.get {harness, conversation_id}` reads an agent run's
conversation from the agent's own transcript (claude's `-p` and codex's
`exec` ones too, though `conversations.list` never lists them), so it is
there for as long as the agent keeps it, after the run's output is gone.

A run's session handles no approvals: a hook's `PermissionRequest`
registers none and is answered at once (claude refuses the tool, as
`--permission-prompts none` says), and the session raises no
`needs_approval`, `waiting_input`, `turn_complete` or `exited`
notification (its end is notified as the schedule's; see
"Notifications").

**Time limit, retries and stopping** (`command` and `agent` schedules
alike). A try still running `timeout_min` after it started is sent
`session.signal terminate` (SIGTERM to its process group; an agent
run's wrapper passes it on and still leaves its outcome), and `kill` 10
s later when it has not ended. It is recorded with `result: timed_out`
(whatever its exit code, which is kept) and `error: "timed out after
<limit>"` (Go's duration, `15m0s`); an agent try keeps its
`conversation_id`, `summary` and `denials`. Like the times, the limit is
compared with the wall clock every 30 s.

A try that ends `failed` or `timed_out` while the line's `retries` are
not used up and the schedule is `active` is run again
`retry_delay_min` after it ended, in the same line: meanwhile the line
keeps the failed try's `result` and the entry has `retry_at`; when the
retry starts, the try before goes to the end of `tries` and the line's
head is the new try (`result: running`, `retry: n`, a new `session_id`;
the holder's `--run` stays the `run_id`). The app shows "retry 1/2".
A retry runs in the line's `cwd` with the schedule's command, prompt and
permission as they are then. Runs of `trigger: now` are retried the same
way. `denied`, `succeeded`, `undetermined` and skipped lines are not
retried, nor is a try that ends while the schedule is `paused`. A
waiting retry is dropped when the schedule's next time comes,
`schedule.run_now` is called or the schedule is paused or removed; the
failed try is then its time's last result. A daemon that starts again
waits again for the retry of a schedule's newest line when its newest
try failed with retries left, the schedule is `active`, no run of it is
running and nothing has been dealt with since (`scheduled_at` not before
`last_at`): `ended_at` + `retry_delay_min`.

A time counts once, its retries included: when its last try ends
`failed` or `timed_out` (or its retry is dropped) the schedule's
`failed_in_row` goes up by one; `succeeded` or `denied` makes it 0;
`undetermined` and skipped lines leave it. When it reaches
`stop_after_failures` while the schedule is `active`, the schedule
becomes `paused` with `auto_stopped: true` (and `schedule.changed`), and
that time's line gets `auto_stopped: <failed_in_row>`; the app shows
"stopped after 3 failures in a row" with Resume. `schedule.resume`
makes `failed_in_row` 0 and drops `auto_stopped`, as for any paused
schedule.

**A run's output** is its sessions' scrollback, which stays on the disk
after the holder exits (`session.scrollback` is the holder's and ends
with it). `schedule.output {id, run_id, session_id?, before?,
max_bytes?}` reads a try's there, running or ended — `session_id` names
the try (omitted: the newest; a session of no try of the run is
`not_found`) — and answers a page as `session.scrollback` does (`{data,
start, end, first}`, the same `before` and `max_bytes`), or `gone: true`
with empty `data` when it is no longer kept or the try had no session
(it could not start); an unknown `id` or `run_id` is `not_found`. When a
try ends it gets `summary?`: the end of its output as plain text, kept
for when the output is gone — a command try's last 50 lines (escape
sequences removed, a line redrawn after `\r` as it was left), at most 8
KiB; an agent try's is its agent's last reply, or its output's end when
the reply could not be read. Retention (`scrollback_days`) does not
delete the output of each schedule's newest 5 lines that had a session
(all their tries), so a weekly run's output is there until the next; it
still counts toward `scrollback_total_mib`, over which the oldest output
goes first, these included. Older runs' output goes after
`scrollback_days` like any session's.

**Without a GUI login (macOS).** A run's holder starts in the GUI login
session as a `session.spawn` holder does (see "Starting in the GUI login
session"); while nobody is logged in to the desktop it is started
directly, and the try records `outside_gui: true` (any kind of
schedule). There claude cannot read its login from the keychain and,
without `~/.claude/.credentials.json`, ends asking for `/login`: an agent
try that failed because its agent could not log in (claude's `is_error`
with a result asking for `/login`, or `api_error_status` 401) records
`auth_failed: true`. The `claude_gui_login` warning looks at the
credentials file of the daemon's environment only
(`CLAUDE_CODE_OAUTH_TOKEN` and `ANTHROPIC_API_KEY` are not looked for;
see the README): its runs fail while nobody is logged in to the desktop.

**Notifications.** A schedule's `notify` says which of its runs are
notified, with reason `schedule`. Only the last result of a time is,
once its retries are done: a failure that is run again is not. A problem
is `failed`, `timed_out`, `denied`, `undetermined`, an `unjudged` run,
`skipped_overlap`, and the schedule stopping after failures; `late` and
`skipped_paused` are recorded only, and a run stopped by
`schedule.remove` is not notified. Under `problems`, while problems go
on only the first and the stop are notified, and the first success after
them is notified as fixed (`problem_sent` remembers it); `always`
notifies every result, a success after problems as fixed; `off` none.
`notify.reasons.schedule` `false` pushes no schedule's (the event still
comes; the app filters it as it does the other reasons). The
notification (`title` the schedule's name, `body` a fixed phrase in
`notify.lang` — "Failed (exit code 2)", "Stopped at its time limit", "3
actions were refused", "<agent> could not authenticate: nobody is logged
in to the GUI" for a failed try with `outside_gui` and `auth_failed` —
never the agent's reply or the output) has no `session_id` but
`schedule_id` and, for a run's result, `run_id` (none for a stop, a fix
or a skip: those are about the schedule). It is pushed under the usual
conditions (no app in the foreground, nobody present at the run's
session). On ntfy its sequence ID is `schedule_<schedule id>`, and on
Discord and Telegram it replaces the schedule's previous message, so the
newest stands for the schedule; nothing clears it.

#### Click link

A push links to the session it is about (`h=<host_id>`, `s=<session
id>`), or to the host only (`h`) when it is about no session (a digest,
`notify.test`). Which link depends on the channel:

| channel | link | pushes linked |
|---|---|---|
| ntfy (`Click`) | `notify.click_base` with `h` and `s` added to its query, e.g. `sshtermx://open?h=<host_id>&s=<session id>` | every push (`h` only without a session) |
| chat destination (a button, on Slack a link; see "Chat destinations") | `notify.click_page` with `h` and `s` in its fragment, e.g. `https://sshterm.iru-yo.com/open#h=<host_id>&s=<session id>` | a push of one session only: none for a digest, `notify.test` or a push of no session |
| generic webhook (`click_url`) | as the chat destination | every push (`#h=<host_id>` only without a session) |

A schedule's notification links `sc=<schedule id>` and, for a run's
result, `r=<run id>` instead of `s` (`sshtermx://open?h=…&r=…&sc=…`, in
the fragment likewise); a digest folding some schedule's notification
links an empty `sc=` (the host's schedules).

The fragment is `h=<host_id>&s=<session id>` (form-encoded, `h` first);
it never reaches the open page's server, but the message — link included —
stays with the chat service. Nothing else goes into a link. An empty
`click_base` or `click_page` (or a host without `host_id`) means no link
on its channels: no `Click`, no button, no `click_url`. stagent knows no
app scheme and no page: the app sets both.

#### Push notifications

Every notification is a `notification` event for the app. It is also
pushed (ntfy, webhook, chat) only when all of these hold:

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

On the chat destination (see "Chat destinations"), Discord and Telegram
replace and resolve too; the daemon keeps the ids of the messages they
took:

- Replace: the next message of a session is sent first, then the
  session's older message is deleted (Discord `DELETE
  /webhooks/<id>/<token>/messages/<message id>`, Telegram
  `deleteMessage`), so one unresolved message per session stays in the
  channel. A message the service does not delete (Telegram's after 48
  hours, another refusal) is edited to resolved instead (below).
- Resolve: when the session settles (above), its message is edited, not
  deleted: ☑️ in place of the reason's emoji and `（解決済み）` after the
  title in `notify.lang` (en ` (resolved)`, ko ` (해결됨)`, zh
  `（已解决）`), the body unchanged and the same link button. Discord
  `PATCH` of the same path (`?with_components=true` with the button) with
  a grey embed (`0x99AAB5`) and `components` sent again; Telegram
  `editMessageText {chat_id, message_id, text, link_preview_options}`
  with `reply_markup` sent again (without it the button would go). Then
  the daemon forgets the message.
- Digest: a digest's message remembers the sessions in it and is edited
  to resolved once all of them have settled. A later message of one of
  them never deletes the digest; replacing deletes a session's own
  messages only. A digest replaces the older own messages of its
  sessions.
- Slack (Incoming Webhooks name no message) only gets pushes: nothing is
  deleted or edited.
- The daemon keeps the unresolved messages in `state/chat-messages.json`
  (mode 0600): per message its sessions (a digest: those not settled
  yet), the Discord message id or the Telegram chat id and message id,
  the time it was sent, the SHA-256 of the `notify.chat.url` it went
  through — never the URL or its token — and the title, body and link
  the resolved edit writes again. The file is written when a message is
  sent and when one is let go. After a restart, a message of a session
  whose holder still answers is the session's again (resolved when it
  settles); any other is edited to resolved at once.
- When `notify.chat.url` changes or `chat` is removed, the messages sent
  through the old URL are forgotten without an edit: they stay as they
  look.
- A failed delete or edit is not a failed push (`last_error`) and is not
  retried: a 404 (the message, webhook or bot's chat is gone) lets the
  message go; a 429 or a connection error lets it go with one log line;
  a refused delete is followed by the resolved edit, and a refused edit
  lets the message go with one log line.

The generic webhook (`notify.webhook`) gets no clear: a receiver that
turns every POST into a notification would show one for it.

`notify.lang` (`en` default, `ja`, `ko`, `zh`) is the language of the
fixed phrases stagent writes — notification bodies such as `Turn
complete`, digest titles and the `notify.test` body — in events and
pushes alike. `reason` never changes; the app can build its own text from
it.

`notify.last_error` (in the `config.get` / `config.set` result and in
`stagent doctor`) holds, per channel (`ntfy`, `webhook`, `chat`), the last
failed push: `{at (unix ms), status? (HTTP status; absent for a
connection error), error, kind? (chat only)}`. A successful push to the channel removes its
entry; channels without a failure are absent and `last_error` is `{}` when
none failed. `error` is the HTTP status line (`429 Too Many Requests`) or
the connection error, never the ntfy topic, the webhook URL or the chat
URL; `notify.test`'s `unavailable` message is built the same way (a chat
URL that names no service, in a hand-edited `config.json`, fails with the
list of formats). Queued pushes and
`notify.test` both count. The daemon keeps the entries in
`state/notify-errors.json` (mode 0600), so they survive its restarts,
next to `state/chat-messages.json` (mode 0600, the chat destination's
unresolved messages, see above).

The chat destination's `error` is the status line followed by the
service's short reason, `<status line>: <reason>` — Discord's `message`
and `(code)`, Telegram's `description`, Slack's body when it is only
lowercase letters and `_` — e.g. `404 Not Found: Unknown Webhook (10015)`,
`403 Forbidden: bot was blocked by the user`, `404 Not Found:
channel_not_found`. The reason is 200 characters at most, without control
characters (and without the status line's own text repeated); a reason
holding the URL, a token (the webhook token, the bot token) or the URL's
path is dropped whole, leaving the status line: the URL and its tokens
never show. Its `kind` says what failed (a client that does not know the
value shows `error`):

| `kind` | failure |
|---|---|
| `revoked` | the service disabled the destination: Discord codes 10015 (Unknown Webhook) and 50027 (Invalid Webhook Token), Telegram 401, Slack `no_service`, `no_active_hooks`, `invalid_token`, `team_disabled`, `no_team` |
| `unreachable` | the destination cannot be reached: Telegram 403 (bot blocked, removed from the group, never messaged) and 400 `chat not found` or a group upgraded to a supergroup (stagent does not follow `migrate_to_chat_id`: get the new chat id), Slack `channel_not_found`, `channel_is_archived`, `action_prohibited` |
| `rate_limited` | 429 |
| `rejected` | any other 4xx (the message stagent built was refused) |
| `server` | 5xx |
| `network` | no status line (connection error, timeout) |

Telegram is classified by its `error_code`, not the `description` text
(except for the two 400s above). A queued push answered 429 whose wait —
Discord's JSON `retry_after` (seconds, fractional) or `Retry-After`,
Telegram's `parameters.retry_after`, Slack's `Retry-After` — is 5 s
(`notify.Timeout`) or less is sent once more after that wait (the worker
waits: other channels' pushes wait with it); a longer or unnamed wait
drops the push. Only a failed resend is recorded. `notify.test` is never
resent (it fails right away, kind `rate_limited`), and neither is a 5xx
or a network failure. Pushes keep going after any failure, `revoked` and
`unreachable` included (fixing it on the service side is enough); while
the same kind repeats, the daemon logs only the first. `config.set`
changing `notify.chat.url` (to `null` included) removes `last_error.chat`:
a failure of the old URL says nothing about the new one, so a push still
queued for the old URL does not bring it back; `enabled: false` alone
keeps it.

If the daemon restarts while the bridge is connected, daemon requests in
flight fail with `unavailable` and the bridge restores the watch itself:
events the app missed arrive as `event`, every current session as
`session.updated`, sessions that disappeared as `session.removed`, every
current task as `task.updated`, tasks that disappeared as `task.removed`,
and the current `unwrapped` list (empty after a restart) as `unwrapped.updated`. The
app needs no daemon-restart handling. Session requests go to the holders
directly and are unaffected.

#### Chat destinations (`notify.chat`)

`notify.chat {enabled, url}` sends every push to a chat service in the
service's own message format; stagent builds it (no template). It is a
channel of its own (`chat` in `last_error` and `notify.test`), next to
the generic webhook (`notify.webhook`, which gets the `notification`
object with `click_url?` added): both can be enabled and both get every push. `url` is
one line and names the service:

| service | `url` |
|---|---|
| Discord | `https://discord.com/api/webhooks/<id>/<token>` (also `discordapp.com`) or `discord://<id>/<token>` |
| Slack (Incoming Webhook) | `https://hooks.slack.com/services/<a>/<b>/<c>` or `slack://<a>/<b>/<c>` |
| Telegram | `tgram://<bot token>/<chat id>` |

`config.set` refuses (`bad_request`, nothing written) any other `url`: an
https URL on another host or path, with a port, user, query or fragment,
a Discord id that is not a number, a Telegram bot token not of the form
`<number>:<letters, digits, _ and ->` or a chat id that is neither a
number (negative for groups) nor `@<channel name>`. Slack Workflow
Builder webhooks (`https://hooks.slack.com/triggers/…`, `/workflows/…`)
are refused too: they take the generic webhook. Whether the destination
answers is `notify.test`'s job (`{channels: ["chat"]}`).

A message starts with the emoji of its `reason` (🔒 `needs_approval`, ⏳
`waiting_input`, ✅ `turn_complete`, ⚠️ abnormal / 🏁 normal `exited`, 🔔
`digest`; none for `terminal` and `test`), then the host-labelled title,
then on the next line the body (the fixed phrase in `notify.lang`):

| service | request (JSON, `Content-Type: application/json`) | the message's ids |
|---|---|---|
| Discord | `POST https://discord.com/api/webhooks/<id>/<token>?wait=true` `{embeds: [{title, description: body, color?}], allowed_mentions: {parse: []}}`; `color` is yellow (`0xFEE75C`) for level `warn`; no mention in the text pings. With a link: `&with_components=true` and `components: [{type: 1, components: [{type: 2, style: 5, label, url}]}]` (a link button) | the reply's `id` |
| Slack | `POST https://hooks.slack.com/services/<a>/<b>/<c>` `{text: "*<title>*\n<body>", unfurl_links: false}`; `&`, `<` and `>` escaped (`&amp;` `&lt;` `&gt;`), so no link or mention of the text's own. With a link: `\n<url\|label>` at the end of `text` (the url's `&` as `&amp;`) | none (Incoming Webhooks name no message) |
| Telegram | `POST https://api.telegram.org/bot<bot token>/sendMessage` `{chat_id, text: "<title>\n<body>", link_preview_options: {is_disabled: true}}`; no `parse_mode`: plain text. With a link: `reply_markup: {inline_keyboard: [[{text: label, url}]]}` (a url button) | the reply's `result.chat.id` and `result.message_id` |

The link (see "Click link") is the open page `notify.click_page` with
`#h=<host_id>&s=<session id>`, only on a push of one session; `label` is
`Open in SSH Term` in `notify.lang` (ja `SSH Term で開く`, ko `SSH
Term에서 열기`, zh `在 SSH Term 中打开`).

Discord and Telegram messages are replaced and resolved (see "Push
notifications"); Slack messages are only sent.

`url` is a secret: anyone who knows it can post to the channel, and a
Telegram one holds the bot token, which controls the bot. The host keeps
it in plain text in `config.json` (mode 0600; Windows has no permission
bits), and `config.get` returns it. It never goes into `last_error`,
`notify.test`'s errors (`config.set`'s `bad_request` names the formats,
not the URL), `stagent doctor`, the logs or `state/chat-messages.json`.

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
| `task.updated` | `Task` (full object, added or changed; replaces the previous one) |
| `task.removed` | `{id, kept_branch?}` — `kept_branch`: the branch `task.remove` kept (see "Removing a task") |
| `schedule.changed` | `{id, entry?}` — `entry`: the `ScheduleEntry` now, omitted when the schedule was removed (see "Schedules") |
| `host_screen.frame` | `{seq, data, width, height}` (see "Host screen") |
| `host_screen.state` | `{state: "live"\|"gone", title?, url?}` (see "Host screen") |

## Objects

`Session`: `id, harness (claude|codex|omp|pi|copilot|other), command_harness
(claude|codex|omp|pi|copilot|other), command[], cwd, pid,
holder_pid, mode (passthrough|detached), attached, state (working|idle|
waiting_input|needs_approval|exited), state_source (hook|terminal|activity|
process), title?, conversation_id?, transcript_path?, last_message?, cols,
rows, started_at, last_activity_at, last_local_input_at?, focused?,
presence_file?, exit_code?, task_id?, schedule_id?, run_id?`.
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

`task_id` is the task the session belongs to, decided by the daemon (see
"Tasks"); omitted when none.

`schedule_id` and `run_id` (since 0.17.0) name the schedule run the
session is (see "Schedules"); omitted for any other session. The app
leaves these sessions out of the kept shells and the host's agents.

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

`command_harness` (always present; 0.15.0) is the harness the session's
command runs by itself, judged from `command` alone: its program's name
(for a task's prepared session, `stagent task run … -- <command>`, the
command after `--`), `other` for any other program. Hooks never change it.
It differs from `harness` while an agent runs inside a session that
started as another program (a kept shell): `harness` is that agent,
`command_harness` is `other`.

`Event`: `seq, time, session_id?, kind, data`. Kinds and `data`:

| kind | data |
|---|---|
| `session_started` | `{harness, command[], cwd, mode}` |
| `session_ended` | `{exit_code, hung_up?}` — `hung_up`: a client ended it with `session.signal hangup`, or (Windows) the console of a passthrough session closed |
| `state_changed` | `{from, to, source}` |
| `notification` | `{title, body, level (info\|warn), reason, count?, session_id?}` — reason `waiting_input\|needs_approval\|turn_complete\|exited\|terminal\|test\|digest`; `session_id` names the session it is about (absent for a digest, `notify.test` and approvals of no session); the generic webhook posts this object with `click_url?` added (see "Click link"; absent without `notify.click_page`) |
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
therefore watches its screen (local IPC only): while a claude, Codex or
Copilot CLI approval of the session is pending, the daemon sends the holder
`holder.prompt_watch {id, gen, on: true, harness}` (a new `gen` with every
approval; `harness` (since 0.16.0) names the harness whose menu to read:
Copilot CLI draws its menu in a box `╭─╮ │ │ ╰─╯` with claude's `❯` cursor,
its dialog starting at the box's top border; `on: false` once none is
pending, again to a holder that re-registers). Copilot CLI runs its
PermissionRequest hook before its own permission checks (rules, approvals
for the session), so that hook returns at once and registers nothing; the
`Notification` with `notification_type` `permission_prompt` that Copilot CLI
sends with its menu registers the approval, `tool_name` and `summary` from
the latest PermissionRequest of the session (else the notification's
`message`). It closes like a Codex approval (prompt watch, output activity,
Stop).
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

`Task`: `id, repo, kind (issue|pr|name), number, name?, title, url,
branch, base, worktree, in_place, created_at, state (preparing|ready|
failed|missing|removing), stage?, error?, session_id?`. A named task
(`kind: name`) has `name`, `number` 0, empty `title` and `url`, and is
never `in_place`; an Issue or PR has no `name`. `repo` is the source
checkout's canonical path; `worktree` is where the task is worked on
(`repo` itself when `in_place`). `created_at` is RFC 3339 (UTC, whole
seconds). `stage`
is, while preparing, `fetch|branch|worktree|copy|setup|agent` (the stage
it failed at, once `failed`) and, while removing,
`stop|archive|worktree|branch` (the stage that failed, once a removal
failed and the task is back to `ready` or `failed`). `error` is one line; details stay in the
terminal's output. `session_id` is the session that prepares the task;
sessions are tied to tasks by `Session.task_id`, not by it.

`UnwrappedLaunch`: `conversation_id, harness (claude|codex|omp|pi|copilot), cwd, reason
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
token?}, webhook {enabled, url, headers?}, chat {enabled, url},
debounce_ms, digest_window_ms,
host_label, click_base, click_page, reasons {needs_approval, waiting_input,
turn_complete, exited, terminal, schedule}, lang, skip_when_claude_app_notifies},
retention {events_days, events_max, scrollback_days,
scrollback_total_mib, scrollback_session_mib}, idle_after_ms,
disable_handoff, repos? {<source checkout>: {worktree_include?[]}},
worktree_folder?, worktree_watch?[], worktree_repos?[]`. `repos` (absent
when empty) holds the app's patterns of files to copy per source checkout
(see `config.set` and "Files to copy"). `worktree_folder` (absent when
unset) is the worktree folder, an absolute path (see `config.set`).
`worktree_watch` and `worktree_repos` (absent when empty) are the watch
folders and the source checkouts found (see "Worktrees").
`disable_handoff` (default false) makes
`stagent run --handoff=auto` — what the shell wrappers run — end the
program with its terminal unless `STAGENT_HANDOFF` decides otherwise;
holders read it at every start, so it applies from the next start on, also
in shells that are already open. `notify.host_label` (no default: empty
means the host name) names this host in the title of every push (see
`notify.test`); the app sets it to the connection's name. `notify.click_base`
(no default: empty means no link) is the link an ntfy push opens when
tapped (see "Click link"); it must be an absolute URL.
`notify.click_page` (no default: empty means no link) is the open page
that chat destinations and the generic webhook link to (see "Click
link"); it must be an absolute https URL with no query and no fragment.
`notify.reasons` selects
what is pushed (see "Push notifications"): booleans, defaults `true`,
`true`, `true` and `terminal` `false`, `schedule` `true` (see "Schedules";
an older stagent ignores the key); `exited` is `off`, `error`
(default: abnormal exits only) or `all`. `notify.lang` is `en` (default),
`ja`, `ko` or `zh`; the app sets it to its own language.
`notify.skip_when_claude_app_notifies` defaults to false. `notify.chat` is
the chat destination (see "Chat destinations"). Missing keys take their
defaults; another `exited` or `lang`, or a chat `url` of no known format,
is `bad_request`.

## `stagent follow` (terminal chat view, follow protocol 1)

`stagent follow` streams the transcript of the coding agent (Claude Code,
Codex, omp, Pi, Copilot CLI) running in a terminal tab. The app runs it over
an exec channel **on the same SSH connection as the tab's shell**, with the
command forms of
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
(`claude`, `codex`, `omp`, `pi`, `copilot`, also `node|bun … <script>` of
their npm packages; Pi sets its process title to `pi`, which Linux shows as
its name and macOS as its argv, `["pi", "", …]`, its arguments
overwritten); a process of the same harness below an agent belongs to it
(Codex's and Copilot CLI's native binaries under their node launchers, a
pi Pi's bash tool started). A multiplexer client there
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
(`sessionId`) → `projects/*/<sessionId>.jsonl`. Codex, omp and Pi, first
match wins:

1. the rollout / session file the agent has open (Linux; both keep it open
   once they have written to it);
2. omp: the session `~/.omp/agent/terminal-sessions/<terminal>` names for
   the agent's terminal (`pts-3` for `/dev/pts/3`), which omp rewrites
   whenever it starts, continues, resumes or switches sessions and which
   `omp -c` reads. It counts when written since the agent started, or for
   `-c/--continue` when its cwd is the agent's; one marked `fresh` whose
   file does not exist yet means no transcript, not a guess below;
3. the session its command line names: `codex [exec] resume <uuid>` →
   `sessions/**/rollout-*-<uuid>.jsonl`; `omp -r/--resume <id prefix|path>`
   → the newest `<ts>_<id>.jsonl` whose id starts with the prefix, or the
   file; `omp -c/--continue` → the newest session of the agent's directory
   created before the agent started (`--session-dir` honoured); `pi
   --session <path|id prefix>` → the file, or the newest
   `<ts>_<id>.jsonl` whose id starts with the prefix; `pi --session-id
   <id>` → that session, a new one when there is none; `pi
   -c/--continue` → the session of the agent's directory
   (`--<cwd, / \ : as ->--`) created before the agent started and written
   last (`--session-dir` honoured); `pi --fork` starts a new one. Pi's
   command line is readable only where its process title does not
   overwrite it (Windows);
4. the harness's newest conversation in the agent's working directory
   written since the agent started that no other running agent process
   (Codex, omp, Pi, Copilot CLI) has open or names on its command line,
   and no other listed agent already follows. An agent that starts a new
   conversation (no resume / continue) prefers one created since it
   started.

Pi opens its session file only to append a record (Linux rarely sees it
open) and keeps no record per terminal, so a Pi agent's session is mostly
the one of 4.

Copilot CLI (`copilot`, its native binary, also under its npm launcher
`node …/@github/copilot/npm-loader.js`): `session-state/<id>/events.jsonl`
of the session directory one of its processes has a file of open, else
the session `--resume[=]<id>`, `-r <id>` or `--session-id[=]<id>` names,
else 4. Its records are read tolerantly: types not read and lines that do
not parse are skipped.

`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, `PI_CODING_AGENT_DIR` (Pi's sessions
are in its `sessions`, default `~/.pi/agent/sessions`) and `COPILOT_HOME`
(default `~/.copilot`) are read from the agent's environment.

### Security

`stagent follow` only uses processes of the user it runs as: the same
effective uid, with the real uid equal to it (a set-user-ID program has its
owner's privileges but the arguments and environment of whoever started it,
so it counts as another user's), or on Windows the same token user SID. On
Linux both stagent's uids and every other process's are read from
`/proc/<pid>/status`, so under proot's fake root (`proot -0`), which fakes
`geteuid` but not `/proc`, every process of the guest has the user's kernel
uid, root's included (proot separates no privileges). A
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
| `stagent integrate --json --plan\|--apply [--harness claude,codex,omp,pi,copilot] [--shell-wrapper] [--service] [--linger] [--terminal] [--execution-policy] [--wsl-keep-running] [--remove claude,codex,omp,pi,copilot,shell-wrapper,service,linger,terminal,wsl-keep-running]` | `{applied, result, login_shell, changes[{id, target, action (create\|modify\|delete\|skip), summary, diff, error?, error_code?}], notes[{code, text, args?}]}` |
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
| `omp_restart` | – | running omp sessions load the extension only after a restart (given when the extension changes and omp is on the host) |
| `pi_restart` | – | running Pi sessions load the extension only after a restart (or `/reload`) (given when the extension changes and pi is on the host) |
| `copilot_restart` | – | running Copilot CLI sessions load the hooks (`$COPILOT_HOME/hooks/ssh-term-stagent.json`, default `~/.copilot/hooks/`) only after a restart (given when the hooks file changes and copilot is on the host) |
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
stays. Pi's extension and Copilot CLI's hooks file are also removed where
the manifest records them, so one written under another
`PI_CODING_AGENT_DIR` / `COPILOT_HOME` than the current one goes too
(`doctor` lists it there until then).

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
(`notify: the last push to ntfy failed at <RFC 3339 time>: <error>`); the
chat destination's names its `kind` (`notify: the last push to chat
failed at <RFC 3339 time> (<kind>): <error>`), and a `revoked` one adds
`; the destination was disabled on the service side; set a new URL from
the app`.

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
Schedule runs start the same way and record whether they did not get
into the GUI login session (`outside_gui`; see "Schedules").

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
PowerShell profile block) define functions `claude`, `codex`, `omp`, `pi`
and `copilot` (every harness stagent knows)
that run the real command as `stagent run --handoff=auto -- <name>
<args…>` when the shell is interactive, stdin and stdout are terminals, the
shell is not already inside a stagent session (`STAGENT_SESSION_ID` unset)
and the stagent binary exists. Non-interactive invocations run the real
command directly: claude with `-p` / `--print` anywhere in its arguments,
codex whose first argument is `exec` / `e`, omp with `-p` / `--print` or a
`--mode` (`--mode X` or `--mode=X`) other than `text`, pi with `-p` /
`--print` / `--no-session`, a `--mode` other than `text`, or a first
argument `install`, `remove`, `uninstall`, `update`, `list`, `config`,
`auth` or `mcp`, copilot with `-p` / `--prompt` / `--prompt=…` / `--acp`
or a first argument that is one of its subcommands (`login`, `help`,
`init`, `config`, `update`, `version`, `workflow`, `sessions`, `memories`,
`plugin`, `mcp`, `skill`, `instruction`, `lsp`, `sandbox`, `completion`,
`app`). The block also exports `STAGENT_SHELL_WRAPPER=1`, marking shells
that have the wrapper.
The wrapper itself does not look at `STAGENT_HANDOFF`; `stagent run
--handoff=auto` does. The PowerShell block also wraps the prompt defined
before it with the working-directory hook described under
`session.spawn` (each prompt sets the process's working directory to the
shell's file system location), so a terminal sshd starts in PowerShell
moves the repository the GitHub screen sees with `Set-Location`; a prompt
a later part of the profile defines replaces it.

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
hook payload on stdin. An event name given as an argument (`stagent hook
<harness> <event>`) names the event, else `hook_event_name` inside the
payload does. Codex's `notify` program is `stagent hook codex notify` and
gets the JSON as its last argument. Copilot CLI's hooks file
(`$COPILOT_HOME/hooks/ssh-term-stagent.json`, default
`~/.copilot/hooks/`) registers the PascalCase events `SessionStart`,
`SessionEnd`, `UserPromptSubmit`, `Stop`, `Notification` and
`PermissionRequest`, whose payloads are Claude Code's snake_case form
(the camelCase fields `sessionId`, `transcriptPath`, `toolName`,
`toolArgs` are read too); each hook passes its event as the argument,
`stagent hook copilot <event>`, from a bash command (`[ -x '<bin>' ] &&
'<bin>' hook copilot <event> || true`) and a powershell one (below). Its
`PermissionRequest` hook answers nothing and returns at once: Copilot CLI
shows its menu only after the hook. The omp extension runs `stagent hook
omp` with `{hook_event_name, session_id, transcript_path, cwd, message?}`
on stdin, and Pi's extension (`$PI_CODING_AGENT_DIR` or `~/.pi/agent`,
`extensions/ssh-term-stagent.ts`) runs `stagent hook pi` with the same and
`argv` (Pi's arguments, `process.argv` after the script), both using
Claude's event names (`SessionStart`, `UserPromptSubmit`, `Stop`,
`SessionEnd`; Pi's: `session_start`, `before_agent_start`,
`agent_settled` — the end of a run no retry, compaction or queued message
follows — and `session_shutdown`). Both stay silent in a run without a
terminal UI (the extension context's `hasUI` false, e.g. `-p`, `--mode
json`); Pi's also when its arguments hold `-p` /
`--print` or `--mode json` / `--mode rpc` (also `--mode=…`), as Pi
reports a UI in rpc mode, and in a pi that Pi's bash tool started
(`PI_SESSION_ID` set, which Pi gives only to those commands; such a pi
has no terminal and is one of the former anyway), so a nested agent is
not counted twice. A hook never fails the harness: when the daemon is
unreachable it exits 0 without output.
A hook run with `CLAUDE_CODE_BRIDGE_SESSION_ID` set (claude connected to
Remote Control) tells the daemon so (`notify.skip_when_claude_app_notifies`).
On Windows the commands run `stagent.exe` by its absolute path. Claude Code
runs them through Git Bash or cmd, so its command quotes the path with
forward slashes (`"C:/…/stagent.exe" hook claude`). Codex runs them with
the session's shell, PowerShell on Windows, where a quoted path followed by
arguments does not parse: its command leaves the forward-slash path bare
(`C:/…/stagent.exe hook codex`, valid in every shell), or uses the call
operator (`& 'C:/…/stagent.exe' hook codex`) when the path holds a space
or another character PowerShell reads specially. Copilot CLI's powershell
command keeps the path's backslashes, single-quoted for PowerShell (`'`
doubled), and skips a missing binary as the bash command does: `if
(Test-Path -LiteralPath 'C:\…\stagent.exe') { & 'C:\…\stagent.exe' hook
copilot <event> }`.

### Agents without a stagent session

Outside a stagent session (`STAGENT_SESSION_ID` unset) `stagent hook` also
tells the daemon how the agent was started (`hook.event` params, local IPC
only):

| param | value |
|---|---|
| `shell_wrapper` | `STAGENT_SHELL_WRAPPER` is set: the shell has the wrapper's rc block |
| `entrypoint` | `CLAUDE_CODE_ENTRYPOINT` (Claude Code: `cli`, `sdk-cli`, `sdk-ts`, `claude-vscode`, …) |
| `parent_tty` | the harness — the hook's nearest ancestor that is not a shell (the installed Codex command keeps `sh -c` in between) — reads a terminal: its stdin (`/proc/<pid>/fd/0`) on Linux, its controlling terminal (as `ps -o tty=`) on macOS; absent when unknown (Windows) |
| `parent_batch` | the harness's command line asks for a non-interactive run by the shell wrapper's rule (`claude -p` / `--print`; `codex exec` / `e` as the first argument; `omp -p` / `--print` or `--mode` other than `text`; the same for pi, plus `--no-session` and its subcommands, whose command line is the payload's `argv`: its process title hides it; `copilot -p` / `--prompt` / `--prompt=…` / `--acp` or one of its subcommands) |
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
| codex / omp / pi / copilot (and Claude Code without `entrypoint`) with `parent_tty` false or `parent_batch` | not recorded (non-interactive) |
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
goes away at the conversation's `SessionEnd` (Claude Code, Copilot CLI;
omp's and Pi's `session_shutdown`), 12 hours after its last hook (Codex
sends no end), when
a stagent session carries the conversation, and with the daemon; nothing is
written to disk. Recording changes nothing else about the hook.
