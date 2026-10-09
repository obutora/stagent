//go:build !windows

package holder

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// With a menu on the screen — claude's or Codex's permission menu, Codex's
// update notice or folder trust prompt at startup (#440), or claude's
// workspace trust prompt (#491) — a chat message (session.input with paste
// and submit) is refused with menu_open and not a byte of it reaches the
// program; the menu's answer ({text}) is typed.
func TestChatMessageRefusedOnMenu(t *testing.T) {
	for _, fixture := range []string{"claude_permission_60x30", "codex_approval_60x30", "codex_update_60x30", "codex_trust_60x30", "claude_trust_60x30"} {
		t.Run(fixture, func(t *testing.T) {
			l := isolate(t)
			path, err := filepath.Abs("testdata/" + fixture + ".ansi")
			if err != nil {
				t.Fatal(err)
			}
			// The title tells that the whole menu was drawn. No echo, as
			// under the program: the holder's answers to the queries
			// claude ends its screen with must not land on it.
			id, done := startDetached(t, l, 60, 30, "sh", "-c",
				`stty -echo; cat "$0"; printf '\033]0;menu\007'; read a; printf 'got<%s>END' "$a"`, path)
			c := dialHolder(t, l, id)
			from := 0
			if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, nil); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(testTimeout)
			for {
				var s wire.Session
				if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &s); err != nil {
					t.Fatal(err)
				}
				if s.Title == "menu" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the menu was never drawn")
				}
				time.Sleep(10 * time.Millisecond)
			}

			err = c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Paste: "hello", Submit: true}, nil)
			if errCode(err) != wire.ErrMenuOpen {
				t.Fatalf("chat message on the menu: %v, want %s", err, wire.ErrMenuOpen)
			}
			if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "1", Submit: true}, nil); err != nil {
				t.Fatal(err)
			}
			// The line read may start with the holder's answers to the
			// program's queries, never with the chat message.
			out := c.outputUntil(t, &from, "END")
			got := out[max(0, bytes.LastIndex(out, []byte("got<"))):]
			if !bytes.HasSuffix(got, []byte("1>END")) || bytes.Contains(got, []byte("hello")) {
				t.Fatalf("the program read %q, want only the answer", got)
			}
			waitExit(t, done)
		})
	}
}

// Once claude's workspace trust prompt was answered in the terminal and
// left the screen, a chat message goes through again (#491): claude sends
// the initial prompt it held itself, the chat goes on from there.
func TestChatMessageAfterTrustPrompt(t *testing.T) {
	l := isolate(t)
	path, err := filepath.Abs("testdata/claude_trust_60x30.ansi")
	if err != nil {
		t.Fatal(err)
	}
	id, done := startDetached(t, l, 60, 30, "sh", "-c", `stty -echo; cat "$0"; printf '\033]0;menu\007'
read a
printf '\033[2J\033[H● PONG\r\n\033]0;gone\007'
read b
printf 'got<%s>END' "$b"`, path)
	c := dialHolder(t, l, id)
	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, nil); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, c, id, "menu")
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Paste: "hello", Submit: true}, nil); errCode(err) != wire.ErrMenuOpen {
		t.Fatalf("chat message on the trust prompt: %v, want %s", err, wire.ErrMenuOpen)
	}
	// The user answers in the terminal (↓, Enter).
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "\x1b[B", Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, c, id, "gone")
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Paste: "hello", Submit: true}, nil); err != nil {
		t.Fatalf("chat message after the trust prompt: %v", err)
	}
	out := c.outputUntil(t, &from, "END")
	if got := out[max(0, bytes.LastIndex(out, []byte("got<"))):]; !bytes.Contains(got, []byte("hello")) {
		t.Fatalf("the program read %q, want the chat message", got)
	}
	waitExit(t, done)
}

// A permission menu that comes up after session.input accepted a chat
// message but before the queue wrote it drops the message: not a byte of
// it reaches the program, and the request fails with menu_open, so the
// app sends it again after the menu.
func TestQueuedChatMessageDroppedForMenu(t *testing.T) {
	l := isolate(t)
	fixture, err := filepath.Abs("testdata/claude_permission_60x30.ansi")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// More than the PTY buffers: the queue blocks writing it until the
	// program reads, and the chat message waits behind it.
	const fill = 8 << 20
	// The program draws the menu once told to (file "menu"), then reads
	// the filler and one more byte (file "read") and prints that byte.
	id, done := startDetached(t, l, 60, 30, "sh", "-c", `stty raw -echo
printf '\033]0;ready\007'
while [ ! -e "$1/menu" ]; do sleep 0.01; done
cat "$0"; printf '\033]0;menu\007'
while [ ! -e "$1/read" ]; do sleep 0.01; done
printf 'got<%s>END' "$(head -c $(($2 + 1)) | tail -c 1)"`, fixture, dir, strconv.Itoa(fill))
	c := dialHolder(t, l, id)
	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, nil); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, c, id, "ready")

	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: strings.Repeat("y", fill)}, nil); err != nil {
		t.Fatal(err)
	}
	msg := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		msg <- c.Call(ctx, wire.MethodSessionInput, wire.InputParams{ID: id, Paste: "msg", Submit: true}, nil)
	}()
	// The holder handles a connection's requests in order: once this is
	// answered, the message got past its check while no menu was up.
	waitTitle(t, c, id, "ready")
	select {
	case err := <-msg:
		t.Fatalf("chat message answered (%v) before the queue reached it; the PTY took the filler without blocking", err)
	default:
	}

	if err := os.WriteFile(filepath.Join(dir, "menu"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitTitle(t, c, id, "menu")
	if err := os.WriteFile(filepath.Join(dir, "read"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-msg; errCode(err) != wire.ErrMenuOpen {
		t.Fatalf("chat message the menu came up before: %v, want %s", err, wire.ErrMenuOpen)
	}
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "Q"}, nil); err != nil {
		t.Fatal(err)
	}
	if out := c.outputUntil(t, &from, "END"); !bytes.Contains(out, []byte("got<Q>END")) {
		t.Fatalf("the program read %q after the filler, want only the next input", out[max(0, len(out)-40):])
	}
	waitExit(t, done)
}

// waitTitle waits until the session's title is title.
func waitTitle(t *testing.T, c *testClient, id, title string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		var s wire.Session
		if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &s); err != nil {
			t.Fatal(err)
		}
		if s.Title == title {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("title %q never came, it is %q", title, s.Title)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
