//go:build linux

package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// startPersistBridge runs `stagent bridge` for an isolated home whose
// login shell is bash, and opens the app's watch.
func startPersistBridge(t *testing.T) (a *app, l *paths.Layout, bin, home string) {
	t.Helper()
	bin = buildStagent(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	home = t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv(daemonclient.EnvExe, bin)
	if l, err = paths.Resolve(); err != nil {
		t.Fatal(err)
	}
	// Only a login shell reads ~/.bash_profile.
	profile := "echo LOGIN-PROFILE-RAN\nPS1='$ '\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
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
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stdin.Close()
		cmd.Wait()
		shutdownDaemon(l)
		if t.Failed() {
			t.Logf("bridge stderr:\n%s", stderr.String())
			logs, _ := filepath.Glob(filepath.Join(l.LogDir, "*.log"))
			for _, f := range logs {
				b, _ := os.ReadFile(f)
				t.Logf("%s:\n%s", filepath.Base(f), b)
			}
		}
	})
	a = newApp(stdout, stdin)
	var hello wire.HelloResult
	a.call(t, wire.MethodHello, wire.HelloParams{Protocol: version.Protocol, Client: "e2e"}, &hello)
	if !slices.Contains(hello.Capabilities, wire.CapPersist) {
		t.Fatalf("hello capabilities %v lack %q", hello.Capabilities, wire.CapPersist)
	}
	a.call(t, wire.MethodWatch, wire.WatchParams{}, nil)
	return a, l, bin, home
}

// mark is the number of notifications received so far.
func (a *app) mark() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.notifs)
}

// waitOutputs waits until the output of session id received after mark
// contains want and returns those output notifications.
func (a *app) waitOutputs(t *testing.T, id string, mark int, want string) []wire.OutputParams {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	timer := time.AfterFunc(waitLong, a.cond.Broadcast)
	defer timer.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		var outs []wire.OutputParams
		var all bytes.Buffer
		for _, m := range a.notifs[mark:] {
			var o wire.OutputParams
			if m.Method == wire.NotifyOutput && json.Unmarshal(m.Params, &o) == nil && o.ID == id {
				outs = append(outs, o)
				all.Write(o.Data)
			}
		}
		if strings.Contains(all.String(), want) {
			return outs
		}
		if time.Now().After(deadline) || isDone(a.Client) {
			t.Fatalf("timed out waiting for output %q of %s; got %q", want, id, all.String())
		}
		a.cond.Wait()
	}
}

// sessionUpdated reports whether m is a session.updated for id matching
// pred.
func sessionUpdated(m *wire.Msg, id string, pred func(wire.Session) bool) bool {
	var s wire.Session
	return m.Method == wire.NotifySessionUpdated && json.Unmarshal(m.Params, &s) == nil && s.ID == id && pred(s)
}

// echoProgram answers each input line "l" with "<l>" and nothing else (no
// echo, no newline), so the bytes produced for an input are known exactly.
// "big" produces more output than a resume replays.
const echoProgram = `stty -echo; printf ready
while read l; do
  if [ "$l" = big ]; then head -c 1100000 /dev/zero | tr '\0' x; printf '<big-done>'
  else printf '<%s>' "$l"; fi
done`

