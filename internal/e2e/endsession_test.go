//go:build linux

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// A session whose holder socket is gone while the holder runs on —
// `stagent uninstall --level purge` deletes the run directory and leaves
// sessions running, and a reinstalled daemon takes their holders back —
// stays listed live. The app's 終了 (session.signal hangup, over a new
// connection) used to fail with "no holder answers" and leave it there;
// the daemon now ends it: the program, which ignores SIGHUP, and the
// holder are killed and the end is recorded as a hangup.
func TestHangupEndsASessionWhoseHolderSocketIsGone(t *testing.T) {
	a, l, bin, home := startPersistBridge(t)
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{
		Command: []string{"sh", "-c", "trap '' HUP; printf ready; while :; do sleep 1; done"}, Cols: 80, Rows: 24,
	}, &sp)
	id, program, holder := sp.Session.ID, sp.Session.PID, sp.Session.HolderPID
	waitScrollback(t, a, id, "ready")
	if err := os.Remove(l.HolderAddr(id)); err != nil {
		t.Fatal(err)
	}
	a.hangUp()

	// The app connects again: its bridge has no holder connection to reuse.
	b := startBridgeFor(t, bin, home)
	var sl wire.SessionsListResult
	b.call(t, wire.MethodSessionsList, nil, &sl)
	if len(sl.Sessions) != 1 || sl.Sessions[0].ID != id || sl.Sessions[0].ExitCode != nil {
		t.Fatalf("sessions before 終了: %+v", sl.Sessions)
	}
	b.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalHangup}, nil)
	m := b.waitFor(t, "session_ended", func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionEnded, id)
		return ok
	})
	ev, _ := eventOf(m, wire.EventSessionEnded, id)
	var se wire.SessionEndedData
	if err := json.Unmarshal(ev.Data, &se); err != nil || !se.HungUp || se.ExitCode != 128+int(syscall.SIGKILL) {
		t.Fatalf("session_ended %s, want exit 137 hung up", ev.Data)
	}
	for what, pid := range map[string]int{"program": program, "holder": holder} {
		deadline := time.Now().Add(waitLong)
		for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			if time.Now().After(deadline) {
				t.Fatalf("%s (pid %d) still running", what, pid)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
