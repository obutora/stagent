//go:build !windows

package holder

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// With claude's or Codex's permission menu on the screen, a chat message
// (session.input with paste and submit) is refused with menu_open and not
// a byte of it reaches the program; the menu's answer ({text}) is typed.
func TestChatMessageRefusedOnMenu(t *testing.T) {
	for _, fixture := range []string{"claude_permission_60x30", "codex_approval_60x30"} {
		t.Run(fixture, func(t *testing.T) {
			l := isolate(t)
			path, err := filepath.Abs("testdata/" + fixture + ".ansi")
			if err != nil {
				t.Fatal(err)
			}
			// The title tells that the whole menu was drawn.
			id, done := startDetached(t, l, 60, 30, "sh", "-c",
				`cat "$0"; printf '\033]0;menu\007'; read a; printf 'got<%s>END' "$a"`, path)
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
			if out := c.outputUntil(t, &from, "END"); !bytes.Contains(out, []byte("got<1>END")) {
				t.Fatalf("the program read %q, want only the answer", out)
			}
			waitExit(t, done)
		})
	}
}
