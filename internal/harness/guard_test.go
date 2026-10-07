package harness

import (
	"errors"
	"net"
	"os"
	"runtime"
	"testing"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// fakeProc is one process of a fake table; argv nil reads as unreadable.
type fakeProc struct {
	ppid int
	name string
	argv []string
}

type fakeTable map[int]fakeProc

func (f fakeTable) table() Table {
	return Table{
		Process: func(pid int) (ptable.Proc, bool) {
			p, ok := f[pid]
			return ptable.Proc{PID: pid, PPID: p.ppid, Name: p.name}, ok
		},
		Argv: func(pid int) ([]string, error) {
			if p, ok := f[pid]; ok && p.argv != nil {
				return p.argv, nil
			}
			return nil, os.ErrPermission
		},
	}
}

// host is a machine with a person's ssh session and a Claude Code session
// started from a terminal.
func host() fakeTable {
	return fakeTable{
		1:   {0, "launchd", nil},
		10:  {1, "sshd", nil},
		11:  {10, "sshd-session", nil},
		12:  {11, "stagent", []string{"stagent", "bridge"}},
		20:  {1, "stagent", []string{"stagent", "run", "--", "/bin/zsh", "-l"}},
		21:  {20, "zsh", []string{"/bin/zsh", "-l"}},
		22:  {21, "stagent", []string{"stagent", "attach", "0123456789abcdef"}},
		30:  {21, "node", []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}},
		31:  {30, "bash", []string{"/bin/bash", "-c", "make"}},
		32:  {31, "make", []string{"make"}},
		33:  {32, "stagent", []string{"stagent", "attach", "0123456789abcdef"}},
		40:  {21, "omp", []string{"bun", "/home/u/.bun/bin/omp"}},
		41:  {40, "stagent", []string{"stagent", "hook", "omp"}},
		50:  {1, "codex", nil}, // another user's, or hidden: argv unreadable
		51:  {50, "stagent", []string{"stagent", "ls"}},
		60:  {99, "stagent", []string{"stagent", "attach"}}, // parent 99 unreadable
		600: {0, "kernel", nil},
	}
}

func TestTableAgent(t *testing.T) {
	for _, c := range []struct {
		name string
		pid  int
		want string
		err  error
	}{
		{"bridge under sshd", 12, "", nil},
		{"attach in a kept shell", 22, "", nil},
		{"claude descendant", 33, wire.HarnessClaude, nil},
		{"claude itself", 30, wire.HarnessClaude, nil},
		{"omp child", 41, wire.HarnessOmp, nil},
		{"harness known by name only", 51, wire.HarnessCodex, nil},
		{"hidden ancestor ends the walk", 60, "", nil},
		{"own parent", 600, "", nil},
		{"gone", 77, "", errGone},
	} {
		h, err := host().table().Agent(c.pid)
		if h != c.want || err != c.err {
			t.Errorf("%s: Agent(%d) = %q, %v; want %q, %v", c.name, c.pid, h, err, c.want, c.err)
		}
	}
}

// changing is f's table with the processes of over read through them:
// the n-th read of pid returns over[pid][n] (the last one from then on,
// false = not there).
func changing(f fakeTable, over map[int][]*ptable.Proc) Table {
	tab := f.table()
	process := tab.Process
	reads := map[int]int{}
	tab.Process = func(pid int) (ptable.Proc, bool) {
		seq, ok := over[pid]
		if !ok {
			return process(pid)
		}
		n := min(reads[pid], len(seq)-1)
		reads[pid]++
		if seq[n] == nil {
			return ptable.Proc{}, false
		}
		return *seq[n], true
	}
	return tab
}

// sandbox: claude runs bwrap, whose pid 1 (a subreaper) runs sh, which
// runs stagent (pid 9).
func sandbox() fakeTable {
	return fakeTable{
		1: {0, "init", nil},
		5: {1, "systemd", nil},
		6: {5, "claude", nil},
		7: {6, "bwrap", nil},
		8: {7, "sh", nil},
		9: {8, "stagent", nil},
	}
}

