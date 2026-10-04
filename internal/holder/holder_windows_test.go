package holder

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// A program on the pseudo console prints, reads a typed line and exits
// with its own code, which the holder exits with too.
func TestWindowsDetachedRawAttachInputAndClose(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "cmd.exe", "/d", "/v:on", "/c", "echo hello& set /p x=& echo got:!x!& exit 3")
	c := dialHolder(t, l, id)

	var info wire.Session
	if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &info); err != nil {
		t.Fatal(err)
	}
	if info.ID != id || info.Mode != wire.ModeDetached || info.PID <= 0 || info.HolderPID != os.Getpid() ||
		info.Cols != 80 || info.Rows != 24 {
		t.Fatalf("session.info = %+v", info)
	}
	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, nil); err != nil {
		t.Fatal(err)
	}
	c.outputUntil(t, &from, "hello")
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "abc", Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	c.outputUntil(t, &from, "got:abc")
	var closed wire.ClosedParams
	json.Unmarshal(c.next(t, &from, wire.NotifyClosed).Params, &closed)
	if closed.ID != id || closed.ExitCode != 3 {
		t.Fatalf("closed = %+v, want exit 3", closed)
	}
	if r := waitExit(t, done); r.code != 3 || r.err != nil {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}

// Windows has no SIGHUP: hangup writes ^C and terminates the program 3 s
// later. An idle shell ignores ^C, so it ends with TerminateProcess's exit
// code 1; the holder still records the hangup, so the exit is no abnormal
// one, and the app's "end" takes well under 4 s.
func TestWindowsHangupEndsIdleShellAndIsRecorded(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	id, done := startDetached(t, l, 80, 24, "cmd.exe", "/d", "/k", "prompt READY$G")
	c := dialHolder(t, l, id)
	from := 0
	c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil)
	c.outputUntil(t, &from, "READY>")

	start := time.Now()
	if err := c.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalHangup}, nil); err != nil {
		t.Fatal(err)
	}
	dfrom := 0
	em := d.waitFor(t, &dfrom, "holder.ended", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderEnded })
	elapsed := time.Since(start)
	var ended wire.ClosedParams
	json.Unmarshal(em.Params, &ended)
	if ended.ID != id || !ended.HungUp || ended.ExitCode != 1 {
		t.Fatalf("holder.ended after hangup %+v, want hung_up with exit 1", ended)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("hangup took %v to end the shell, want under 4 s", elapsed)
	}
	waitExit(t, done)
}

// A client that comes back with the `end` it last wrote continues from
// there: the bytes it missed, not a snapshot.
func TestWindowsAttachResumesSince(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "cmd.exe", "/d", "/k", "prompt READY$G")
	c := dialHolder(t, l, id)
	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	var last int64
	var seen []byte
	for !bytes.Contains(seen, []byte("READY>")) {
		var o wire.OutputParams
		json.Unmarshal(c.next(t, &from, wire.NotifyOutput).Params, &o)
		seen = append(seen, o.Data...)
		last = o.End
	}
	// Let the prompt's trailing bytes arrive before taking the position.
	for settle := time.Now().Add(500 * time.Millisecond); time.Now().Before(settle); time.Sleep(50 * time.Millisecond) {
		c.mu.Lock()
		for ; from < len(c.notes); from++ {
			if m := c.notes[from]; m.Method == wire.NotifyOutput {
				var o wire.OutputParams
				json.Unmarshal(m.Params, &o)
				last = o.End
			}
		}
		c.mu.Unlock()
	}
	if err := c.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil); err != nil {
		t.Fatal(err)
	}

	// Output the client misses while away: 6*7 shows up only as 42.
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "set /a 6*7", Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testTimeout)
	for {
		var sb wire.ScrollbackResult
		if err := c.call(t, wire.MethodSessionScrollback, wire.ScrollbackParams{ID: id}, &sb); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(sb.Data), "42") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scrollback never showed 42: %q", sb.Data)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var res wire.AttachResult
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Since: &last}, &res); err != nil {
		t.Fatal(err)
	}
	var first wire.OutputParams
	json.Unmarshal(c.next(t, &from, wire.NotifyOutput).Params, &first)
	if !res.Resumed || first.Reset || first.End != res.Offset || !bytes.Contains(first.Data, []byte("42")) {
		t.Fatalf("attach since %d = %+v, first output reset=%v end=%d data=%q", last, res, first.Reset, first.End, first.Data)
	}

	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "exit 0", Submit: true}, nil)
	if r := waitExit(t, done); r.code != 0 {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}
