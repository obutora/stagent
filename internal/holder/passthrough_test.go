//go:build linux

package holder

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"testing"
	"time"

	cpty "github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/obutora/stagent/internal/wire"
)

// envRunArgs makes the test binary act as `stagent run` (see TestMain).
const envRunArgs = "STAGENT_TEST_RUN_ARGS"

func TestMain(m *testing.M) {
	if a := os.Getenv(envRunArgs); a != "" {
		var args []string
		if err := json.Unmarshal([]byte(a), &args); err != nil {
			os.Exit(99)
		}
		os.Exit(Main(args))
	}
	os.Exit(m.Run())
}

// localTerminal is the terminal a passthrough holder runs in, driven by the
// test through its master side.
type localTerminal struct {
	ptmx, tty *os.File
	cooked    uint32 // tty's lflag before stagent run took it
	mu        sync.Mutex
	out       []byte
}

// startPassthrough runs `stagent run args...` in a fresh 90x20 local
// terminal and records what the terminal shows. exited gets cmd.Wait's
// result.
func startPassthrough(t *testing.T, args []string) (lt *localTerminal, cmd *exec.Cmd, exited <-chan error) {
	t.Helper()
	a, _ := json.Marshal(args)
	ptmx, tty, err := cpty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	cpty.Setsize(ptmx, &cpty.Winsize{Cols: 90, Rows: 20})
	lt = &localTerminal{ptmx: ptmx, tty: tty, cooked: lflag(t, tty)}

	cmd = exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envRunArgs+"="+string(a))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			lt.mu.Lock()
			lt.out = append(lt.out, buf[:n]...)
			lt.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return lt, cmd, done
}

func (lt *localTerminal) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for {
		lt.mu.Lock()
		ok := bytes.Contains(lt.out, []byte(want))
		lt.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			lt.mu.Lock()
			defer lt.mu.Unlock()
			t.Fatalf("local terminal never showed %q; got %q", want, lt.out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func lflag(t *testing.T, f *os.File) uint32 {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag
}

func TestPassthroughMirrorsAndFollowsLocalTerminal(t *testing.T) {
	l := isolate(t)
	id := wire.NewSessionID()
	lt, cmd, exited := startPassthrough(t, []string{"--id", id, "--", "sh", "-c",
		"stty size; read a; stty size; read b; stty size; read c; exit 5"})
	ptmx, tty := lt.ptmx, lt.tty

	lt.waitFor(t, "20 90") // the program got the local size and its output is mirrored
	if lflag(t, tty)&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatal("local terminal is not in raw mode while the session runs")
	}

	c := dialHolder(t, l, id)
	var info wire.Session
	c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &info)
	if info.Mode != wire.ModePassthrough || info.Cols != 90 || info.Rows != 20 {
		t.Fatalf("session.info %+v", info)
	}
	if err := c.call(t, wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 50, Rows: 10}, nil); errCode(err) != wire.ErrNotSizeOwner {
		t.Fatalf("resize without force: %v, want not_size_owner", err)
	}

	// The local terminal resizes: the session follows.
	cpty.Setsize(ptmx, &cpty.Winsize{Cols: 100, Rows: 30})
	deadline := time.Now().Add(testTimeout)
	for info.Cols != 100 || info.Rows != 30 {
		if time.Now().After(deadline) {
			t.Fatalf("session did not follow the local size: %dx%d", info.Cols, info.Rows)
		}
		time.Sleep(10 * time.Millisecond)
		c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &info)
	}
	ptmx.Write([]byte("x\r"))
	lt.waitFor(t, "30 100")

	// The app takes the size explicitly; app input reaches the program too.
	if err := c.call(t, wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: 70, Rows: 15, Force: true}, nil); err != nil {
		t.Fatal(err)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	lt.waitFor(t, "15 70")

	ptmx.Write([]byte("z\r"))
	select {
	case err := <-exited:
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 5 {
			t.Fatalf("stagent run exited with %v, want the program's code 5", err)
		}
	case <-time.After(testTimeout):
		cmd.Process.Kill()
		t.Fatal("stagent run did not exit")
	}
	if got := lflag(t, tty); got&(unix.ICANON|unix.ECHO) != lt.cooked&(unix.ICANON|unix.ECHO) {
		t.Fatalf("local terminal mode not restored: lflag %#x, was %#x", got, lt.cooked)
	}
}

