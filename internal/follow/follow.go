// Package follow is `stagent follow`: it finds the coding agent running in
// the terminal that shares its SSH connection — in the terminal's shell, or
// in the tmux / zellij / screen / herdr pane that terminal shows — and
// streams the agent's transcript to the app as JSON lines on stdout until
// stdin closes. PROTOCOL.md ("stagent follow") describes the wire format.
package follow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// scanInterval is how often the process table is walked again.
	scanInterval = time.Second
	// tailInterval is how often the followed transcript is polled for
	// appended records.
	tailInterval = 300 * time.Millisecond
	// pageSize is the size of the first page and of each older page.
	pageSize = 60
)

// Frame names (the "t" member).
const (
	frameHello    = "hello"
	frameTarget   = "target"
	frameMessages = "messages"
	framePage     = "page"
	frameError    = "error"
)

type helloFrame struct {
	T              string `json:"t"`
	Version        string `json:"version"`
	FollowProtocol int    `json:"follow_protocol"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
}

type targetFrame struct {
	T         string      `json:"t"`
	Mux       string      `json:"mux"`
	MuxDetail string      `json:"mux_detail,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Diag      string      `json:"diag,omitempty"`
	Agents    []agentInfo `json:"agents"`
	Selected  *string     `json:"selected"`
}

