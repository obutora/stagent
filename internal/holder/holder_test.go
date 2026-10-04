//go:build !windows

package holder

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/wire"
)

func TestDetachedRawAttachInputAndClose(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "sh", "-c", "printf hello; read x; echo got:$x")
	c := dialHolder(t, l, id)

	var info wire.Session
	if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &info); err != nil {
		t.Fatal(err)
	}
	if info.ID != id || info.Mode != wire.ModeDetached || info.PID <= 0 || info.HolderPID != os.Getpid() ||
		info.Cols != 80 || info.Rows != 24 || info.Harness != wire.HarnessOther {
		t.Fatalf("session.info = %+v", info)
	}

	// Attach until the reset snapshot shows the program's first output.
	from := 0
	for attempt := 0; ; attempt++ {
		var res wire.AttachResult
		if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, &res); err != nil {
			t.Fatal(err)
		}
		if res.Cols != 80 || res.Rows != 24 || res.Mode != wire.AttachRaw {
			t.Fatalf("attach result %+v", res)
		}
		var out wire.OutputParams
		json.Unmarshal(c.next(t, &from, wire.NotifyOutput).Params, &out)
		if !out.Reset {
			t.Fatal("first output after attach is not a reset snapshot")
		}
		if bytes.Contains(out.Data, []byte("hello")) {
			break
		}
		if attempt > 200 {
			t.Fatalf("snapshot never contained hello: %q", out.Data)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "abc", Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	c.outputUntil(t, &from, "got:abc")
	var closed wire.ClosedParams
	json.Unmarshal(c.next(t, &from, wire.NotifyClosed).Params, &closed)
	if closed.ID != id || closed.ExitCode != 0 {
		t.Fatalf("closed = %+v", closed)
	}
	if r := waitExit(t, done); r.code != 0 || r.err != nil {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
	if _, err := os.Stat(l.HolderAddr(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("holder socket left behind: %v", err)
	}
	// Scrollback survives the session for the daemon's retention.
	if m, _ := filepath.Glob(filepath.Join(l.SessionDataDir(id), "*.seg")); len(m) == 0 {
		t.Fatal("no scrollback segment written")
	}
}

func TestDetachedScreenModeResizeAndErrors(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "sh", "-c", "read a; stty size; read b; exit 7")
	c := dialHolder(t, l, id)

	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachScreen, FPS: 30}, nil); err != nil {
		t.Fatal(err)
	}
	var first wire.OutputParams
	json.Unmarshal(c.next(t, &from, wire.NotifyOutput).Params, &first)
	if !first.Reset {
		t.Fatal("screen mode did not start with a reset frame")
	}

	if err := c.call(t, wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 100, Rows: 30}, nil); err != nil {
		t.Fatal(err)
	}
	var rz wire.ResizeParams
	json.Unmarshal(c.next(t, &from, wire.NotifyResize).Params, &rz)
	if rz.ID != id || rz.Cols != 100 || rz.Rows != 30 {
		t.Fatalf("resize notification %+v", rz)
	}
	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	c.outputUntil(t, &from, "30 100") // the program sees the new size

	for _, tc := range []struct {
		method string
		params any
		code   string
	}{
		{wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 0, Rows: 5}, wire.ErrBadRequest},
		{wire.MethodSessionInput, wire.InputParams{ID: id, Keys: []string{"hyper-x"}}, wire.ErrBadRequest},
		{wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: "stop"}, wire.ErrBadRequest},
		{wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: "video"}, wire.ErrBadRequest},
		{wire.MethodSessionInfo, wire.SessionRef{ID: "0000000000000000"}, wire.ErrNotFound},
		{"session.bogus", nil, wire.ErrUnknownMethod},
	} {
		if err := c.call(t, tc.method, tc.params, nil); errCode(err) != tc.code {
			t.Errorf("%s %+v: err %v, want %s", tc.method, tc.params, err, tc.code)
		}
	}

	if err := c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil); err != nil {
		t.Fatal(err)
	}
	var closed wire.ClosedParams
	json.Unmarshal(c.next(t, &from, wire.NotifyClosed).Params, &closed)
	if closed.ExitCode != 7 {
		t.Fatalf("closed = %+v, want exit 7", closed)
	}
	if r := waitExit(t, done); r.code != 7 {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}

