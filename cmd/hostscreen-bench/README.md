# hostscreen-bench

Measures the host screen's traffic (#1974). Not shipped.

It plays the app's host screen tab against a local `stagent bridge` (its
stdin/stdout, no SSH): `hello` → `host_screen.list` → `host_screen.start`
→ `host_screen.ack` for every frame the moment it arrives (as
`HostScreenController` does) → inputs as the app sends them → `stop`. A
fresh bridge (own `STAGENT_HOME`) runs for each scenario.

## The link

Both directions of the bridge's stdio go through an emulated link
(`link.go`): bytes leave one after another at the link's bandwidth, then
arrive after a fixed one-way delay.

| flag | default | |
|---|---|---|
| `-down-mbps` | 5 | stagent → app |
| `-up-mbps` | 1 | app → stagent |
| `-delay` | 40ms | one way, each direction: RTT 80 ms |

Not modelled: SSH/TCP framing overhead, loss, the SSH window (stagent's
sending is bounded by the acks: at most 2 frames in flight).

## Scenarios

Every kind shows the same pages, served by the harness on 127.0.0.1:
desktop Chrome gets a new tab per scenario through its CDP port, the
Android emulator opens them in Chrome (`am start … http://10.0.2.2:…`), the
iOS simulator in Safari (`simctl openurl`).

| scenario | page | inputs during the window |
|---|---|---|
| `still` | text and coloured cards | none |
| `scroll` | 240 numbered paragraphs | bursts of 13 `scroll {x:.5, y:.6, dy:-0.06}` 80 ms apart (the app's `scrollEvery`), then 5 s still; the first of each burst is timed |
| `text` | a textarea (32 px, caret hidden), tapped before the window | one letter every 2 s (`text`), each timed |
| `video` | a muted looping `<video>`: ffmpeg `testsrc2` 1280×720 30 fps, 10 s H.264, made once into the cache dir (`pages.go:videoArgs`) | none |

Scrollbars and the caret are hidden so that their fading and blinking are
not taken for an input's change.

## What it records

Per kind × scenario, over the window (`-duration`, default 30 s):

- down / up bytes per second on the wire (everything the link carried, on
  every channel), frames per second, full and tiles frames, JPEG bytes per
  frame (all the tiles' of a tiles frame), the mean JPEG quality and the
  px size of the picture. The quality is estimated from each JPEG's
  luminance table against the IJG one (exact for Chrome's and Go's
  encoders; Apple's tables get the closest IJG quality);
- input → screen change: from sending an input to the arrival of the first
  frame that looks different from the frame shown when it was sent. Frames
  are compared by mean luma over a grid of 160 cells along the longer side
  (2 cells differing by more than 12 make a change); pictures of other px
  with the same sides' ratio (stagent's half resolution while operating)
  are compared the same way on a grid of 40 cut in proportion. A tiles
  frame is drawn on the picture before it, as the app does, and the whole
  picture is compared. An input is not timed when a changed frame arrived
  within 500 ms before it (`unsettled`), and counts as `unchanged` when no
  different frame follows;
- the settle frame: per gesture (inputs less than 1 s apart), the last
  frame that arrived after its last input, before the next input and
  within 6 s (stagent settles at the latest 5 s after the last input) — the
  picture left on the still screen. Its quality, px,
  bytes and delay after the last input, and how many were full frames;
- stagent CPU over the window in % of one core (every channel's stagent
  process and their direct children, `ps`), and their whole-life CPU
  (getrusage at exit). The browser's and the simulator's JPEGs are made by
  Chrome and idb_companion, not stagent;
- `decode ms/frame`: Go `image/jpeg` decode of each received frame (of its
  tiles) on this machine — what stagent would add per frame to compare
  JPEG sources (browser, iOS) tile by tile.

## Running

Needs: desktop Chrome with a CDP port, a running Android emulator with
gRPC, a booted iOS simulator (macOS), `ffmpeg` for the video. From the
stagent module:

```sh
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  --remote-debugging-port=9222 --user-data-dir=/tmp/cdp-bench \
  --no-first-run --window-size=1280,800 &
~/Library/Android/sdk/emulator/emulator -avd Pixel_9_API_36 -no-snapshot-save -grpc 8554 &
go run ./cmd/hostscreen-bench -out results.json -md results.md
```

`-kinds`, `-scenarios`, `-repeat`, `-stagent <binary>` (default: builds
`./cmd/stagent`), `-keep-logs` (keeps the bridges' stderr) narrow or
change a run. The table goes to stdout (and `-md`), every number to
`-out`.

## Frame transports

`-transport` picks how frames travel:

- `json` (default): `host_screen.frame`, base64 in the bridge's JSON
  lines — the app before #1974.
- `binary`: the app since #1974 (`binary.go`). `hello` announces
  `host_screen_binary` and `host_screen_tiles`; right after it,
  `host_screen.channel` and a second exec, `stagent host-screen-channel`,
  with the token on its stdin, over the same emulated links. Records are
  acked on arrival; a `host_screen.frame` overtaken by a newer seq is
  acked, not shown; tiles not on the frame shown last are dropped and
  `host_screen.keyframe` asked once until a full frame comes.

A new one is an entry of `transports` in `transport.go` implementing
`Transport`: it opens its channels with `Conn.Exec` so that they share
the same emulated links, and calls the sink for each frame.

## Tests

`go test ./cmd/hostscreen-bench` covers the link's timing, the per-window
accounting, the change detection (across a half resolution too), the
settle frame, the JPEG quality estimate, tiles drawn on the picture
before, the binary transport's acks/keyframes and both channels counted
on one link, and the `ps` time parsing.
