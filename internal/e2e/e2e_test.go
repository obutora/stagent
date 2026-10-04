// Package e2e drives a real `stagent bridge` process — and through it the
// daemon and detached holders it starts — the way the app does.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/hostid"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

const waitLong = 10 * time.Second

// buildStagent compiles cmd/stagent for the end-to-end tests that drive
// POSIX shells: they run on linux only.
func buildStagent(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("end-to-end test runs on linux only")
	}
	return compileStagent(t)
}

// compileStagent compiles cmd/stagent into a temp dir.
func compileStagent(t *testing.T) string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not in PATH")
	}
	bin := filepath.Join(t.TempDir(), "stagent")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command(goBin, "build", "-o", bin, "github.com/obutora/stagent/cmd/stagent").CombinedOutput()
	if err != nil {
		t.Skipf("building stagent failed:\n%s", out)
	}
	return bin
}

// app speaks the protocol to a bridge subprocess and records every
// notification it receives.
type app struct {
	*rpc.Client
	mu     sync.Mutex
	cond   *sync.Cond
	notifs []*wire.Msg
}

type pipeRW struct {
	io.Reader
	io.WriteCloser
}

func newApp(r io.Reader, w io.WriteCloser) *app {
	a := &app{}
	a.cond = sync.NewCond(&a.mu)
	a.Client = rpc.NewClient(pipeRW{r, w}, func(m *wire.Msg) {
		a.mu.Lock()
		a.notifs = append(a.notifs, m)
		a.mu.Unlock()
		a.cond.Broadcast()
	})
	go func() {
		<-a.Done()
		a.cond.Broadcast()
	}()
	return a
}

func (a *app) call(t *testing.T, method string, params, result any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLong+5*time.Second)
	defer cancel()
	if err := a.Call(ctx, method, params, result); err != nil {
		t.Fatalf("%s: %v", method, err)
	}
}

// waitFor returns the first notification (ever received) matching pred.
func (a *app) waitFor(t *testing.T, what string, pred func(*wire.Msg) bool) *wire.Msg {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	timer := time.AfterFunc(waitLong, a.cond.Broadcast)
	defer timer.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		for _, m := range a.notifs {
			if pred(m) {
				return m
			}
		}
		if time.Now().After(deadline) || isDone(a.Client) {
			t.Fatalf("timed out waiting for %s; got %d notifications: %s", what, len(a.notifs), summarize(a.notifs))
		}
		a.cond.Wait()
	}
}

// waitOutput waits until the concatenated output of session id (optionally
// only non-reset frames) contains want, and returns it.
func (a *app) waitOutput(t *testing.T, id, want string, skipReset bool) string {
	t.Helper()
	var got string
	a.waitFor(t, fmt.Sprintf("output of %s containing %q", id, want), func(*wire.Msg) bool {
		var buf bytes.Buffer
		for _, m := range a.notifs {
			if m.Method != wire.NotifyOutput {
				continue
			}
			var o wire.OutputParams
			if json.Unmarshal(m.Params, &o) == nil && o.ID == id && !(skipReset && o.Reset) {
				buf.Write(o.Data)
			}
		}
		got = buf.String()
		return strings.Contains(got, want)
	})
	return got
}

func isDone(c *rpc.Client) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

func summarize(ms []*wire.Msg) string {
	var b strings.Builder
	for _, m := range ms {
		p := string(m.Params)
		if len(p) > 160 {
			p = p[:160] + "…"
		}
		fmt.Fprintf(&b, "\n  %s %s", m.Method, p)
	}
	return b.String()
}

func eventOf(m *wire.Msg, kind, sessionID string) (wire.Event, bool) {
	var ev wire.Event
	if m.Method != wire.NotifyEvent || json.Unmarshal(m.Params, &ev) != nil {
		return ev, false
	}
	return ev, ev.Kind == kind && (sessionID == "" || ev.SessionID == sessionID)
}