// TestTableAgentRewalks: an ancestor that exits during the walk does not
// end it; the walk starts again along the new parents.
func TestTableAgentRewalks(t *testing.T) {
	stagent := func(ppid int) *ptable.Proc { return &ptable.Proc{PID: 9, PPID: ppid, Name: "stagent"} }
	sh := func(ppid int) *ptable.Proc { return &ptable.Proc{PID: 8, PPID: ppid, Name: "sh"} }
	// flapping: each read of the peer names the other of two parents,
	// neither of which can be read.
	var flapping []*ptable.Proc
	for i := range 2 * maxWalks {
		flapping = append(flapping, stagent(8-i%2))
	}
	for _, c := range []struct {
		name string
		over map[int][]*ptable.Proc
		want string
		err  error
	}{
		{"parent exited, peer reparented to the subreaper",
			map[int][]*ptable.Proc{8: {nil}, 9: {stagent(8), stagent(7)}}, wire.HarnessClaude, nil},
		{"grandparent exited, parent reparented",
			map[int][]*ptable.Proc{7: {nil}, 8: {sh(7), sh(6)}}, wire.HarnessClaude, nil},
		{"peer exited with its parent",
			map[int][]*ptable.Proc{8: {nil}, 9: {stagent(8), nil}}, "", errGone},
		{"parents keep changing",
			map[int][]*ptable.Proc{7: {nil}, 8: {nil}, 9: flapping}, "", errUnstable},
	} {
		h, err := changing(sandbox(), c.over).Agent(9)
		if h != c.want || err != c.err {
			t.Errorf("%s: Agent = %q, %v; want %q, %v", c.name, h, err, c.want, c.err)
		}
	}
}

// An agent known by its argv only (node running claude's cli.js) that exits
// between the reads of its process and its argv is not taken for a plain
// node: the peer is gone, and refused as such.
func TestTableAgentArgvGone(t *testing.T) {
	f := fakeTable{1: {0, "init", nil}, 21: {1, "zsh", nil}, 30: {21, "node", nil}}
	node := &ptable.Proc{PID: 30, PPID: 21, Name: "node"}
	h, err := changing(f, map[int][]*ptable.Proc{30: {node, nil}}).Agent(30)
	if h != "" || err != errGone {
		t.Fatalf("Agent = %q, %v; want %v", h, err, errGone)
	}
}

// TestTableAgentTooDeep: an ancestry deeper than the walk goes is refused,
// so nesting cannot hide an agent above it.
func TestTableAgentTooDeep(t *testing.T) {
	f := fakeTable{1: {0, "init", nil}, 2: {1, "claude", nil}}
	pid := 2
	for i := range maxAncestry + 10 {
		f[100+i] = fakeProc{pid, "sh", nil}
		pid = 100 + i
	}
	if h, err := f.table().Agent(pid); err != errTooDeep {
		t.Fatalf("Agent = %q, %v; want %v", h, err, errTooDeep)
	}
	g := &Guard{PeerPID: func(net.Conn) (int, error) { return pid, nil }, Table: f.table()}
	if err := g.Check(nil); err == nil || err.Code != wire.ErrAgentRefused {
		t.Fatalf("Check = %v; want %s", err, wire.ErrAgentRefused)
	}
}

func TestGuardCheck(t *testing.T) {
	conn := func(pid int, err error) func(net.Conn) (int, error) {
		return func(net.Conn) (int, error) { return pid, err }
	}
	for _, c := range []struct {
		name    string
		peer    func(net.Conn) (int, error)
		refused bool
	}{
		{"sshd descendant", conn(12, nil), false},
		{"claude descendant", conn(33, nil), true},
		{"peer gone", conn(77, nil), true},
		{"pid unknown", conn(0, nil), true},
		{"peer credentials unreadable", conn(-1, errors.New("boom")), true},
	} {
		g := &Guard{PeerPID: c.peer, Table: host().table()}
		err := g.Check(nil)
		if (err != nil) != c.refused {
			t.Errorf("%s: Check = %v, refused want %v", c.name, err, c.refused)
		}
		if err != nil && err.Code != wire.ErrAgentRefused {
			t.Errorf("%s: code %q, want %q", c.name, err.Code, wire.ErrAgentRefused)
		}
	}
}

func TestNewGuardSkipsIsolatedLayouts(t *testing.T) {
	if g := NewGuard(&paths.Layout{Isolated: true}); g != nil {
		t.Fatal("an isolated layout is guarded")
	}
	var none *Guard
	if err := none.Check(nil); err != nil {
		t.Fatalf("nil guard refused: %v", err)
	}
	g := NewGuard(&paths.Layout{})
	if (g == nil) != (runtime.GOOS == "windows") {
		t.Fatalf("NewGuard(real layout) = %v on %s", g, runtime.GOOS)
	}
}
