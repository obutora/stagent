package follow

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/ptable"
)

// fakeProc is one process of a fake table. Unset lazy details read as
// unreadable.
type fakeProc struct {
	ptable.Proc
	argv     []string
	env      []string
	withheld bool // env reads as withheld by the OS (macOS, restricted)
	cwd      string
	exe      string
	files    []string
	owner    string // "" reads as unknown
}

// Users of the fake tables. The tests run stagent as testUser unless they
// say otherwise.
const (
	testUser = "1000"
	bobUser  = "1001"
	rootUser = "0"
)

// tree builds fake process tables.
type tree struct {
	procs map[int]*fakeProc
	base  time.Time
	user  string // owner of the processes added from now on
}

func newTree() *tree {
	t := &tree{procs: map[int]*fakeProc{}, base: time.Now().Add(-time.Hour), user: testUser}
	t.add(1, 0, "systemd", "/sbin/init")
	return t
}

// add registers a process of t.user started pid seconds after the tree's
// base time (so parents start before children), in its own process group
// with no terminal.
func (t *tree) add(pid, ppid int, name string, argv ...string) *fakeProc {
	p := &fakeProc{Proc: ptable.Proc{
		PID: pid, PPID: ppid, Name: name, Pgrp: pid,
		Start: t.base.Add(time.Duration(pid) * time.Second),
	}, argv: argv, owner: t.user}
	t.procs[pid] = p
	return p
}

// fg puts p in the foreground of its terminal.
func (p *fakeProc) fg() *fakeProc {
	p.Tpgid = p.Pgrp
	return p
}

// bg gives p a terminal whose foreground is another group.
func (p *fakeProc) bg() *fakeProc {
	p.Tpgid = p.Pgrp + 100000
	return p
}

func (p *fakeProc) withEnv(kv ...string) *fakeProc {
	p.env = append(p.env, kv...)
	return p
}

// withhold makes p's environment read as withheld (ptable.ErrEnvWithheld).
func (p *fakeProc) withhold() *fakeProc {
	p.withheld = true
	return p
}

// onTTY gives p the controlling terminal dev.
func (p *fakeProc) onTTY(dev uint64) *fakeProc {
	p.TTY = dev
	return p
}

// as makes p run as owner ("": unknown, or a set-user-ID program).
func (p *fakeProc) as(owner string) *fakeProc {
	p.owner = owner
	return p
}

func (t *tree) remove(pid int) { delete(t.procs, pid) }

func (t *tree) snapshot() *ptable.Snapshot {
	var procs []ptable.Proc
	for _, p := range t.procs {
		procs = append(procs, p.Proc)
	}
	return ptable.New(procs, fakeSource{t})
}

type fakeSource struct{ t *tree }

var errGone = errors.New("no such process")

func (f fakeSource) get(pid int) (*fakeProc, error) {
	if p := f.t.procs[pid]; p != nil {
		return p, nil
	}
	return nil, errGone
}

func (f fakeSource) Argv(pid int) ([]string, error) {
	p, err := f.get(pid)
	if err != nil {
		return nil, err
	}
	return p.argv, nil
}

func (f fakeSource) Exe(pid int) (string, error) {
	p, err := f.get(pid)
	if err != nil {
		return "", err
	}
	return p.exe, nil
}

func (f fakeSource) Env(pid int) ([]string, error) {
	p, err := f.get(pid)
	switch {
	case err == nil && p.withheld:
		return nil, ptable.ErrEnvWithheld
	case err != nil || p.env == nil:
		return nil, os.ErrPermission
	}
	return p.env, nil
}

func (f fakeSource) Cwd(pid int) (string, error) {
	p, err := f.get(pid)
	if err != nil {
		return "", err
	}
	return p.cwd, nil
}

func (f fakeSource) OpenFiles(pid int) ([]string, error) {
	p, err := f.get(pid)
	if err != nil {
		return nil, err
	}
	return p.files, nil
}

func (f fakeSource) Owner(pid int) (string, error) {
	p, err := f.get(pid)
	if err != nil || p.owner == "" {
		return "", os.ErrPermission
	}
	return p.owner, nil
}

// fakeMux answers multiplexer queries from tables; a missing entry is an
// error. calls records the queries.
type fakeMux struct {
	tmux       map[string][]tmuxClient // by strings.Join(socket, " ")
	zellij     map[string][]zellijClient
	screen     map[string]int // by sty
	herdrList  map[string][]herdrPane
	herdrInfos map[string]herdrProcessInfo // by session + " " + pane
	calls      []string
}

var errNoAnswer = errors.New("no answer")

func (m *fakeMux) tmuxClients(inv invocation, socket []string) ([]tmuxClient, error) {
	key := strings.Join(socket, " ")
	m.calls = append(m.calls, "tmux "+key)
	c, ok := m.tmux[key]
	if !ok {
		return nil, errNoAnswer
	}
	return c, nil
}

func (m *fakeMux) zellijClients(inv invocation, session string) ([]zellijClient, error) {
	m.calls = append(m.calls, "zellij "+session)
	c, ok := m.zellij[session]
	if !ok {
		return nil, errNoAnswer
	}
	return c, nil
}

func (m *fakeMux) screenWindow(inv invocation, sty string) (int, error) {
	m.calls = append(m.calls, "screen "+sty)
	n, ok := m.screen[sty]
	if !ok {
		return 0, errNoAnswer
	}
	return n, nil
}

func (m *fakeMux) herdrPanes(inv invocation, session string) ([]herdrPane, error) {
	m.calls = append(m.calls, "herdr panes "+session)
	p, ok := m.herdrList[session]
	if !ok {
		return nil, errNoAnswer
	}
	return p, nil
}

func (m *fakeMux) herdrProcessInfo(inv invocation, session, pane string) (herdrProcessInfo, error) {
	m.calls = append(m.calls, "herdr info "+session+" "+pane)
	i, ok := m.herdrInfos[session+" "+pane]
	if !ok {
		return herdrProcessInfo{}, errNoAnswer
	}
	return i, nil
}

// selfPID is stagent's pid in the fake tables.
const selfPID = 110

// sshTree is a connection whose terminal shell is 101 (pgrp 101) and whose
// exec channel runs stagent (selfPID); an unrelated connection runs
// another agent.
func sshTree() *tree {
	t := newTree()
	t.add(90, 1, "sshd", "sshd: /usr/sbin/sshd -D [listener]")
	t.add(100, 90, "sshd-session", "sshd-session: user@pts/0,notty")
	t.add(101, 100, "bash", "-bash").fg()
	t.add(selfPID, 100, "stagent", "/home/u/.ssh-term/agent/bin/stagent", "follow")
	t.add(95, 90, "sshd-session", "sshd-session: user@pts/1")
	t.add(96, 95, "bash", "-bash")
	t.add(97, 96, "claude", "claude").fg()
	return t
}

func testLocator(m muxSystem) *locator {
	return &locator{
		self: selfPID, owner: testUser, sys: m, conns: map[image]connClass{},
		ttyHosts: func() map[uint64]string { return nil },
		ttyName:  func(dev uint64) string { return fmt.Sprintf("ttys%03d", dev) },
	}
}