func TestScreenModeCoalescesUpdates(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 40, 6, "sh", "-c",
		`read go; i=0; while [ $i -lt 300 ]; do printf '\r%d' $i; i=$((i+1)); done; echo; echo done; read x`)
	c := dialHolder(t, l, id)
	from := 0
	if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachScreen, FPS: 5}, nil); err != nil {
		t.Fatal(err)
	}
	client := vt.NewEmulator(40, 6)
	go io.Copy(io.Discard, client) // replies of the client-side emulator
	frames := 0
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	for !strings.Contains(client.String(), "done") {
		var p wire.OutputParams
		json.Unmarshal(c.next(t, &from, wire.NotifyOutput).Params, &p)
		client.Write(p.Data)
		frames++
	}
	if !strings.Contains(client.String(), "299") {
		t.Fatalf("final screen lacks the last update:\n%s", client.String())
	}
	// 300 redraws at 5 fps: intermediate states must have been skipped.
	if frames > 20 {
		t.Fatalf("%d frames for a burst of 300 updates at 5 fps", frames)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}

func TestMissingCommandExits127(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "stagent-test-no-such-command")
	r := waitExit(t, done)
	if r.code != ExitNotFound || r.err == nil {
		t.Fatalf("Run = %d, %v; want %d and an error", r.code, r.err, ExitNotFound)
	}
	if _, err := os.Stat(l.HolderAddr(id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket exists for a session that never started: %v", err)
	}
}

func TestSignalTerminateEndsSession(t *testing.T) {
	l := isolate(t)
	id, done := startDetached(t, l, 80, 24, "sh", "-c", "echo ready; exec sleep 60")
	c := dialHolder(t, l, id)
	from := 0
	c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil)
	c.outputUntil(t, &from, "ready")
	if err := c.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalTerminate}, nil); err != nil {
		t.Fatal(err)
	}
	var closed wire.ClosedParams
	json.Unmarshal(c.next(t, &from, wire.NotifyClosed).Params, &closed)
	if closed.ExitCode != 128+15 {
		t.Fatalf("exit code %d, want 143 (SIGTERM)", closed.ExitCode)
	}
	waitExit(t, done)
}

// A client that stops reading must not stall the session; once it reads
// again it gets a fresh snapshot instead of the bytes it missed.
func TestSlowRawClientIsResynchronized(t *testing.T) {
	l := isolate(t)
	// A small screen keeps the emulator cheap: the point is the client.
	id, done := startDetached(t, l, 20, 5, "sh", "-c",
		"head -c 1500000 /dev/zero | tr '\\0' x; echo; echo flood-done; read z")
	var conn net.Conn
	deadline := time.Now().Add(testTimeout)
	for {
		var err error
		if conn, err = ipc.Dial(l.HolderAddr(id), time.Second); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer conn.Close()
	codec := wire.NewCodec(conn, conn)
	codec.Request(1, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw})

	// Stall until the program finished flooding (seen through a second
	// connection's scrollback), then drain.
	probe := dialHolder(t, l, id)
	for {
		var sb wire.ScrollbackResult
		if err := probe.call(t, wire.MethodSessionScrollback, wire.ScrollbackParams{ID: id, MaxBytes: 64}, &sb); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(sb.Data, []byte("flood-done")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("flood never finished: the session stalled behind a slow client")
		}
		time.Sleep(20 * time.Millisecond)
	}

	resets, raw := 0, 0
	var last []byte
	r := bufio.NewReaderSize(conn, 1<<20)
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	dec := wire.NewCodec(r, conn)
	for !bytes.Contains(last, []byte("flood-done")) {
		m, err := dec.Read()
		if err != nil {
			t.Fatalf("read: %v (resets %d, raw bytes %d)", err, resets, raw)
		}
		if m.Method != wire.NotifyOutput {
			continue
		}
		var p wire.OutputParams
		json.Unmarshal(m.Params, &p)
		if p.Reset {
			resets++
			last = p.Data
		} else {
			raw += len(p.Data)
			last = append(last, p.Data...)
		}
	}
	if resets < 2 {
		t.Fatalf("got %d reset snapshots, want the initial one plus a resync", resets)
	}
	if raw >= 1500000 {
		t.Fatalf("slow client received all %d flooded bytes; nothing was dropped", raw)
	}
	probe.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}