func TestBridgeEndToEnd(t *testing.T) {
	bin := buildStagent(t)
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv(daemonclient.EnvExe, bin)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "bridge")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var holderPIDs []int
	t.Cleanup(func() {
		stdin.Close()
		cmd.Wait()
		shutdownDaemon(l)
		for _, pid := range holderPIDs {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
		if t.Failed() {
			t.Logf("bridge stderr:\n%s", stderr.String())
			logs, _ := filepath.Glob(filepath.Join(l.LogDir, "*.log"))
			for _, f := range logs {
				b, _ := os.ReadFile(f)
				t.Logf("%s:\n%s", filepath.Base(f), b)
			}
		}
	})
	a := newApp(stdout, stdin)

	var hello wire.HelloResult
	a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "e2e"}, &hello)
	if hello.Protocol != version.Protocol || hello.Home != home || !slices.Contains(hello.Capabilities, wire.CapSpawn) ||
		!hostid.Valid(hello.HostID) || hostid.Read(l.HostID) != hello.HostID {
		t.Fatalf("hello = %+v", hello)
	}
	var watch wire.WatchResult
	a.call(t, wire.MethodWatch, wire.WatchParams{}, &watch)

	// --- raw mode: a session that prints, reads a line, answers, exits 3.
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{
		Command: []string{"sh", "-c", "printf ready; read l; echo got:$l; exit 3"},
		Cols:    80, Rows: 24,
	}, &sp)
	s1 := sp.Session
	holderPIDs = append(holderPIDs, s1.HolderPID)
	if s1.Mode != wire.ModeDetached || s1.Cwd != home || s1.Cols != 80 || s1.Rows != 24 {
		t.Fatalf("spawned session = %+v", s1)
	}
	a.waitFor(t, "session.updated for s1", func(m *wire.Msg) bool {
		var s wire.Session
		return m.Method == wire.NotifySessionUpdated && json.Unmarshal(m.Params, &s) == nil && s.ID == s1.ID
	})
	a.waitFor(t, "session_started for s1", func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionStarted, s1.ID)
		return ok
	})

	waitScrollback(t, a, s1.ID, "ready")

	var att wire.AttachResult
	a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: s1.ID, Mode: wire.AttachRaw}, &att)
	if att.Mode != wire.AttachRaw || att.Cols != 80 || att.Rows != 24 {
		t.Fatalf("attach = %+v", att)
	}
	first := a.waitFor(t, "first output of s1", func(m *wire.Msg) bool {
		var o wire.OutputParams
		return m.Method == wire.NotifyOutput && json.Unmarshal(m.Params, &o) == nil && o.ID == s1.ID
	})
	var snap wire.OutputParams
	json.Unmarshal(first.Params, &snap)
	if !snap.Reset || !strings.Contains(string(snap.Data), "ready") {
		t.Fatalf("first raw output reset=%v data=%q", snap.Reset, snap.Data)
	}

	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: s1.ID, Text: "hello", Submit: true}, nil)
	a.waitOutput(t, s1.ID, "got:hello", true)
	closed := a.waitFor(t, "closed for s1", func(m *wire.Msg) bool {
		var c wire.ClosedParams
		return m.Method == wire.NotifyClosed && json.Unmarshal(m.Params, &c) == nil && c.ID == s1.ID
	})
	var cp wire.ClosedParams
	json.Unmarshal(closed.Params, &cp)
	if cp.ExitCode != 3 {
		t.Fatalf("closed = %+v, want exit_code 3", cp)
	}
	ended := a.waitFor(t, "session_ended for s1", func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionEnded, s1.ID)
		return ok
	})
	ev, _ := eventOf(ended, wire.EventSessionEnded, s1.ID)
	var ed wire.SessionEndedData
	if json.Unmarshal(ev.Data, &ed); ed.ExitCode != 3 {
		t.Fatalf("session_ended data = %s", ev.Data)
	}

	// --- screen mode: diffs of changed rows.
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{
		Command: []string{"sh", "-c", `printf 'screen-start\n'; read l; printf 'second:%s\n' "$l"; read l2`},
		Cols:    60, Rows: 10,
	}, &sp)
	s2 := sp.Session
	holderPIDs = append(holderPIDs, s2.HolderPID)
	waitScrollback(t, a, s2.ID, "screen-start")
	a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: s2.ID, Mode: wire.AttachScreen, FPS: 30}, &att)
	if att.Mode != wire.AttachScreen || att.Cols != 60 || att.Rows != 10 {
		t.Fatalf("screen attach = %+v", att)
	}
	first = a.waitFor(t, "first screen frame of s2", func(m *wire.Msg) bool {
		var o wire.OutputParams
		return m.Method == wire.NotifyOutput && json.Unmarshal(m.Params, &o) == nil && o.ID == s2.ID
	})
	json.Unmarshal(first.Params, &snap)
	if !snap.Reset || !strings.Contains(string(snap.Data), "screen-start") {
		t.Fatalf("first screen frame reset=%v data=%q", snap.Reset, snap.Data)
	}
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: s2.ID, Text: "abc", Submit: true}, nil)
	diffs := a.waitOutput(t, s2.ID, "second:abc", true)
	if strings.Contains(diffs, "screen-start") {
		t.Errorf("screen diffs redrew an unchanged row: %q", diffs)
	}

	var sb wire.ScrollbackResult
	a.call(t, wire.MethodSessionScrollback, wire.ScrollbackParams{ID: s2.ID}, &sb)
	if !strings.Contains(string(sb.Data), "screen-start") || !strings.Contains(string(sb.Data), "second:abc") ||
		sb.End-sb.Start != int64(len(sb.Data)) || sb.Start != sb.First {
		t.Fatalf("scrollback = start %d end %d first %d data %q", sb.Start, sb.End, sb.First, sb.Data)
	}

	a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: s2.ID, Signal: wire.SignalKill}, nil)
	a.waitFor(t, "closed for s2", func(m *wire.Msg) bool {
		var c wire.ClosedParams
		return m.Method == wire.NotifyClosed && json.Unmarshal(m.Params, &c) == nil && c.ID == s2.ID
	})

	// --- notifications: nothing is sent before a channel is enabled.
	var we *wire.Error
	if err := a.Call(t.Context(), wire.MethodNotifyTest, nil, nil); !errors.As(err, &we) || we.Code != wire.ErrNotConfigured {
		t.Fatalf("notify.test without a channel: %v, want not_configured", err)
	}
	pushes := make(chan wire.NotificationData, 4)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n wire.NotificationData
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &n)
		pushes <- n
	}))
	defer hook.Close()
	a.call(t, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {"host_label": "e2e", "webhook": {"enabled": true, "url": "` + hook.URL + `"}}}`)}, nil)
	a.call(t, wire.MethodNotifyTest, nil, nil)
	a.waitFor(t, "test notification event", func(m *wire.Msg) bool {
		ev, ok := eventOf(m, wire.EventNotification, "")
		var nd wire.NotificationData
		return ok && json.Unmarshal(ev.Data, &nd) == nil && nd.Reason == "test" && nd.Title == "SSH Term"
	})
	select {
	case n := <-pushes:
		if n.Title != "e2e · SSH Term" {
			t.Fatalf("test push titled %q", n.Title)
		}
	case <-time.After(waitLong):
		t.Fatal("no test push")
	}

	// The test push links to this host (the id hello returned), with no
	// session and no sequence ID.
	clicks := make(chan http.Header, 4)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clicks <- r.Header.Clone()
	}))
	defer ntfy.Close()
	a.call(t, wire.MethodConfigSet, wire.ConfigSetParams{Config: json.RawMessage(`{"notify": {"click_base": "sshtermx://open", "webhook": null, "ntfy": {"enabled": true, "server": "` + ntfy.URL + `", "topic": "t"}}}`)}, nil)
	a.call(t, wire.MethodNotifyTest, nil, nil)
	select {
	case h := <-clicks:
		if h.Get("Click") != "sshtermx://open?h="+hello.HostID || h.Get("X-Sequence-ID") != "" {
			t.Fatalf("test push Click %q X-Sequence-ID %q", h.Get("Click"), h.Get("X-Sequence-ID"))
		}
	case <-time.After(waitLong):
		t.Fatal("no ntfy test push")
	}

	// --- shutdown: daemon through a direct IPC client, bridge on stdin EOF.
	if err := shutdownDaemon(l); err != nil {
		t.Fatalf("daemon.shutdown: %v", err)
	}
	stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("bridge exit: %v", err)
		}
	case <-time.After(waitLong):
		t.Fatal("bridge did not exit after stdin closed")
	}
}

// TestDeliberateStopIsNotUndoneByHolders: `uninstall --level stop` shuts the
// daemon down; a running session must not start it again, but must
// re-register once something else does.
func TestDeliberateStopIsNotUndoneByHolders(t *testing.T) {
	bin := buildStagent(t)
	t.Setenv(paths.EnvHome, t.TempDir())
	t.Setenv(daemonclient.EnvExe, bin)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(bin, "run", "--detached", "--id", "00000000000000d1", "--", "sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		holder.Process.Kill()
		holder.Wait()
		shutdownDaemon(l)
	})
	sessions := func() int {
		conn, err := ipc.Dial(l.DaemonAddr, 200*time.Millisecond)
		if err != nil {
			return -1
		}
		c := rpc.NewClient(conn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var st wire.DaemonStatus
		if c.Call(ctx, wire.MethodDaemonStatus, nil, &st) != nil {
			return -1
		}
		return st.Sessions
	}
	waitSessions := func(want int) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for sessions() != want {
			if time.Now().After(deadline) {
				t.Fatalf("daemon never reported %d session(s)", want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitSessions(1) // the holder auto-started the daemon and registered

	if err := shutdownDaemon(l); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second) // several holder reconnect attempts
	if n := sessions(); n != -1 {
		t.Fatalf("daemon is running again after a deliberate stop (%d sessions)", n)
	}

	if err := daemonclient.StartDaemon(l); err != nil {
		t.Fatal(err)
	}
	waitSessions(1) // the holder found the new daemon and re-registered
}

// TestHolderOutlivesItsStderrReader: `stagent run --detached` started over
// an SSH exec channel has a pipe for stderr, whose reader goes away with
// the connection. A daemon replaced after that makes the holder log; the
// session must survive it and register with the new daemon (#250).
func TestHolderOutlivesItsStderrReader(t *testing.T) {
	bin := buildStagent(t)
	t.Setenv(paths.EnvHome, t.TempDir())
	t.Setenv(daemonclient.EnvExe, bin)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	const id = "00000000000000d2"
	holder := exec.Command(bin, "run", "--detached", "--id", id, "--", "sleep", "60")
	holder.Stderr = w
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	exited := make(chan struct{})
	go func() {
		holder.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		holder.Process.Kill()
		<-exited
		shutdownDaemon(l)
	})
	registered := func() bool {
		conn, err := ipc.Dial(l.DaemonAddr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		c := rpc.NewClient(conn, nil)
		defer c.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		var res wire.SessionsListResult
		if c.Call(ctx, wire.MethodSessionsList, nil, &res) != nil {
			return false
		}
		return slices.ContainsFunc(res.Sessions, func(s wire.Session) bool { return s.ID == id && s.State != wire.StateExited })
	}
	waitRegistered := func() {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for !registered() {
			select {
			case <-exited:
				t.Fatalf("holder exited: %v", holder.ProcessState)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("the holder never registered")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitRegistered()
	r.Close() // the SSH client went away

	if err := shutdownDaemon(l); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the holder logs that the daemon is gone
	if err := daemonclient.StartDaemon(l); err != nil {
		t.Fatal(err)
	}
	waitRegistered()
	log, err := os.ReadFile(filepath.Join(l.LogDir, id+".log"))
	if err != nil || !strings.Contains(string(log), "registered with the daemon") {
		t.Fatalf("holder log (%v):\n%s", err, log)
	}
}

// TestSpawnFindsAgentsOutsideTheExecPath: an SSH exec channel starts the
// bridge with a non-login shell's PATH. Agents installed where only the
// login profile puts them on PATH (bun's ~/.bun/bin in ~/.bash_profile) must
// still start, with the profile's exports; so must agents in well-known tool
// directories nobody put on PATH.
func TestSpawnFindsAgentsOutsideTheExecPath(t *testing.T) {
	bin := buildStagent(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	script := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script(filepath.Join(home, "profile-bin", "profagent"), "#!/bin/sh\necho profile-ran:$MYVAR; sleep 3\n")
	script(filepath.Join(home, ".bun", "bin", "bunagent"), "#!/bin/sh\necho bun-ran; sleep 3\n")
	script(filepath.Join(home, ".bash_profile"),
		"export PATH=\"$HOME/profile-bin:$PATH\"\nexport MYVAR=from-profile\necho noise from the profile\n")

	cmd := exec.Command(bin, "bridge")
	cmd.Env = []string{
		"HOME=" + home, paths.EnvHome + "=" + home, daemonclient.EnvExe + "=" + bin,
		"SHELL=" + bash, "PATH=/usr/bin:/bin",
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Wait()
		shutdownDaemon(l)
	})
	a := newApp(stdout, stdin)
	a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "e2e"}, nil)

	for _, tc := range []struct{ name, want string }{
		{"profagent", "profile-ran:from-profile"},
		{"bunagent", "bun-ran"},
	} {
		var sp wire.SpawnResult
		a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{tc.name}, Cols: 80, Rows: 24}, &sp)
		a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: sp.Session.ID}, nil)
		a.waitOutput(t, sp.Session.ID, tc.want, false)
	}
}

// waitScrollback polls session.scrollback until it contains want, so a
// following attach snapshot deterministically includes it.
func waitScrollback(t *testing.T, a *app, id, want string) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		var sb wire.ScrollbackResult
		a.call(t, wire.MethodSessionScrollback, wire.ScrollbackParams{ID: id}, &sb)
		if strings.Contains(string(sb.Data), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("scrollback of %s never contained %q: %q", id, want, sb.Data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// shutdownDaemon asks a running daemon to exit; nil if none is running.
func shutdownDaemon(l *paths.Layout) error {
	conn, err := ipc.Dial(l.DaemonAddr, time.Second)
	if err != nil {
		return nil
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.Call(ctx, wire.MethodDaemonShutdown, nil, nil)
	if errors.Is(err, rpc.ErrClosed) {
		return nil // it closed the connection while exiting
	}
	return err
}