func TestAttachResume(t *testing.T) {
	a, _, _, _ := startPersistBridge(t)
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{"sh", "-c", echoProgram}, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	t.Cleanup(func() { a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil) })
	waitScrollback(t, a, id, "ready")
	var sb wire.ScrollbackResult
	a.call(t, wire.MethodSessionScrollback, wire.ScrollbackParams{ID: id}, &sb)

	attach := func(since *int64) (wire.AttachResult, int) {
		t.Helper()
		mark := a.mark()
		var att wire.AttachResult
		a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw, Since: since}, &att)
		return att, mark
	}
	input := func(text string) {
		t.Helper()
		a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: text, Submit: true}, nil)
	}
	// checkEnds verifies that live output ends advance by exactly the bytes
	// delivered, starting at from, and returns the last end.
	checkEnds := func(outs []wire.OutputParams, from int64) int64 {
		t.Helper()
		end := from
		for _, o := range outs {
			if o.Reset {
				t.Fatalf("unexpected reset in live output: %+v", o)
			}
			if o.End != end+int64(len(o.Data)) {
				t.Fatalf("output %q ends at %d, want %d", o.Data, o.End, end+int64(len(o.Data)))
			}
			end = o.End
		}
		return end
	}

	// No since: a snapshot taken at the current stream end.
	att, mark := attach(nil)
	if att.Resumed || att.Offset != sb.End {
		t.Fatalf("plain attach = %+v, want offset %d (scrollback end), not resumed", att, sb.End)
	}
	outs := a.waitOutputs(t, id, mark, "ready")
	if !outs[0].Reset || outs[0].End != att.Offset {
		t.Fatalf("first output reset=%v end=%d, want a snapshot at %d", outs[0].Reset, outs[0].End, att.Offset)
	}
	input("one")
	outs = a.waitOutputs(t, id, mark, "<one>")
	last := checkEnds(outs[1:], att.Offset)
	if last != att.Offset+int64(len("<one>")) {
		t.Fatalf("last end %d after <one>, offset %d", last, att.Offset)
	}

	// Detached, the program keeps producing; resuming at the last end
	// replays exactly that and live output continues after it.
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	input("two")
	waitScrollback(t, a, id, "<two>")
	att, mark = attach(&last)
	if !att.Resumed || att.Offset != last+int64(len("<two>")) {
		t.Fatalf("resume attach = %+v, want resumed at offset %d", att, last+int64(len("<two>")))
	}
	outs = a.waitOutputs(t, id, mark, "<two>")
	if outs[0].Reset || string(outs[0].Data) != "<two>" || outs[0].End != att.Offset {
		t.Fatalf("first resumed output = reset %v data %q end %d", outs[0].Reset, outs[0].Data, outs[0].End)
	}
	input("three")
	outs = a.waitOutputs(t, id, mark, "<three>")
	last = checkEnds(outs, last)

	// Nothing missed: resumed with no replay at all.
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	att, mark = attach(&last)
	if !att.Resumed || att.Offset != last {
		t.Fatalf("resume without missed output = %+v, want resumed at %d", att, last)
	}
	input("four")
	outs = a.waitOutputs(t, id, mark, "<four>")
	if outs[0].Reset || string(outs[0].Data) != "<four>" {
		t.Fatalf("first output after an empty resume = %+v", outs[0])
	}
	last = checkEnds(outs, last)

	// A size change after since: the missed bytes were laid out for
	// another size, so the client gets a snapshot.
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	a.call(t, wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 100, Rows: 30}, nil)
	att, mark = attach(&last)
	outs = a.waitOutputs(t, id, mark, "<four>")
	if att.Resumed || !outs[0].Reset || outs[0].End != att.Offset || att.Cols != 100 {
		t.Fatalf("attach across a resize = %+v, first output reset=%v end=%d", att, outs[0].Reset, outs[0].End)
	}
	last = att.Offset

	// More than 1 MiB missed: snapshot.
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	input("big")
	waitScrollback(t, a, id, "<big-done>")
	att, mark = attach(&last)
	outs = a.waitOutputs(t, id, mark, "<big-done>")
	if att.Resumed || !outs[0].Reset || att.Offset < last+1100000 {
		t.Fatalf("attach after 1.1 MB = %+v, first output reset=%v", att, outs[0].Reset)
	}

	// A position the stream has not reached: snapshot.
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	future := att.Offset + 1000
	att, mark = attach(&future)
	if outs = a.waitOutputs(t, id, mark, "<big-done>"); att.Resumed || !outs[0].Reset {
		t.Fatalf("attach with a future since = %+v, first output reset=%v", att, outs[0].Reset)
	}
}

func TestShellSessionAndHangup(t *testing.T) {
	a, _, _, _ := startPersistBridge(t)
	bash, _ := exec.LookPath("bash")
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	if !slices.Equal(sp.Session.Command, []string{bash, "-l"}) || sp.Session.Mode != wire.ModeDetached {
		t.Fatalf("shell session = %+v", sp.Session)
	}
	waitScrollback(t, a, id, "LOGIN-PROFILE-RAN")
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "shopt -q login_shell && echo login-$((40+2))", Submit: true}, nil)
	waitScrollback(t, a, id, "login-42")

	// hangup is what closing a terminal does: the shell exits.
	a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalHangup}, nil)
	a.waitFor(t, "session_ended after hangup", func(m *wire.Msg) bool {
		_, ok := eventOf(m, wire.EventSessionEnded, id)
		return ok
	})
}