func TestDaemonLinkReportsLifecycleAndReregisters(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	id, done := startDetached(t, l, 90, 20, "sh", "-c",
		`printf '\033]2;my title\007'; read a; printf '\033]9;need you\007'; read b; exit 3`)

	from := 0
	reg := d.waitFor(t, &from, "holder.register", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	var s wire.Session
	json.Unmarshal(reg.Params, &s)
	if s.ID != id || s.Mode != wire.ModeDetached || s.PID <= 0 || s.HolderPID != os.Getpid() ||
		s.Cols != 90 || s.Rows != 20 || s.State != wire.StateWorking || s.StartedAt == 0 ||
		len(s.Command) != 3 || s.Command[0] != "sh" || s.Cwd == "" {
		t.Fatalf("registered session %+v", s)
	}
	if s.Title != "my title" { // unless the output was seen before registering
		d.waitFor(t, &from, "title patch", isPatch(func(p wire.SessionPatch) bool {
			return p.ID == id && p.Title != nil && *p.Title == "my title"
		}))
	}

	// The daemon restarts: the holder must register again with its
	// current state, and the session keeps running meanwhile.
	d.dropConnections()
	rereg := d.waitFor(t, &from, "re-registration", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	json.Unmarshal(rereg.Params, &s)
	if s.ID != id || s.Title != "my title" {
		t.Fatalf("re-registered session %+v", s)
	}

	c := dialHolder(t, l, id)
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	d.waitFor(t, &from, "waiting_input patch", isPatch(func(p wire.SessionPatch) bool {
		return p.State != nil && *p.State == wire.StateWaitingInput && *p.StateSource == wire.SourceTerminal
	}))
	nm := d.waitFor(t, &from, "holder.notify", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderNotify })
	var n wire.HolderNotifyParams
	json.Unmarshal(nm.Params, &n)
	if n.ID != id || n.Body != "need you" || n.Bell {
		t.Fatalf("holder.notify %+v", n)
	}

	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	d.waitFor(t, &from, "exited patch", isPatch(func(p wire.SessionPatch) bool {
		return p.State != nil && *p.State == wire.StateExited
	}))
	em := d.waitFor(t, &from, "holder.ended", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderEnded })
	var ended wire.ClosedParams
	json.Unmarshal(em.Params, &ended)
	if ended.ID != id || ended.ExitCode != 3 {
		t.Fatalf("holder.ended %+v", ended)
	}
	if r := waitExit(t, done); r.code != 3 {
		t.Fatalf("Run = %d, %v", r.code, r.err)
	}
}

// A program a client hung up ends with holder.ended hung_up (the app ending
// a kept shell is no abnormal exit); one that exits on its own does not.
// The holder reports its $CLAUDE_CLIENT_PRESENCE_FILE.
func TestHangupIsRecordedAndPresenceFileReported(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	presence := filepath.Join(t.TempDir(), "present")
	t.Setenv(wire.EnvClaudePresenceFile, presence)
	ended := func(from *int) wire.ClosedParams {
		em := d.waitFor(t, from, "holder.ended", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderEnded })
		var p wire.ClosedParams
		json.Unmarshal(em.Params, &p)
		return p
	}

	from := 0
	id, done := startDetached(t, l, 80, 24, "sh", "-c", "read x")
	reg := d.waitFor(t, &from, "holder.register", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	var s wire.Session
	json.Unmarshal(reg.Params, &s)
	if s.PresenceFile != presence {
		t.Fatalf("registered presence_file %q, want %q", s.PresenceFile, presence)
	}
	c := dialHolder(t, l, id)
	if err := c.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalHangup}, nil); err != nil {
		t.Fatal(err)
	}
	if p := ended(&from); p.ID != id || !p.HungUp {
		t.Fatalf("holder.ended after hangup %+v", p)
	}
	waitExit(t, done)

	id, done = startDetached(t, l, 80, 24, "sh", "-c", "read x; exit 1")
	c = dialHolder(t, l, id)
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	if p := ended(&from); p.ID != id || p.HungUp || p.ExitCode != 1 {
		t.Fatalf("holder.ended of an exit %+v", p)
	}
	waitExit(t, done)
}