type agentInfo struct {
	Key            string `json:"key"`
	Harness        string `json:"harness"`
	PID            int    `json:"pid"`
	Cwd            string `json:"cwd"`
	Pane           string `json:"pane,omitempty"`
	TTY            string `json:"tty,omitempty"`
	Title          string `json:"title,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	Foreground     bool   `json:"foreground"`
}

type messagesFrame struct {
	T        string         `json:"t"`
	Key      string         `json:"key"`
	Path     string         `json:"path"`
	Harness  string         `json:"harness"`
	Reset    bool           `json:"reset"`
	Messages []wire.Message `json:"messages"`
	Cursor   int64          `json:"cursor"`
}

type pageFrame struct {
	T        string         `json:"t"`
	Key      string         `json:"key"`
	Messages []wire.Message `json:"messages"`
	Cursor   int64          `json:"cursor"`
}

type errorFrame struct {
	T       string `json:"t"`
	Message string `json:"message"`
}

// request is one line of stdin.
type request struct {
	Op     string  `json:"op"`
	Key    *string `json:"key"`
	Before int64   `json:"before"`
}

// Main runs `stagent follow` until stdin closes.
func Main(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: stagent follow  (streams the agent in this SSH connection's terminal on stdout)")
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent follow:", err)
		return 1
	}
	owner, err := ptable.CurrentOwner()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent follow:", err)
		return 1
	}
	f := newFollower(os.Stdout, home, ptable.Take, newLocator(execMux{}, owner), os.Getenv)
	if err := f.run(os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "stagent follow:", err)
		return 1
	}
	return 0
}

// agent is one agent of the current target.
type agent struct {
	info    agentInfo
	id      instance
	mtime   time.Time // of its transcript
	session int64     // start of its terminal session (see candidate)
}

// selection is a picked agent.
type selection struct {
	key       string
	id        instance
	harness   string
	pane, tty string
}

// followed is the transcript being streamed.
type followed struct {
	key, path, harness string
	offset             int64 // tail position
	cursor             int64 // start of the oldest page sent
}

// follower holds the state of one `stagent follow` run. Every method runs
// on the goroutine of run.
type follower struct {
	out    *wire.Codec
	home   string
	take   func() (*ptable.Snapshot, error)
	loc    *locator
	getenv func(string) string // stagent's own environment
	tr     *transcripts

	last   location // of the last scan
	agents []agent
	auto   *selection // picked automatically
	sticky *selection // picked by the app; holds until its process exits
	sent   []byte     // last target frame written
	errs   []string   // problems of the last scan, reported once
	cwds   map[instance]string
	cur    followed
}

func newFollower(w io.Writer, home string, take func() (*ptable.Snapshot, error), loc *locator, getenv func(string) string) *follower {
	return &follower{
		out:    wire.NewCodec(nil, w),
		home:   home,
		take:   take,
		loc:    loc,
		getenv: getenv,
		tr:     newTranscripts(),
		cwds:   map[instance]string{},
	}
}

// run serves until in reaches EOF. It returns an error only when stdout
// fails.
func (f *follower) run(in io.Reader) error {
	if err := f.out.Encode(helloFrame{
		T:              frameHello,
		Version:        version.Version,
		FollowProtocol: version.FollowProtocol,
		OS:             runtime.GOOS,
		Arch:           runtime.GOARCH,
	}); err != nil {
		return err
	}
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		c := wire.NewCodec(in, nil)
		for {
			line, err := c.ReadLine()
			if err != nil {
				return
			}
			lines <- line
		}
	}()
	if err := f.scan(); err != nil {
		return err
	}
	scan := time.NewTicker(scanInterval)
	defer scan.Stop()
	tail := time.NewTicker(tailInterval)
	defer tail.Stop()
	for {
		var err error
		select {
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			err = f.handle(line)
		case <-scan.C:
			err = f.scan()
		case <-tail.C:
			err = f.tail()
		}
		if err != nil {
			return err
		}
	}
}

// scan locates the agents again and publishes what changed.
func (f *follower) scan() error {
	s, err := f.take()
	if err != nil {
		return f.report([]string{"process table: " + err.Error()})
	}
	f.last = f.loc.locate(s)
	f.agents = f.describe(s)
	if err := f.report(f.last.errs); err != nil {
		return err
	}
	return f.publish()
}

// report writes the problems that were not already reported by the
// previous scan.
func (f *follower) report(errs []string) error {
	for _, e := range errs {
		if !slices.Contains(f.errs, e) {
			if err := f.sendError(e); err != nil {
				return err
			}
		}
	}
	f.errs = errs
	return nil
}

// describe turns the located candidates into agents. An agent the app
// picked stays listed while its process lives (and runs as stagent's user),
// even when the terminal no longer shows it.
func (f *follower) describe(s *ptable.Snapshot) []agent {
	cwds := map[instance]string{}
	owns := func(pid int) bool { return f.loc.owns(s, pid) }
	cl := f.tr.newClaims(s, owns, func(pid int) transcript.Roots { return f.roots(s, pid) })
	out := make([]agent, 0, len(f.last.agents))
	for _, c := range f.last.agents {
		out = append(out, f.agent(s, c, cwds, cl))
	}
	if st := f.sticky; st != nil && find(out, st.key) == nil {
		if p := s.Get(st.id.pid); p != nil && p.Start.UnixNano() == st.id.start && owns(p.PID) && harnessOf(p.Name, s.Argv(p.PID)) == st.harness {
			c := candidate{harness: st.harness, pane: st.pane, tty: st.tty}
			c.pids = sameHarness(s, p.PID, st.harness, owns, nil)
			out = append(out, f.agent(s, c, cwds, cl))
		}
	}
	f.cwds = cwds
	f.tr.endScan()
	return out
}

// roots are the transcript directories as the environment of pid
// configures them (stagent's own when unreadable).
func (f *follower) roots(s *ptable.Snapshot, pid int) transcript.Roots {
	getenv := f.getenv
	if env := s.Env(pid); env != nil {
		getenv = func(k string) string { return ptable.Lookup(env, k) }
	}
	return transcript.DefaultRoots(f.home, getenv)
}

// sameHarness returns pid and the processes of the same harness below it
// that run as stagent's user.
func sameHarness(s *ptable.Snapshot, pid int, harness string, owns func(int) bool, acc []int) []int {
	acc = append(acc, pid)
	for _, c := range s.Children(pid) {
		if owns(c) && harnessOf(s.Get(c).Name, s.Argv(c)) == harness {
			acc = sameHarness(s, c, harness, owns, acc)
		}
	}
	return acc
}

func (f *follower) agent(s *ptable.Snapshot, c candidate, cwds map[instance]string, cl *claims) agent {
	pid := c.pids[0]
	p := s.Get(pid)
	id := instance{pid, p.Start.UnixNano()}
	roots := f.roots(s, pid)
	// Agents do not change directory; macOS pays an lsof per lookup.
	cwd, ok := f.cwds[id]
	if !ok {
		cwd = s.Cwd(pid)
	}
	path, sessionCwd := f.tr.resolve(agentFacts{
		harness:   c.harness,
		pids:      c.pids,
		start:     p.Start,
		cwd:       cwd,
		roots:     roots,
		argv:      s.Argv,
		openFiles: s.OpenFiles,
		claimed:   cl.claimedBy(c.pids),
	})
	cl.assign(path)
	if cwd == "" {
		cwd = sessionCwd
	}
	if cwd != "" {
		cwds[id] = cwd
	}
	a := agent{id: id, session: c.session, info: agentInfo{
		Key:            c.harness + ":" + strconv.Itoa(pid),
		Harness:        c.harness,
		PID:            pid,
		Cwd:            cwd,
		Pane:           c.pane,
		TTY:            c.tty,
		TranscriptPath: path,
		Foreground:     c.foreground,
	}}
	if path != "" {
		a.info.Title, a.mtime = f.tr.meta(c.harness, roots, path)
	}
	return a
}

func find(agents []agent, key string) *agent {
	for i := range agents {
		if agents[i].info.Key == key {
			return &agents[i]
		}
	}
	return nil
}

// choose returns the agent to follow: the app's pick while its process
// lives, else the automatic one. The automatic pick only moves when its
// agent is gone or another agent is in the foreground while it is not, so
// two agents writing in turn do not make the view flip.
func (f *follower) choose() *agent {
	if st := f.sticky; st != nil {
		if a := find(f.agents, st.key); a != nil && a.id == st.id {
			return a
		}
		f.sticky = nil
	}
	fg := false
	for _, a := range f.agents {
		fg = fg || a.info.Foreground
	}
	if au := f.auto; au != nil {
		if a := find(f.agents, au.key); a != nil && a.id == au.id && (a.info.Foreground || !fg) {
			return a
		}
	}
	var best *agent
	for i := range f.agents {
		if a := &f.agents[i]; best == nil || better(a, best) {
			best = a
		}
	}
	f.auto = nil
	if best != nil {
		f.auto = &selection{key: best.info.Key, id: best.id}
	}
	return best
}

// better ranks agents for the automatic pick: of the newest terminal
// session when several may be the tab's, then in the foreground (of the
// shown pane), then newest transcript, then newest process.
func better(a, b *agent) bool {
	if a.session != b.session {
		return a.session > b.session
	}
	if a.info.Foreground != b.info.Foreground {
		return a.info.Foreground
	}
	if !a.mtime.Equal(b.mtime) {
		return a.mtime.After(b.mtime)
	}
	return a.id.start > b.id.start
}

// publish writes the target frame if it changed and starts following the
// chosen agent's transcript. A change of the diag alone — its counts move
// with every scan — does not count.
func (f *follower) publish() error {
	sel := f.choose()
	frame := targetFrame{
		T:         frameTarget,
		Mux:       f.last.mux,
		MuxDetail: f.last.muxDetail,
		Agents:    make([]agentInfo, 0, len(f.agents)),
	}
	for _, a := range f.agents {
		frame.Agents = append(frame.Agents, a.info)
	}
	switch {
	case f.last.noTerminal:
		frame.Reason = reasonNoTerminal
	case len(f.agents) == 0:
		frame.Reason = reasonNoAgent
	case f.last.terminalAmbiguous:
		frame.Reason = reasonTerminalAmbiguous
	case f.last.ambiguous:
		frame.Reason = reasonAmbiguous
	}
	if sel != nil {
		frame.Selected = &sel.info.Key
	}
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, f.sent) {
		f.sent = b
		if frame.Diag = f.last.diag; frame.Diag != "" {
			if b, err = json.Marshal(frame); err != nil {
				return err
			}
		}
		if err := f.out.Encode(json.RawMessage(b)); err != nil {
			return err
		}
	}
	return f.follow(sel)
}

// follow switches to a's transcript, sending its newest page, when the
// selected agent or its transcript changed.
func (f *follower) follow(a *agent) error {
	var next followed
	if a != nil {
		next = followed{key: a.info.Key, path: a.info.TranscriptPath, harness: a.info.Harness}
	}
	if next.key == f.cur.key && next.path == f.cur.path {
		return nil
	}
	f.cur = next
	if next.path == "" {
		return nil
	}
	page, err := transcript.ReadPage(next.path, next.harness, 0, pageSize)
	if err != nil {
		f.cur.path = "" // the next scan tries again
		return f.sendError("read transcript: " + err.Error())
	}
	f.cur.offset, f.cur.cursor = page.End, page.Cursor
	return f.out.Encode(messagesFrame{
		T:        frameMessages,
		Key:      next.key,
		Path:     next.path,
		Harness:  next.harness,
		Reset:    true,
		Messages: nonNil(page.Messages),
		Cursor:   page.Cursor,
	})
}

// tail sends the records appended to the followed transcript.
func (f *follower) tail() error {
	if f.cur.path == "" {
		return nil
	}
	msgs, off, err := transcript.ReadFrom(f.cur.path, f.cur.harness, f.cur.offset)
	if err != nil {
		return nil // gone or unreadable: the next scan resolves the transcript again
	}
	f.cur.offset = off
	if len(msgs) == 0 {
		return nil
	}
	return f.out.Encode(messagesFrame{
		T:        frameMessages,
		Key:      f.cur.key,
		Path:     f.cur.path,
		Harness:  f.cur.harness,
		Messages: msgs,
		Cursor:   f.cur.cursor,
	})
}

func (f *follower) handle(line []byte) error {
	var r request
	if err := json.Unmarshal(line, &r); err != nil {
		return f.sendError("bad request: " + err.Error())
	}
	switch r.Op {
	case "select":
		return f.selectAgent(r.Key)
	case "older":
		return f.older(r.Before)
	}
	return f.sendError(fmt.Sprintf("unknown op %q", r.Op))
}

// selectAgent pins the agent key; an empty or null key returns to the
// automatic pick.
func (f *follower) selectAgent(key *string) error {
	if key == nil || *key == "" {
		f.sticky, f.auto = nil, nil
		return f.publish()
	}
	a := find(f.agents, *key)
	if a == nil {
		return f.sendError("unknown agent " + strconv.Quote(*key))
	}
	f.sticky = &selection{key: a.info.Key, id: a.id, harness: a.info.Harness, pane: a.info.Pane, tty: a.info.TTY}
	return f.publish()
}

// older answers with the page of messages that end at byte offset before.
func (f *follower) older(before int64) error {
	if f.cur.path == "" {
		return f.sendError("no transcript is being followed")
	}
	reply := pageFrame{T: framePage, Key: f.cur.key, Messages: []wire.Message{}}
	if before > 0 {
		page, err := transcript.ReadPage(f.cur.path, f.cur.harness, before, pageSize)
		if err != nil {
			return f.sendError("read transcript: " + err.Error())
		}
		reply.Messages, reply.Cursor = nonNil(page.Messages), page.Cursor
		f.cur.cursor = min(f.cur.cursor, page.Cursor)
	}
	return f.out.Encode(reply)
}

func (f *follower) sendError(msg string) error {
	return f.out.Encode(errorFrame{T: frameError, Message: msg})
}

func nonNil(m []wire.Message) []wire.Message {
	if m == nil {
		return []wire.Message{}
	}
	return m
}
