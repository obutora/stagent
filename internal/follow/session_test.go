package follow

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const testSessionID = "0123456789abcdef"

// fakeHolder serves session.info for testSessionID over in-memory pipes.
type fakeHolder struct {
	mu    sync.Mutex
	pid   int
	exit  *int
	down  bool // dialing fails, as with no socket
	conns []net.Conn
}

func (h *fakeHolder) dial() (net.Conn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.down {
		return nil, errors.New("connect: no such file or directory")
	}
	c, s := net.Pipe()
	h.conns = append(h.conns, s)
	go rpc.Serve(context.Background(), s, func(_ context.Context, _ *rpc.Conn, m *wire.Msg) (any, error) {
		if m.Method != wire.MethodSessionInfo {
			return nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		return wire.Session{ID: testSessionID, PID: h.pid, ExitCode: h.exit}, nil
	})
	return c, nil
}

// exitNow makes the holder go away: its connections close and its socket
// is gone.
func (h *fakeHolder) exitNow() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.down = true
	for _, c := range h.conns {
		c.Close()
	}
	h.conns = nil
}

func (h *fakeHolder) set(f func(h *fakeHolder)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

func sessionFixture(t *testing.T, h *fakeHolder) *fixture {
	x := newFixture(t, sshTree())
	hs := &holderSession{id: testSessionID, dial: h.dial}
	t.Cleanup(hs.drop)
	x.f.loc.session = hs.root
	return x
}

// With --session the search starts at the session's program, not at this
// SSH connection's terminal: the tmux client running in the session leads
// to its pane, while the agent in the connection's own terminal and the one
// of another connection are not the target.
func TestFollowSessionSearchesSessionProgram(t *testing.T) {
	h := &fakeHolder{pid: 501}
	x := sessionFixture(t, h)
	p, _ := x.claude(102, 101, "tab")
	p.fg()
	x.tr.add(500, 1, "stagent", "stagent", "run", "--detached", "--id", testSessionID, "--", "bash", "-l")
	x.tr.add(501, 500, "bash", "bash", "-l")
	x.tr.add(502, 501, "tmux: client", "tmux", "attach", "-t", "main").fg()
	x.tr.add(200, 1, "tmux: server", "tmux", "new", "-s", "main")
	x.tr.add(201, 200, "bash", "-bash")
	_, path := x.claude(202, 201, "in session")
	x.mux.tmux = map[string][]tmuxClient{"": {{clientPID: 502, panePID: 201, paneID: "%3", session: "main"}}}
	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 2 || fs[0].Mux != muxTmux || fs[0].Reason != "" || fs[0].Diag != "" ||
		len(fs[0].Agents) != 1 || fs[0].Agents[0].Key != "claude:202" || fs[0].Agents[0].Pane != "%3" || selected(fs[0]) != "claude:202" {
		t.Fatalf("frames %+v", fs)
	}
	if fs[1].T != frameMessages || fs[1].Path != path || texts(fs[1].Messages) != "in session" {
		t.Fatalf("messages %+v", fs[1])
	}
}

// A session whose holder does not answer, or that ended, has no terminal;
// follow keeps scanning and finds the agent once the holder answers, and
// loses it again when the holder goes away.
func TestFollowSessionHolderGone(t *testing.T) {
	h := &fakeHolder{pid: 501, down: true}
	x := sessionFixture(t, h)
	x.tr.add(500, 1, "stagent", "stagent", "run", "--detached", "--", "bash", "-l")
	x.tr.add(501, 500, "bash", "bash", "-l")
	p, _ := x.claude(502, 501, "s1")
	p.fg()

	x.do(x.f.scan())
	fs := x.frames()
	if len(fs) != 1 || fs[0].Reason != reasonNoTerminal || fs[0].Mux != muxNone || len(fs[0].Agents) != 0 || fs[0].Selected != nil ||
		fs[0].Diag != "stagent session "+testSessionID+": holder not reachable: connect: no such file or directory" {
		t.Fatalf("frames %+v", fs)
	}
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 0 {
		t.Fatalf("unchanged rescan sent %+v", fs)
	}

	h.set(func(h *fakeHolder) { h.down = false })
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 2 || fs[0].Reason != "" || selected(fs[0]) != "claude:502" {
		t.Fatalf("frames %+v", fs)
	}

	h.exitNow()
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 1 || fs[0].Reason != reasonNoTerminal || len(fs[0].Agents) != 0 ||
		!strings.HasPrefix(fs[0].Diag, "stagent session "+testSessionID+": holder not reachable: ") {
		t.Fatalf("frames %+v", fs)
	}
}

func TestFollowSessionEnded(t *testing.T) {
	code := 3
	h := &fakeHolder{pid: 501, exit: &code}
	x := sessionFixture(t, h)
	x.tr.add(501, 1, "claude", "claude").fg()
	x.do(x.f.scan())
	if fs := x.frames(); len(fs) != 1 || fs[0].Reason != reasonNoTerminal || len(fs[0].Agents) != 0 ||
		fs[0].Diag != "stagent session "+testSessionID+": session ended (exit 3)" {
		t.Fatalf("frames %+v", fs)
	}
}

func TestParseArgs(t *testing.T) {
	for _, c := range []struct {
		args    []string
		session string
		ok      bool
	}{
		{nil, "", true},
		{[]string{"--session", testSessionID}, testSessionID, true},
		{[]string{"-session=" + testSessionID}, testSessionID, true},
		{[]string{"--session", "0123456789ABCDEF"}, "", false},
		{[]string{"--session", "0123456789abcde"}, "", false},
		{[]string{"--session", "../../stagent.so"}, "", false},
		{[]string{"--session="}, "", false},
		{[]string{"--session"}, "", false},
		{[]string{"--session", testSessionID, "extra"}, "", false},
		{[]string{"now"}, "", false},
		{[]string{"--bogus"}, "", false},
	} {
		session, ok := parseArgs(c.args, io.Discard)
		if session != c.session || ok != c.ok {
			t.Errorf("parseArgs(%q) = %q, %v; want %q, %v", c.args, session, ok, c.session, c.ok)
		}
	}
}
