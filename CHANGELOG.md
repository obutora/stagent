# stagent changelog

Each release's section is its GitHub release notes (`scripts/release.sh`
publishes it with the binaries).

## 0.16.0

### Added

- Pi (`pi`, earendil-works/pi) is a harness (`pi`): `stagent integrate
  --harness pi` writes an extension to Pi's agent directory
  (`$PI_CODING_AGENT_DIR`, `~` expanded, else `~/.pi/agent`) under
  `extensions/ssh-term-stagent.ts` that runs `stagent hook pi` on
  `session_start`, `before_agent_start` (the prompt), `agent_settled` (the
  end of the turn) and `session_shutdown`, and `--remove pi` / `uninstall
  --level unhook` delete it, also where the manifest recorded it under an
  earlier `PI_CODING_AGENT_DIR`; doctor reports Pi; a new note
  `pi_restart` when the extension changes. The extension stays silent
  without a terminal UI, with `-p` / `--print` or `--mode json` / `rpc`
  (also `--mode=…`) among Pi's arguments (Pi reports a UI in rpc mode),
  and in a pi that Pi's bash tool started (`PI_SESSION_ID`), so a nested
  Pi is not counted twice. Pi's process is recognized by its title `pi`
  and by its npm packages (`@earendil-works/pi-coding-agent`,
  `@mariozechner/pi-coding-agent`); its conversations (session format v3
  under `sessions/`) are listed, titled by `session_info` or the first
  prompt, read and followed; the shell wrapper wraps `pi` and leaves
  `-p` / `--print`, `--mode` other than `text`, `--no-session` and its
  package and account commands alone. Its hooks carry Pi's arguments
  (`argv`): Pi's process title hides them from the process table, and
  `parent_batch` is judged from them (#1112).
- GitHub Copilot CLI (`copilot`) is a harness (`copilot`): `stagent
  integrate --harness copilot` writes `$COPILOT_HOME/hooks/ssh-term-stagent.json`
  (default `~/.copilot/hooks/`), whose hooks run `stagent hook copilot
  <event>` from bash and from PowerShell for `SessionStart`, `SessionEnd`,
  `UserPromptSubmit`, `Stop`, `Notification` and `PermissionRequest`
  (PascalCase names: Copilot CLI then sends Claude Code's snake_case
  payload; `stagent hook copilot` also adds `session_id`,
  `transcript_path`, `tool_name`, `tool_input` from camelCase payloads);
  `--remove copilot` / `uninstall --level unhook` delete it, also where
  the manifest recorded it under an earlier `COPILOT_HOME`; doctor
  reports Copilot CLI; a new note `copilot_restart`. Its PermissionRequest
  hook returns at once and registers nothing (Copilot CLI runs it before
  its own permission checks); the `permission_prompt` notification
  registers the approval, named by the tool the latest permission check
  named, and the holder watches Copilot CLI's boxed menu like Codex's.
  Copilot CLI's notices of background work (`shell_completed`,
  `shell_detached_completed`, `agent_completed`, `agent_idle`) set no
  state. Its process is recognized by name (the native binary, also under
  its npm launcher) and npm package (`@github/copilot`); its conversations
  (`session-state/<id>/events.jsonl`) are listed, titled by
  `workspace.yaml`'s `name`, else `summary`, else the first prompt, read
  (records of unknown types and lines that do not parse are skipped) and
  followed (an open file of the session directory, `--resume[=]<id>`,
  `-r`, `--session-id`); the shell wrapper wraps `copilot` and leaves
  `-p` / `--prompt[=…]`, `--acp` and its subcommands alone (#1113).
- `holder.prompt_watch` gains `harness` (with `on`): the harness whose
  menu the approval brings, so Copilot CLI's menu (claude's `❯` cursor, in
  a box) is read as Copilot CLI draws it. Without it every harness's menus
  are looked for (#1113).

### Changed

- A user message whose content is a plain string (Pi) is read in omp's
  transcript format too (#1112).
- `omp_restart` is noted only when `integrate` changes the omp extension,
  not on every `integrate --harness omp`.

## 0.15.0

### Added

- `Session` gains `command_harness` (always present): the harness the
  session's command runs by itself (`claude`, `codex`, `omp` or `other`),
  judged from `command` as `harness` starts out. It stays `other` for a
  kept shell while hooks set `harness` to an agent running inside it, so
  the app tells "an agent inside a shell" without judging the command
  itself (#1109).

### Changed

- The harnesses stagent knows are listed in one place, the harness
  registry, and every list and judgement of them (`stagent integrate
  --harness` / `--remove`, doctor, the shell wrapper's functions and its
  rule for non-interactive runs, transcript roots, process recognition)
  is built from it. One judgement tells a program's harness for a
  session's command and for a process alike, so a session whose command
  is Codex's native binary (`codex-x86_64-…`, `codex-aarch64-…`) is now
  `codex`, as its process already was. The shell wrapper's scripts are
  generated from the registry: their text changes (the batch functions
  get one branch per command, the comments name no harness) but they
  wrap and leave alone the same runs; `stagent integrate
  --shell-wrapper` offers to rewrite an installed block (#1109).

## 0.14.0

### Added

- Tasks: a task can be tied to a name instead of an Issue or PR (`kind:
  name`, `Task.name`): a worktree on a branch of that name (or the app's)
  made from the base without gh, with the same copied files, shared
  directories, setup and 片付け as an issue's. Its key is the checkout
  and the name (an issue's number never matches it); it is never
  `in_place`. `task.create` and `task.register` gain `name`;
  `task.create` refuses an invalid name, a missing `worktree_folder` and
  a place that is a non-empty directory (`bad_request`). `task.preview`
  gains `name?` and `branch?` and answers `name_problem` (`invalid`,
  `task`, `folder`, `branch` or `no_folder`) while the app's name is
  typed. A push about a named task's session puts its name where an
  issue's `#N` goes (#877).
- `config.json` gains `worktree_folder`, the worktree folder (worktree
  の置き場): named tasks go in `<folder>/<checkout name>/<name>`, and
  with it set Issue and PR tasks in `<folder>/<checkout
  name>/<number>-<slug>` instead of next to the checkout. `config.set`
  expands a leading `~` with the host's home and refuses a path that is
  not absolute (#877).

### Fixed

- Host screen, Android: on Linux without `$XDG_RUNTIME_DIR` (an SSH
  session in a container without logind, `su`), the emulator's discovery
  file is also read where the emulator falls back to when `/run/user/<uid>`
  does not exist: `avd/running` in its user directory (`~/.android`, or
  `$ANDROID_EMULATOR_HOME`, `$ANDROID_PREFS_ROOT`, `$ANDROID_SDK_HOME` as
  the emulator takes them). Such an emulator was listed `no_grpc` and
  `host_screen.start` answered `unavailable` (#705).

## 0.13.0

### Added

- Host screen: an iOS simulator takes `text` of any script, Japanese
  included (it took ASCII only and answered other text `unsupported`;
  #649, #665). The text is put on the simulator's clipboard (`xcrun
  simctl pbcopy`) and pasted by what the app in front may do, read
  (never written) from the simulator's TCC.db: an app allowed to paste
  from other apps gets ⌘V, and keys (the Return of `enter`, the next
  text) wait 3 s after it, while iOS drops them under its "pasted from"
  banner; otherwise ASCII is typed after the 英数 key (LANG2), and other
  text is pasted with the edit menu's Paste, opened by tapping the user's
  cursor (`text` gains `x?`, `y?`). An app that never asked first gets a
  ⌘V iOS refuses, which shows its 「ほかのアプリからペースト」 in Settings.
  `host_screen.input` answers such a text `{via: "cmd_v" | "edit_menu" |
  "keys", app?}`; without the edit menu's Paste it fails with the new
  error `paste_by_hand`, the text left on the clipboard. The companion
  gains `accessibility_info` and `list_apps` in stagent's subset of
  idb.proto.

### Fixed

- Host screen, Android: the discovery file an emulator leaves behind
  when it stops (`avd/running/pid_<pid>.ini`) is no longer read once its
  `<pid>` is no running process (signal 0 on Linux and macOS, the
  process's handle on Windows; the file is left as it is). An emulator
  started with `-port` only on the serial of such a file was listed
  `connectable: true` with the file's gRPC port, and `host_screen.start`
  answered `{}`, then `gone` for good; it is now `no_grpc`. `start` of
  an emulator whose gRPC does not answer while `adb devices` lists its
  serial is `unavailable` (`no_grpc`) instead of `{}` and `gone`; one
  adb does not list still waits for its AVD to run again (#679).

## 0.12.0

### Added

- `host_screen.list`: on a host where stagent runs in Windows' WSL, the
  `browser` and `android` sections have `note: {code: "wsl", message}`
  (`Section<T>` gains `note?`), since the browsers and emulators on the
  Windows side are not visible from WSL (#675).
- Host screen: an Android emulator or iOS simulator that stops no longer
  ends the stream. After `state: "gone"` stagent looks for it every 3
  seconds and, once it runs again, connects anew and sends `state:
  "live"` and frames in the same tab: an emulator by its AVD on any
  serial (`Target` has `avd`; `grpc.port` and `grpc.token` read again),
  a simulator by its `udid` (its companion started again). `start` of
  one that is not running answers `{}` and sends `gone`, then waits the
  same way; so does an iOS `start` while simctl fails to list (a
  timeout, say), which was `not_found`. Only no Xcode is still
  `not_found` (#662).
- Host screen: `host_screen.pause` and `host_screen.resume` (no params,
  `{}`) stop and restart capturing while the app does not show the tab
  (another tab, the app in the background, the screen off; #634).
  `pause` drops the frame kept and stops the target's capture: a
  browser's `Page.stopScreencast` (its WebSocket stays open), an Android
  emulator's `streamScreenshot` cut, an iOS simulator's MJPEG video
  stream closed (the companion kept, asked `describe` every 5/3 s so it
  does not shut down idle). `resume` sends one full frame at once, then
  streams again. Like `ack`, both are answered at once, never behind a
  slow `input`; without a stream → `bad_request`; `pause` while paused
  and `resume` while not paused → `{}`. Acks are answered while paused
  (#651).

### Changed

- `host_screen.input` on an Android emulator pastes every `text`, ASCII
  too (gRPC `setClipboard`, then `KEYCODE_PASTE`), instead of `adb shell
  input text`, which a Japanese Gboard typed as romaji to kana ("wifi
  test" became "ウィフぃてｓｔ"). `enter` alone sends only `KEYCODE_ENTER`
  and leaves the clipboard alone (#672).
- Host screen: an Android emulator's image with the same pixels as the
  last one is dropped before JPEG encoding, so a still display sends no
  frame (the emulator sends about 20 a second); an iOS simulator's MJPEG
  is quality 0.5 (was 0.7) at scale 0.5 and `fps` 0 (#662).
- Host screen: at most 10 frames a second are sent. A frame captured
  within 100 ms of the last one sent waits in the place that keeps the
  newest frame (the one used while two frames wait for their ack), and
  is sent once 100 ms passed (#651).

### Fixed

- Host screen, Android: an `emulator-<port>` that `adb devices` shows
  `offline` without a discovery file (any process listening on an odd
  port of 5555–5585) is no longer listed as a `no_grpc` emulator, nor
  asked `emu avd name` (which held `host_screen.list` up ~2 s);
  `host_screen.start` finds no emulator there. Booting emulators (offline
  with a file) and ones without a file in the `device` state are still
  listed (#673).
- `host_screen.list` lists browsers on Linux (WSL too): Chromium writes
  its command line over its arguments as one space-joined string, so no
  browser's `--remote-debugging-port` / `--remote-debugging-pipe` was
  found and none was listed. Such a command line is now read for its
  executable and switches (split at ` --`; a `--user-data-dir` with
  spaces is found, child processes with `--type=` are still left out);
  NUL-separated command lines read as before (#671).

## 0.11.0

### Added

- Host screen (ホストの画面, ADR 0008): `host_screen.list`,
  `host_screen.start`, `host_screen.ack`, `host_screen.stop` and the
  notifications `host_screen.frame` and `host_screen.state` show a page of
  a browser an agent drives in an app tab, over the tab's own bridge. The
  browsers listed are the user's Chromium-family browsers started with
  `--remote-debugging-port` (`0` read from `DevToolsActivePort`), each
  port checked with `/json/version`, with their page targets and the tool
  told by the profile path. A page is shown with one
  `Page.captureScreenshot` and then `Page.startScreencast` (JPEG, quality
  70); at most two frames wait for the app's ack, of the frames taken
  meanwhile only the newest is kept, and a stalled app gets the
  screencast paused and resumed with a full frame. Served to bridges
  under a coding agent too (#640).
- `host_screen.list`: a browser started with `--remote-debugging-pipe`
  only is listed with `connectable: false` and `reason: "pipe_only"`
  (with its tool), and `cdp_ports` names ports on 127.0.0.1 by hand: each
  that answers `/json/version` as CDP is listed with `manual: true`, in
  one item with a browser found on the same port; a port outside 1–65535
  is `bad_request` (#642).
- Host screen: Android emulators. `host_screen.list` has an `android`
  section: the emulators of `adb devices -l` (never a physical device),
  matched by serial with the files they advertise their gRPC port in
  (`avd/running/pid_<pid>.ini`), with the AVD name; one without gRPC
  (started with `-port` only) is `connectable: false`, `reason:
  "no_grpc"`, and a host without adb gets `missing: {code:
  "adb_not_found"}`. `host_screen.start {target: {kind: "android",
  serial}}` shows the emulator through its gRPC `streamScreenshot`
  (Bearer `grpc.token`, RGB888 at half resolution, encoded to JPEG
  quality 70); `width` and `height` are the display's px and swap when
  it turns, and a stopped emulator sends `state: "gone"` (#643).
- Host screen of an iOS simulator (macOS only, capability
  `host_screen_ios`): `host_screen.list` has an `ios` section with the
  booted iOS simulators (`xcrun simctl`; `missing` `xcode_not_found`
  without Xcode, `simctl_failed` with simctl's error as the message when
  it fails or runs out of time, a simulator `companion_not_found` without
  idb_companion), and `host_screen.start {target: {kind: "ios", udid}}`
  shows one through an idb_companion started for the stream on a private
  Unix socket (no TCP port): its MJPEG `video_stream` (half resolution,
  quality 0.7, redrawn screens only), sized in points. The companion is
  stopped with the stream (`stop`, another `start`, the bridge exiting;
  `--idle-shutdown-time` should stagent die), and a simulator shut down
  sends `state: "gone"`. Other hosts answer `unsupported` (#645).
- `host_screen.input` operates the page shown: `tap`, `drag`, `scroll`,
  `text` and `navigate` (`back`, `forward`, `reload`, `url`), points as
  fractions of the frame turned into CSS px with the screencast's
  metadata (`deviceWidth`, `deviceHeight`, `offsetTop`,
  `pageScaleFactor`). A browser gets only `Input.*` (`insertText` for
  text in any script, an Enter key event for `enter`), `Page.navigate`,
  `Page.reload` and the history's `Page.getNavigationHistory` and
  `Page.navigateToHistoryEntry`; `key` is `unsupported`. Inputs
  run in order on `start`'s queue while acks are answered at once, so a
  slow input never stops the frames (#641).
- `host_screen.input` operates an Android emulator: `tap`, `drag` and
  `scroll` (one swipe from the cursor) through gRPC `sendTouch` in the
  display's px, turned back with the emulator's rotation; `key` `back`,
  `home`, `recents` through `adb shell input keyevent` (works with
  `hw.keyboard=no`, no gRPC `sendKey`); `text` through `adb shell input
  text` when printable ASCII, else gRPC `setClipboard` and
  `KEYCODE_PASTE`, with `KEYCODE_ENTER` for `enter`. `navigate` is
  `unsupported` (#644).
- `host_screen.input` operates an iOS simulator: `tap`, `drag`, `scroll`
  (one swipe from the cursor), `text` of ASCII and `key: "home"`, as HID
  events on the stream's idb_companion `hid` stream, kept open and
  reopened when the companion ends it (no program per input). Points are
  fractions of the frame in pt; text is typed as key presses of a US
  keyboard (Shift for capitals and the upper symbols), `enter` as Return.
  `back`, `recents`, `navigate` and text with a character other than
  ASCII (Japanese input is pending, #649) are `unsupported`. A drag cut
  short (the bridge ending) lifts its touch, and stopping the stream lets
  the companion perform what was sent (up to a second) before stopping
  it (#646).

## 0.10.1

### Fixed

- Windows: a task's shared directories (`worktree.sharedDirectories`) of a
  source checkout on a network share (a UNC path or a mapped drive) are
  no longer junctions. An administrator could make one there, but the
  share's server resolves it as its own path, so the copy stage said
  `linked` and the worktree's directory did not open. Such a directory is
  now a directory symbolic link, or copied with the one warning line when
  that cannot be made either (#531).
- `follow`: an omp chat showed the conversation of another agent in the
  same directory, the one the PC was just working in, while the terminal
  showed its own: an omp that has not written since it started (`omp -r`
  after picking a session, `omp -c`, a session resumed or switched to in
  omp, a new session before its first answer), and on macOS, where
  stagent cannot see open files, any omp once the other agent wrote last.
  The session omp records for the agent's terminal
  (`~/.omp/agent/terminal-sessions/`) now decides, and a new session not
  written yet shows no conversation instead of a guess.

## 0.10.0

### Added

- `github.status {items[{repo, kind, number, branch?}]}` (Windows,
  capability `github`) reports the state gh gives each issue or pull
  request (`OPEN`, `CLOSED`, `MERGED`), and for an issue with `branch`
  the state of that branch's pull request as `pr_state` (the answer
  echoing `branch`), so the app can
  mark tasks whose issue was closed or whose pull request was merged;
  `github.activity` only lists open ones. Items gh cannot answer are left
  out (#502).
- Tasks (タスク): an Issue or PR worked on in a worktree of a source
  checkout, or in the checkout itself (`in_place`). The daemon keeps them
  in `state/tasks.json` (mode 0600; only the daemon writes it, only
  `uninstall --level purge` removes it), keyed by the checkout's
  canonical path and the number. `task.list` answers every task in the
  order they were created, `watch` gains `tasks[]`, and `task.updated`
  (the whole `Task`) and `task.removed {id}` follow the changes. A `ready`
  task whose worktree directory is gone or that `git worktree list` no
  longer lists becomes `missing` on `task.list`; a `preparing` task whose
  prepared session ended or never registered becomes `failed` with
  `error: "interrupted"`, also after a daemon restart, and a task still
  `removing` when the daemon starts is `ready` with that error, keeping
  its `stage`. A change `tasks.json` cannot take is not made: the call
  answers `internal`. After a daemon restart the bridge's restored watch
  sends every task as `task.updated` and gone ones as `task.removed`
  (#502).
- `Session.task_id`: the daemon ties a session to the worktree task whose
  worktree holds its `cwd` (also live sessions, when the task is
  registered), and to an `in_place` task only its prepared session and
  sessions started with the new `session.spawn {task_id}` (#502).
- Local IPC for the bridge and `stagent task`: `task.register` (atomic by
  the key; a `failed` task is prepared again, another number's `in_place`
  task on the checkout is the new error `in_use`), `task.progress {id,
  stage, error?, session_id?}` and `task.forget {id}`, refused with
  `agent_refused` to a coding agent's process tree like `config.set`.
  `stop` on a task already `removing` is `busy`, so only one removal runs;
  stage `agent` with an error from the prepared session fails a `ready`
  task. Error codes `not_clean` and `busy` are reserved for `task.remove`
  (#502).
- Pushes of a session tied to a task are titled `<host> · #N <title>` on
  ntfy, the generic webhook and chat destinations; a digest of several is
  not (#502).
- `task.create` (bridge, spawn queue) creates a task and starts its first
  session: the daemon registers it (`{task, session?, existed}`: a `ready`
  task starts nothing, a `preparing` one answers its live prepared
  session, a `failed` one is prepared again), the bridge makes the empty
  directory `<parent of the checkout>/<checkout name>-<N>` and starts the
  session there (in the checkout itself for `in_place`), so its `cwd` is
  the task's place from the start. In it the internal `stagent task run
  <id> -- <command…>` fetches, makes the branch (an issue's `<N>-<slug>`
  or the given `branch`, linked with `gh issue develop`, made locally
  from `base` when gh fails; a PR's head branch with `gh pr checkout` in
  a detached worktree), adds or adopts the worktree, runs `orca.yaml`'s
  `scripts.setup` (login shell; on Windows a `.cmd` runner, or Git for
  Windows' bash for a `#!` script) and then becomes the agent, reporting
  each stage with `task.progress` and showing everything in the
  terminal. A failing stage leaves the task `failed` without an agent —
  also when the agent itself cannot be started after stage `agent`;
  `task.create` again carries on. An empty `command` opens a terminal.
  Refused with `agent_refused` to a coding agent's process tree (#502).
- A task's prepared session reports the harness of the command `stagent
  task run` runs (`Session.harness`), and a Codex there gets
  `--no-daemon` like any other (#502).
- A worktree task gets its files to copy and shared directories in the
  new `copy` stage of `stagent task run`, between `worktree` and `setup`:
  `orca.yaml`'s `worktree.sharedDirectories` of the source checkout that
  are gitignored are linked (a symbolic link; on Windows a junction, else
  a directory symbolic link; one inside another listed one goes with it),
  then the gitignored files the checkout's
  `.worktreeinclude` (Orca's rules: literal paths — a line with `*`, `?`
  or `[` is invalid —, at most 1,000, a file
  up to 256 KiB) and the app's patterns (`config.json`
  `repos.<checkout>.worktree_include`, git pathspec globs) select are
  copied, up to 2 GiB and 50,000 entries. Tracked, not ignored, missing,
  unmatched, invalid and over-the-limit entries and what lies in a linked
  directory are skipped with a line in the terminal (tracked ones
  silently); a destination that exists is left alone, so a retry copies
  only what is missing. A directory that cannot be linked is one warning
  line and what is listed under it is copied instead; nothing here fails
  the task (#502).
- `task.preview {repo, worktree_include?}` (bridge, its own queue,
  allowed for agent descendants) answers what a new worktree of the
  checkout would get, resolved by the same code: `copies` (at most 500,
  `truncated`), `links`, `skipped` with the reason (`tracked`,
  `not_ignored`, `missing`, `no_match`, `under_link`, `invalid`,
  `limit`), `total_bytes`, the checkout's `orca.yaml` `setup` and
  `orca_error`. `worktree_include` tries patterns that are not saved
  (#502).
- `config.json` `repos.<checkout>.worktree_include`, set one checkout at
  a time with `config.set` (`null` removes one); `config.set` refuses
  with `bad_request` a key that is not an absolute clean path and a
  pattern that is empty, a negation, absolute or has a `..` or `.git`
  component (#502).
- `task.remove {id, force?, stop_sessions?, skip_archive?, dry_run?}`
  (bridge, its own queue) removes a task (片付け). `dry_run` reports the
  worktree's uncommitted changes as `git status --porcelain` lines
  (`changes[]`, ignored files and shared-directory links left out),
  the branch's commits on no remote (`unpushed`) and the task's live
  sessions (`sessions[]`). Changes need `force` (`not_clean`), live
  sessions `stop_sessions` (`busy`): they get hangup, then kill after 5 s,
  and sessions bound to the task meanwhile are stopped too. A `preparing`
  task is `busy` without `stop_sessions`; with it its sessions are stopped
  first (it fails, interrupted) and the removal goes on, `in_place` too.
  Then `orca.yaml`'s `scripts.archive` of the source checkout runs in the
  worktree — in a session of the source checkout running the internal
  `stagent task remove <id>`, so its output shows in a terminal — unless
  `skip_archive`; only the shared directories' links are unlinked, `git
  worktree remove` runs, and the branch is deleted when git sees it
  merged or gh says its pull request was merged at the branch's commit
  (a squash merge; a PR task's pull request is looked up by its number,
  so a fork's is found too), otherwise kept and named in `kept_branch`; remote
  branches are never deleted. Each stage is `Task.stage` while
  `removing`; a failing one (archive, `git worktree remove`) takes the
  task back with its `error`, keeping that `stage` (the app offers to skip
  the archive when it is `archive`), and answers `internal`; a second
  removal of a task being removed is `busy`. The reply comes when
  the removal has finished. A `missing` task is forgotten and its
  worktree pruned, an `in_place` one only forgotten. Refused with
  `agent_refused` to a coding agent's process tree (#502).
- `task.forget` takes `kept_branch?` and `task.removed` carries it, so
  every watcher learns the branch a removal kept (#502).

### Fixed

- A chat message no longer lands on claude's workspace trust prompt
  (`Accessing workspace:`, shown at startup in a folder not trusted yet).
  Its options have no numbers, so the holder did not see a menu, and the
  message's Enter picked the highlighted `No, exit`, ending claude. Now
  `session.input` refuses the message with `menu_open` while the prompt is
  up, as on Codex's folder trust prompt; the user answers it in the
  terminal, and claude then sends the initial prompt it held. stagent
  writes no trust (#502).
- `session.spawn` of a program that ends at once no longer fails with
  `holder exited during start` when the holder ended with it before the
  bridge reached it: the answer is the session the daemon lists. The same
  race made `task.create` report a task run that failed at once as a
  start failure, and `task.remove` an archive that failed at once (#502).
- Under proot's fake root (`proot -0`, an Ubuntu guest on Android), the
  chat view and the agent probe find the agents: stagent took its
  own uid from `geteuid`, which proot fakes, and every process's from
  `/proc/<pid>/status`, which proot leaves at the kernel's uid, so every
  process counted as another user's. On Linux stagent now reads its own
  uids from `/proc/self/status` too; elsewhere the uids agree and nothing
  changes (#529).

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