// A PC terminal session started with --handoff outlives the terminal: it
// becomes detached, and the app owns its size and drives it.
func TestHandoffKeepsSessionRunning(t *testing.T) {
	a, _, bin, home := startPersistBridge(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	pty.Setsize(ptmx, &pty.Winsize{Cols: 90, Rows: 20})
	const id = "00000000000000f1"
	program := `stty -echo; printf ready; while read l; do printf '<%s:%s>' "$l" "$(stty size)"; done`
	cmd := exec.Command(bin, "run", "--handoff", "--id", id, "--", "sh", "-c", program)
	cmd.Dir = home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tty.Close()
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Kill()
		<-exited
	})
	a.waitFor(t, "passthrough session listed", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return s.Mode == wire.ModePassthrough })
	})
	// Read the local terminal with poll: closing a master that a goroutine
	// is blocked reading would not release it, and the terminal would
	// never hang up.
	var local []byte
	fd := int(ptmx.Fd())
	deadline := time.Now().Add(waitLong)
	for !bytes.Contains(local, []byte("ready")) {
		if time.Now().After(deadline) {
			t.Fatalf("local terminal never showed the program's output: %q", local)
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		if n, _ := unix.Poll(pfd, 100); n > 0 {
			buf := make([]byte, 4096)
			n, _ := unix.Read(fd, buf)
			local = append(local, buf[:max(n, 0)]...)
		}
	}
	var we *wire.Error
	if err := a.Call(t.Context(), wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 77, Rows: 22}, nil); !errors.As(err, &we) || we.Code != wire.ErrNotSizeOwner {
		t.Fatalf("resize while the terminal owns the size: %v, want not_size_owner", err)
	}

	ptmx.Close() // the terminal hangs up
	a.waitFor(t, "session switched to detached", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return s.Mode == wire.ModeDetached })
	})
	a.call(t, wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 77, Rows: 22}, nil)
	mark := a.mark()
	a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil)
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "hi", Submit: true}, nil)
	a.waitOutputs(t, id, mark, "<hi:22 77>")
	select {
	case <-exited:
		t.Fatal("stagent run exited after the handoff")
	default:
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.notifs {
		if _, ok := eventOf(m, wire.EventSessionEnded, id); ok {
			t.Fatalf("session ended after the handoff: %s", m.Params)
		}
	}
}

// waitAfter returns the index of the first notification received after
// mark that matches pred.
func (a *app) waitAfter(t *testing.T, mark int, what string, pred func(*wire.Msg) bool) int {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	timer := time.AfterFunc(waitLong, a.cond.Broadcast)
	defer timer.Stop()
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		for i := mark; i < len(a.notifs); i++ {
			if pred(a.notifs[i]) {
				return i
			}
		}
		if time.Now().After(deadline) || isDone(a.Client) {
			t.Fatalf("timed out waiting for %s; got after mark: %s", what, summarize(a.notifs[mark:]))
		}
		a.cond.Wait()
	}
}

// The app sees whether a session has a client attached.
func TestAttachedReachesTheApp(t *testing.T) {
	a, _, _, _ := startPersistBridge(t)
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{"sh", "-c", "printf ready; read x"}, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	t.Cleanup(func() { a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil) })
	if sp.Session.Attached {
		t.Fatalf("spawned session attached: %+v", sp.Session)
	}
	attached := func(want bool) func(*wire.Msg) bool {
		return func(m *wire.Msg) bool {
			return sessionUpdated(m, id, func(s wire.Session) bool { return s.Attached == want })
		}
	}
	mark := a.mark()
	a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil)
	a.waitAfter(t, mark, "attached session", attached(true))
	var sl wire.SessionsListResult
	a.call(t, wire.MethodSessionsList, nil, &sl)
	if i := slices.IndexFunc(sl.Sessions, func(s wire.Session) bool { return s.ID == id }); i < 0 || !sl.Sessions[i].Attached {
		t.Fatalf("sessions.list %+v", sl.Sessions)
	}
	mark = a.mark()
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	a.waitAfter(t, mark, "detached session", attached(false))
}

// fakeHookClaude is a stand-in for Claude Code that announces itself the
// way Claude's installed hooks do (`stagent hook claude SessionStart`,
// $STAGENT_SESSION_ID from the session) and runs until it reads a line.
const fakeHookClaude = `#!/bin/sh
'%s' hook claude SessionStart '{"session_id":"e2e-conv","transcript_path":"/nonexistent/e2e-conv.jsonl"}'
echo FAKE-CLAUDE-READY
read line
echo FAKE-CLAUDE-DONE
`