// Keystrokes at the local terminal set last_local_input_at; replies the
// terminal sends on its own (CPR, focus reports) do not, but focus reports
// set focused. `stagent attach` input (local) counts the same way, its
// focus lasting as long as its connection; the app's input does not.
func TestPassthroughRecordsLocalTyping(t *testing.T) {
	l := isolate(t)
	id := wire.NewSessionID()
	lt, cmd, exited := startPassthrough(t, []string{"--id", id, "--", "sh", "-c",
		"echo ready; read a; echo got; read b; exit 0"})
	lt.waitFor(t, "ready")
	c := dialHolder(t, l, id)
	info := func() wire.Session {
		var s wire.Session
		if err := c.call(t, wire.MethodSessionInfo, wire.SessionRef{ID: id}, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	// The program's cooked tty echoes what reached it, so the echo shows
	// the replies went through the holder.
	lt.ptmx.Write([]byte("\x1b[5;10R"))
	lt.waitFor(t, "[5;10R")
	lt.ptmx.Write([]byte("\x1b[I"))
	lt.waitFor(t, "[I")
	if s := info(); s.LastLocalInputAt != 0 || !s.Focused {
		t.Fatalf("after replies and focus in: last_local_input_at %d, focused %v", s.LastLocalInputAt, s.Focused)
	}
	lt.ptmx.Write([]byte("\x1b[O"))
	lt.waitFor(t, "[O")
	if info().Focused {
		t.Fatal("focused after focus out")
	}

	before := time.Now().UnixMilli()
	lt.ptmx.Write([]byte("hi\r"))
	lt.waitFor(t, "got")
	s := info()
	if s.LastLocalInputAt < before || s.LastLocalInputAt > time.Now().UnixMilli() {
		t.Fatalf("last_local_input_at = %d after typing at %d", s.LastLocalInputAt, before)
	}

	// Client input is not the local terminal's.
	time.Sleep(5 * time.Millisecond)
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "qz"}, nil)
	lt.waitFor(t, "qz")
	if got := info().LastLocalInputAt; got != s.LastLocalInputAt {
		t.Fatalf("session.input moved last_local_input_at %d -> %d", s.LastLocalInputAt, got)
	}

	typed := s.LastLocalInputAt
	a := dialHolder(t, l, id)
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "\x1b[I", Local: true}, nil)
	if s := info(); !s.Focused || s.LastLocalInputAt != typed {
		t.Fatalf("attach focus in: %+v", s)
	}
	time.Sleep(5 * time.Millisecond)
	a.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Text: "w", Local: true}, nil)
	if s := info(); s.LastLocalInputAt <= typed {
		t.Fatalf("attach typing left last_local_input_at at %d", s.LastLocalInputAt)
	}
	a.Close()
	deadline := time.Now().Add(testTimeout)
	for info().Focused {
		if time.Now().After(deadline) {
			t.Fatal("focus outlived the attach connection")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.call(t, wire.MethodSessionInput, wire.InputParams{ID: id, Submit: true}, nil)
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("stagent run exited with %v", err)
		}
	case <-time.After(testTimeout):
		cmd.Process.Kill()
		t.Fatal("stagent run did not exit")
	}
}

func TestPassthroughRequiresATerminal(t *testing.T) {
	isolate(t)
	args, _ := json.Marshal([]string{"--", "true"})
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envRunArgs+"="+string(args))
	out, err := cmd.CombinedOutput() // stdin is /dev/null
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != ExitUsage || !bytes.Contains(out, []byte("--detached")) {
		t.Fatalf("exit %v, output %q: want usage exit and a hint to use --detached", err, out)
	}
}