// attached follows the clients attached through session.attach: the
// daemon learns each change between none and some, session.info and a
// re-registration carry the current value, and replacing an attachment or
// detaching one of two clients does not report a detach.
func TestAttachedFollowsClients(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	id, done := startDetached(t, l, 80, 24, "sh", "-c", "read x")
	isRegister := func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister }
	attachedPatch := func(want bool) func(*wire.Msg) bool {
		return isPatch(func(p wire.SessionPatch) bool { return p.ID == id && p.Attached != nil && *p.Attached == want })
	}
	info := func(c *testClient) wire.Session {
		t.Helper()
		var s wire.Session
		if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	attach := func(c *testClient) {
		t.Helper()
		if err := c.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil); err != nil {
			t.Fatal(err)
		}
	}

	from := 0
	var s wire.Session
	json.Unmarshal(d.waitFor(t, &from, "holder.register", isRegister).Params, &s)
	a := dialHolder(t, l, id)
	if s.Attached || info(a).Attached {
		t.Fatal("attached before any client attached")
	}

	attach(a)
	d.waitFor(t, &from, "attached patch", attachedPatch(true))
	attachedAt := from
	if !info(a).Attached {
		t.Fatal("session.info not attached")
	}
	b := dialHolder(t, l, id)
	attach(b)
	attach(a) // replaces a's attachment
	if err := a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil); err != nil {
		t.Fatal(err)
	}
	if !info(a).Attached {
		t.Fatal("not attached while b is")
	}

	// The daemon restarts: the holder registers as attached.
	d.dropConnections()
	json.Unmarshal(d.waitFor(t, &from, "re-registration", isRegister).Params, &s)
	if !s.Attached {
		t.Fatalf("re-registered session %+v", s)
	}

	// The last client goes away without detaching.
	b.Close()
	d.waitFor(t, &from, "detached patch", attachedPatch(false))
	if info(a).Attached {
		t.Fatal("session.info still attached")
	}
	d.mu.Lock()
	for _, m := range d.msgs[attachedAt : from-1] {
		if attachedPatch(false)(m) {
			d.mu.Unlock()
			t.Fatalf("reported detached while a client was attached: %s", m.Params)
		}
	}
	d.mu.Unlock()

	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}

// While the daemon asks (holder.prompt_watch), the holder reports once
// that claude's permission menu it saw left the screen; a watch whose menu
// never showed reports nothing. No client is attached: the holder keeps
// the screen regardless.
func TestPromptWatchReportsMenuGone(t *testing.T) {
	l := isolate(t)
	d := startFakeDaemon(t, l)
	fixture, err := filepath.Abs("testdata/claude_permission_60x30.ansi")
	if err != nil {
		t.Fatal(err)
	}
	id, done := startDetached(t, l, 60, 30, "sh", "-c",
		`cat "$0"; read a; printf '\033[2J\033[H● Write(a.txt)\r\n  ⎿  Wrote 1 line\r\n'; read b; echo more; read c`, fixture)
	from := 0
	d.waitFor(t, &from, "holder.register", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderRegister })
	d.notifyHolders(t, wire.MethodHolderPromptWatch, wire.HolderPromptWatchParams{ID: id, Gen: 5, On: true})

	time.Sleep(4 * promptEvalEvery) // the menu is up
	if g := d.promptGones(0); len(g) != 0 {
		t.Fatalf("prompt_gone while the menu is up: %+v", g)
	}
	c := dialHolder(t, l, id)
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil) // answered
	m := d.waitFor(t, &from, "holder.prompt_gone", func(m *wire.Msg) bool { return m.Method == wire.MethodHolderPromptGone })
	var p wire.HolderPromptGoneParams
	json.Unmarshal(m.Params, &p)
	if p != (wire.HolderPromptGoneParams{ID: id, Gen: 5}) {
		t.Fatalf("prompt_gone %+v", p)
	}

	// A new watch whose menu never shows reports nothing, and the old one
	// does not report again.
	d.notifyHolders(t, wire.MethodHolderPromptWatch, wire.HolderPromptWatchParams{ID: id, Gen: 6, On: true})
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	time.Sleep(4*promptEvalEvery + 2*promptGoneAfter)
	if g := d.promptGones(0); len(g) != 1 {
		t.Fatalf("prompt_gone reports %+v, want only the first", g)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	waitExit(t, done)
}