// A claude started in a shell session makes it an agent session while it
// runs; once it quits, the session is a plain shell again.
func TestShellSessionFollowsAgentInside(t *testing.T) {
	a, _, bin, _ := startPersistBridge(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(fmt.Sprintf(fakeHookClaude, bin)), 0o755); err != nil {
		t.Fatal(err)
	}
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Shell: true, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	t.Cleanup(func() { a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil) })
	if sp.Session.Harness != wire.HarnessOther {
		t.Fatalf("shell session = %+v", sp.Session)
	}
	waitScrollback(t, a, id, "LOGIN-PROFILE-RAN")

	mark := a.mark()
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "PATH='" + dir + "':$PATH claude", Submit: true}, nil)
	promoted := a.waitAfter(t, mark, "session promoted to claude", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool {
			return s.Harness == wire.HarnessClaude && s.ConversationID == "e2e-conv"
		})
	})
	waitScrollback(t, a, id, "FAKE-CLAUDE-READY")
	isOther := func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return s.Harness == wire.HarnessOther })
	}
	// The daemon probes every 2s and gives up on an agent it never
	// recognized after 5 probes (10s). Running claude past two probes and
	// then expecting the revert within 3.5s of quitting shows the probe
	// recognized it in the foreground while it ran.
	time.Sleep(5 * time.Second)
	a.mu.Lock()
	for _, m := range a.notifs[promoted:] {
		if isOther(m) {
			a.mu.Unlock()
			t.Fatalf("reverted while claude was running: %s", m.Params)
		}
	}
	a.mu.Unlock()

	mark = a.mark()
	quit := time.Now()
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	i := a.waitAfter(t, mark, "session back to other", isOther)
	if d := time.Since(quit); d > 3500*time.Millisecond {
		t.Fatalf("reverted %v after claude quit; the probe did not see it running", d)
	}
	a.mu.Lock()
	var s wire.Session
	json.Unmarshal(a.notifs[i].Params, &s)
	a.mu.Unlock()
	if s.ConversationID != "" || s.TranscriptPath != "" || s.ExitCode != nil {
		t.Fatalf("reverted session %+v", s)
	}
	waitScrollback(t, a, id, "FAKE-CLAUDE-DONE")
}

// runInTerminal runs `stagent run <args> -- true` on a new terminal, as a
// shell wrapper would, and returns what it wrote to stderr.
func runInTerminal(t *testing.T, bin, dir string, args ...string) string {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer ptmx.Close()
	cmd := exec.Command(bin, append(append([]string{"run"}, args...), "--", "true")...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tty.Close()
	go io.Copy(io.Discard, ptmx)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("stagent run %v: %v\n%s", args, err, stderr.String())
	}
	return stderr.String()
}

// Before its program starts, `stagent run` in a terminal names the agent
// sessions held on the host — detached, no client attached — in one
// stderr line, and says nothing when there are none. --handoff=auto (the
// shell wrappers) is recorded for doctor's shell_wrapper.last_run_at.
func TestRunNamesHeldSessions(t *testing.T) {
	a, _, bin, home := startPersistBridge(t)
	const notice = "agent session is held on this host; `stagent ls` lists it"
	if got := runInTerminal(t, bin, home); got != "" {
		t.Fatalf("stderr without held sessions: %q", got)
	}

	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nread x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var sp wire.SpawnResult
	a.call(t, wire.MethodSessionSpawn, wire.SpawnParams{Command: []string{claude}, Cols: 80, Rows: 24}, &sp)
	id := sp.Session.ID
	t.Cleanup(func() { a.call(t, wire.MethodSessionSignal, wire.SignalParams{ID: id, Signal: wire.SignalKill}, nil) })
	if sp.Session.Harness != wire.HarnessClaude {
		t.Fatalf("spawned session %+v", sp.Session)
	}
	if got := runInTerminal(t, bin, home); !strings.Contains(got, "stagent: 1 "+notice) || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr with one held session: %q", got)
	}

	// An attached session is in use, not held.
	mark := a.mark()
	a.call(t, wire.MethodSessionAttach, wire.AttachParams{ID: id}, nil)
	a.waitAfter(t, mark, "attached session", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return s.Attached })
	})
	if got := runInTerminal(t, bin, home); got != "" {
		t.Fatalf("stderr with the session attached: %q", got)
	}
	mark = a.mark()
	a.call(t, wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
	a.waitAfter(t, mark, "detached session", func(m *wire.Msg) bool {
		return sessionUpdated(m, id, func(s wire.Session) bool { return !s.Attached })
	})

	lastRunAt := func() *int64 {
		t.Helper()
		out, err := exec.Command(bin, "doctor", "--json").Output()
		if err != nil {
			t.Fatalf("doctor: %v", err)
		}
		var r struct {
			ShellWrapper struct {
				LastRunAt *int64 `json:"last_run_at"`
			} `json:"shell_wrapper"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("doctor output %s: %v", out, err)
		}
		return r.ShellWrapper.LastRunAt
	}
	if v := lastRunAt(); v != nil {
		t.Fatalf("last_run_at before any --handoff=auto run: %d", *v)
	}
	start := time.Now().UnixMilli()
	t.Setenv("STAGENT_HANDOFF", "0") // the held-sessions line does not depend on the handoff choice
	if got := runInTerminal(t, bin, home, "--handoff=auto"); !strings.Contains(got, "stagent: 1 "+notice) {
		t.Fatalf("stderr of --handoff=auto with one held session: %q", got)
	}
	if v := lastRunAt(); v == nil || *v < start || *v > time.Now().UnixMilli() {
		t.Fatalf("last_run_at after a --handoff=auto run = %v, want >= %d", v, start)
	}
}
