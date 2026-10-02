package follow

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/wire"
)

// Multiplexer kinds of the target frame.
const (
	muxNone   = "none"
	muxTmux   = "tmux"
	muxZellij = "zellij"
	muxScreen = "screen"
	muxHerdr  = "herdr"
)

// Reasons of the target frame.
const (
	reasonNoTerminal        = "no_terminal"
	reasonNoAgent           = "no_agent"
	reasonTerminalAmbiguous = "terminal_ambiguous"
	reasonAmbiguous         = "mux_ambiguous"
)

const (
	// maxMuxDepth bounds multiplexers nested in each other's panes.
	maxMuxDepth = 3
	// maxAncestry bounds walks up the process tree.
	maxAncestry = 256
)

// candidate is one agent process found in the terminal.
type candidate struct {
	harness string
	// pids holds the agent process first, then the processes of the same
	// harness below it (Codex's native binary under its node launcher, omp
	// workers): together they are one agent.
	pids       []int
	pane       string
	foreground bool
	// tty names the terminal of the agent's session and session is when
	// that session started (unix ns), when the terminal is ambiguous.
	tty     string
	session int64
}

// location is what locate found in one process-table snapshot.
type location struct {
	noTerminal     bool
	diag           string // why, when noTerminal (see diagnose)
	mux, muxDetail string
	ambiguous      bool // the multiplexer's clients
	// terminalAmbiguous: several terminals may be the tab's, and agents
	// holds those of all of them.
	terminalAmbiguous bool
	agents            []candidate
	errs              []string // failed multiplexer queries
}

// locator finds the agents running in the terminal that shares stagent's
// SSH connection, or in a stagent session. Its decisions depend only on the
// snapshot, muxSystem, ttyHosts and session.
type locator struct {
	self      int
	sshConn   string // stagent's own SSH_CONNECTION ("" when unset)
	sshClient string // stagent's own SSH_CLIENT
	// owner is the user stagent runs as (ptable.Snapshot.Owner form); see
	// owns.
	owner string
	sys   muxSystem
	// ttyHosts and ttyName are ptable.TTYHosts and ptable.TTYName.
	ttyHosts func() map[uint64]string
	ttyName  func(dev uint64) string
	// conns caches what the environment of each program a process ran
	// says about its SSH connection: reading every environment is the
	// costly part of the fallback, and the environment a program started
	// with is fixed.
	conns map[image]connClass
	// session, set by `stagent follow --session`, replaces the search for
	// the terminal: it returns the one root, the session's program (see
	// holderSession).
	session func() (int, error)
}

// instance identifies a process across snapshots (pids are recycled).
type instance struct {
	pid   int
	start int64
}

// image identifies the program a process instance runs. exec keeps the pid
// and start time but brings another environment, and often another owner:
// the child of root's `login` becomes the user's shell.
type image struct {
	instance
	name string
}

// connClass is what a process's environment says about its SSH connection.
type connClass uint8

const (
	connUnreadable connClass = iota
	connWithheld             // see ptable.ErrEnvWithheld
	connNone                 // no SSH_CONNECTION
	connOther                // another connection's
	connSame                 // stagent's
)

func newLocator(sys muxSystem, owner string) *locator {
	return &locator{
		self:      os.Getpid(),
		sshConn:   os.Getenv("SSH_CONNECTION"),
		sshClient: os.Getenv("SSH_CLIENT"),
		owner:     owner,
		sys:       sys,
		ttyHosts:  ptable.TTYHosts,
		ttyName:   ptable.TTYName,
		conns:     map[image]connClass{},
	}
}

// owns reports whether pid runs as stagent's own user. Only such processes
// are used: as the terminal's processes in the SSH_CONNECTION fallback and
// below tailscaled, as agents, as multiplexer clients and servers, and as
// the source of transcript locations. Another user's process is untrusted
// input: stagent runs a multiplexer client's executable with the client's
// environment, which — when stagent runs as root (Tailscale SSH as root,
// `su bob` in root's terminal) — would run that user's code as root, and
// anyone can copy stagent's SSH_CONNECTION into their environment.
// Processes of an unknown owner, or whose real and effective uids differ (a
// set-user-ID program, see ptable.Snapshot.Owner), count as another user's.
// The owner is checked on every scan: a process keeps its pid and start
// time across su's setuid and exec.
func (l *locator) owns(s *ptable.Snapshot, pid int) bool {
	return l.owner != "" && s.Owner(pid) == l.owner
}

// locate finds the agents in the terminal: in its shell's process tree, and
// in the pane a multiplexer client in that tree shows. When several
// terminals may be the tab's, their agents are told apart by terminal.
func (l *locator) locate(s *ptable.Snapshot) location {
	roots, sr := l.scope(s)
	if len(roots) == 0 {
		return location{noTerminal: true, diag: l.diagnose(s, sr), mux: muxNone}
	}
	w := &walker{l: l, s: s, seen: map[int]bool{}, tmux: map[string]tmuxAnswer{}}
	if len(roots) > 1 && sr.sessions {
		w.loc.terminalAmbiguous = true
		for _, r := range roots {
			p := s.Get(r)
			w.explore([]int{r}, level{fg: (*ptable.Proc).Foreground, tty: l.ttyName(p.TTY), session: p.Start.UnixNano()}, 0)
		}
	} else {
		w.explore(roots, level{fg: (*ptable.Proc).Foreground}, 0)
	}
	loc := w.loc
	if loc.mux == "" {
		loc.mux = muxNone
	}
	for _, c := range w.agents {
		loc.agents = append(loc.agents, *c)
	}
	return loc
}

// search records how scope looked for the terminal.
type search struct {
	root int      // the connection root, 0 when there is none
	ts   *tsStats // the Tailscale session search's, nil when it did not run
	// sessions: the roots are terminals the Tailscale session search
	// could not tell apart, each one the tab's or not.
	sessions bool
	conn     *connStats // the SSH_CONNECTION fallback's, nil when it did not run
	// sessionErr: why the session of `--session` has no root.
	sessionErr error
}

// connStats counts what the SSH_CONNECTION fallback saw.
type connStats struct {
	procs    int // processes other than stagent
	env      int // with a readable environment
	withheld int // whose environment the OS withholds
	set      int // with SSH_CONNECTION
	same     int // with stagent's SSH_CONNECTION; of these, not used:
	self     int // stagent's ancestors and descendants
	owner    int // another user's (or of an unknown owner)
	nested   int // below another match or a multiplexer
}

func (st *connStats) count(c connClass) {
	st.procs++
	switch c {
	case connWithheld:
		st.withheld++
	case connSame:
		st.same++
		fallthrough
	case connOther:
		st.set++
		fallthrough
	case connNone:
		st.env++
	}
}

// scope returns the roots of the process trees belonging to the terminal:
// with `--session`, the session's program alone.
//
// Every channel of an SSH connection — the terminal's shell and this exec
// channel — is a child of the connection's SSH server process, so the
// terminal is that process's other children. Without such an ancestor,
// below tailscaled (Tailscale SSH serves connections from the daemon
// itself), the terminal is found among the user's sessions (see
// tailscaleSessions). Else, or when those show no terminal session at all,
// the user's processes carrying stagent's SSH_CONNECTION stand in for it;
// without SSH_CONNECTION there is no terminal to find, rather than a guess.
func (l *locator) scope(s *ptable.Snapshot) ([]int, search) {
	if l.session != nil {
		pid, err := l.session()
		if err != nil {
			return nil, search{sessionErr: err}
		}
		return []int{pid}, search{}
	}
	if root := connectionRoot(s, l.self); root != 0 {
		var roots []int
		for _, c := range s.Children(root) {
			if !descends(s, l.self, c) {
				roots = append(roots, c)
			}
		}
		return roots, search{root: root}
	}
	var sr search
	if daemon, own := tailscaleRoot(s, l.self); daemon != 0 {
		sr.ts = &tsStats{daemon: daemon}
		if roots := l.tailscaleSessions(s, daemon, own, sr.ts); sr.ts.cands > 0 {
			sr.sessions = true
			return roots, sr
		}
	}
	if l.sshConn == "" {
		return nil, sr
	}
	sr.conn = &connStats{}
	return l.sameConnection(s, sr.conn), sr
}

// connectionRoot is the nearest ancestor of pid that is a per-connection
// SSH server process, 0 when there is none.
func connectionRoot(s *ptable.Snapshot, pid int) int {
	p := s.Get(pid)
	for range maxAncestry {
		if p == nil || p.PPID == p.PID {
			return 0
		}
		if p = s.Get(p.PPID); p == nil {
			return 0
		}
		switch baseName(p.Name) {
		case "sshd", "sshd-session", "dropbear":
			return p.PID
		}
	}
	return 0
}

// sameConnection returns the topmost of stagent's user's processes whose
// environment has stagent's SSH_CONNECTION, except stagent's own ancestors
// and descendants and processes in a multiplexer's panes (reached through
// its client), and counts what it saw in st.
func (l *locator) sameConnection(s *ptable.Snapshot, st *connStats) []int {
	cache := make(map[image]connClass, len(l.conns))
	match := map[int]bool{}
	for _, pid := range s.PIDs() {
		if pid == l.self {
			continue
		}
		p := s.Get(pid)
		id := image{instance{pid, p.Start.UnixNano()}, p.Name}
		c, ok := l.conns[id]
		if !ok {
			c = l.connOf(s, pid)
		}
		cache[id] = c
		st.count(c)
		if c != connSame {
			continue
		}
		switch {
		case descends(s, pid, l.self) || descends(s, l.self, pid):
			st.self++
		// Checked after the (cached) environment: the owner is only read
		// for the few processes that match.
		case !l.owns(s, pid):
			st.owner++
		default:
			match[pid] = true
		}
	}
	l.conns = cache
	var roots []int
	for _, pid := range s.PIDs() {
		switch {
		case !match[pid]:
		case l.nested(s, pid, match):
			st.nested++
		default:
			roots = append(roots, pid)
		}
	}
	return roots
}

// connOf reads what the environment of pid says about its SSH connection.
func (l *locator) connOf(s *ptable.Snapshot, pid int) connClass {
	env := s.Env(pid)
	switch v := ptable.Lookup(env, "SSH_CONNECTION"); {
	case env == nil && s.EnvWithheld(pid):
		return connWithheld
	case env == nil:
		return connUnreadable
	case v == "":
		return connNone
	case v == l.sshConn:
		return connSame
	default:
		return connOther
	}
}

// nested reports whether an ancestor of pid is in match or is a
// multiplexer.
func (l *locator) nested(s *ptable.Snapshot, pid int, match map[int]bool) bool {
	p := s.Get(pid)
	for range maxAncestry {
		if p == nil || p.PPID == p.PID {
			return false
		}
		if p = s.Get(p.PPID); p == nil {
			return false
		}
		if match[p.PID] || muxKind(p.Name, s.Argv(p.PID)) != "" {
			return true
		}
	}
	return false
}

// descends reports whether pid is anc or one of its descendants.
func descends(s *ptable.Snapshot, pid, anc int) bool {
	p := s.Get(pid)
	for range maxAncestry {
		if p == nil {
			return false
		}
		if p.PID == anc {
			return true
		}
		if p.PPID == p.PID {
			return false
		}
		p = s.Get(p.PPID)
	}
	return false
}

// maxDiagChain bounds the ancestors a diag lists.
const maxDiagChain = 8

// diagnose explains a no_terminal result in one line for bug reports. With
// `--session` that is why the session has no program to search. Else it is
// stagent's pid and user, whether its SSH_CONNECTION is set, the connection
// root it found (via=root:<name>:<pid>, with that process's child count),
// else what the Tailscale session search (via=tailscale:<daemon pid>) and
// the SSH_CONNECTION fallback (via= or then=ssh_conn) counted, else
// via=none, then its ancestors as the snapshot has them. It names
// processes and users, never environment values or addresses.
func (l *locator) diagnose(s *ptable.Snapshot, sr search) string {
	if sr.sessionErr != nil {
		return sr.sessionErr.Error()
	}
	var b strings.Builder
	conn := "unset"
	if l.sshConn != "" {
		conn = "set"
	}
	fmt.Fprintf(&b, "pid=%d uid=%s ssh_conn=%s", l.self, diagOwner(l.owner), conn)
	if sr.root != 0 {
		fmt.Fprintf(&b, " via=root:%s:%d children=%d", diagName(s.Get(sr.root).Name), sr.root, len(s.Children(sr.root)))
	}
	if t := sr.ts; t != nil {
		fmt.Fprintf(&b, " via=tailscale:%d sessions=%d foreign=%d notty=%d muxed=%d cands=%d our_conn=%s our_ip=%s withheld=%d ip_utmp=%d exact=%d same_ip=%d other=%d unknown=%d",
			t.daemon, t.sessions, t.foreign, t.noTTY, t.muxed, t.cands, diagSource(t.ourConn), diagSource(t.ourIP),
			t.withheld, t.ipUtmp, t.exact, t.sameIP, t.other, t.unknown)
	}
	if st := sr.conn; st != nil {
		via := " via"
		if sr.ts != nil {
			via = " then"
		}
		fmt.Fprintf(&b, "%s=ssh_conn procs=%d env=%d withheld=%d has_conn=%d same=%d drop_self=%d drop_owner=%d drop_nested=%d",
			via, st.procs, st.env, st.withheld, st.set, st.same, st.self, st.owner, st.nested)
	}
	if sr.root == 0 && sr.ts == nil && sr.conn == nil {
		b.WriteString(" via=none")
	}
	b.WriteString(" chain=")
	b.WriteString(diagChain(s, l.self))
	return b.String()
}

func diagSource(src string) string {
	if src == "" {
		return "none"
	}
	return src
}

// diagChain lists the ancestors of pid, nearest first, as pid:name:owner
// joined by ">", then why the list ends: "|top" at a process without a
// parent, "|gone:<ppid>" when the parent is not in the snapshot,
// "|newer:<ppid>" when the parent started after its child (a recycled pid,
// see ptable.New), "|more" after maxDiagChain ancestors.
func diagChain(s *ptable.Snapshot, pid int) string {
	p := s.Get(pid)
	if p == nil {
		return "|gone:self"
	}
	var parts []string
	var end string
	for {
		if d := s.Detached(p.PID); d != 0 {
			end = "newer:" + strconv.Itoa(d)
			break
		}
		if p.PPID == 0 || p.PPID == p.PID {
			end = "top"
			break
		}
		parent := s.Get(p.PPID)
		if parent == nil {
			end = "gone:" + strconv.Itoa(p.PPID)
			break
		}
		if len(parts) == maxDiagChain {
			end = "more"
			break
		}
		p = parent
		parts = append(parts, fmt.Sprintf("%d:%s:%s", p.PID, diagName(p.Name), diagOwner(s.Owner(p.PID))))
	}
	return strings.Join(parts, ">") + "|" + end
}

// diagOwner renders an owner for a diag: "?" when unknown, the last part
// (the RID) of a Windows SID.
func diagOwner(o string) string {
	if o == "" {
		return "?"
	}
	if strings.HasPrefix(o, "S-") {
		return o[strings.LastIndexByte(o, '-')+1:]
	}
	return o
}

// diagName renders a process name as one short token of a diag.
func diagName(n string) string {
	if n == "" {
		return "?"
	}
	r := []rune(n)
	if len(r) > 16 {
		r = r[:16]
	}
	for i, c := range r {
		if unicode.IsSpace(c) || !unicode.IsPrint(c) || strings.ContainsRune(":>|", c) {
			r[i] = '_'
		}
	}
	return string(r)
}

// walker explores the process trees of one locate call.
type walker struct {
	l      *locator
	s      *ptable.Snapshot
	loc    location
	agents []*candidate
	seen   map[int]bool
	tmux   map[string]tmuxAnswer // list-clients per server, asked once
}

type tmuxAnswer struct {
	clients []tmuxClient
	err     error
}

// level describes where the processes being walked run.
type level struct {
	pane string
	// fg reports whether p is in front of the user: in the foreground of
	// its terminal, and that terminal is the one shown.
	fg func(p *ptable.Proc) bool
	// tty and session label the agents of one of several terminals (see
	// candidate).
	tty     string
	session int64
}

type muxProc struct {
	pid  int
	kind string
}

// explore collects the agents under roots, then follows the multiplexer
// clients among them into the panes they show.
func (w *walker) explore(roots []int, lv level, depth int) {
	var muxes []muxProc
	for _, r := range roots {
		w.walk(r, lv, nil, &muxes)
	}
	if depth >= maxMuxDepth {
		return
	}
	// A client in the foreground is what the terminal shows: it names the
	// target's multiplexer.
	sort.SliceStable(muxes, func(i, j int) bool {
		return lv.fg(w.s.Get(muxes[i].pid)) && !lv.fg(w.s.Get(muxes[j].pid))
	})
	for _, m := range muxes {
		r := w.resolve(m)
		if r.err != nil {
			w.loc.errs = append(w.loc.errs, r.err.Error())
		}
		if !r.client {
			continue
		}
		if depth == 0 && w.loc.mux == "" {
			w.loc.mux, w.loc.muxDetail = m.kind, r.detail
		}
		w.loc.ambiguous = w.loc.ambiguous || r.ambiguous
		shown := lv.fg(w.s.Get(m.pid))
		for _, ps := range r.panes {
			fg := ps.fg
			w.explore(ps.roots, level{pane: ps.label, tty: lv.tty, session: lv.session, fg: func(p *ptable.Proc) bool {
				return shown && (p.Foreground() || fg[p.PID])
			}}, depth+1)
		}
	}
}

// walk visits the tree at pid. Multiplexer processes are collected instead
// of descended into: what runs below a server is reached through the pane
// its client shows.
func (w *walker) walk(pid int, lv level, parent *candidate, muxes *[]muxProc) {
	if w.seen[pid] {
		return
	}
	w.seen[pid] = true
	p := w.s.Get(pid)
	if p == nil {
		return
	}
	if !w.l.owns(w.s, pid) {
		// Another user's process (`su bob`, `sudo`) is neither an agent nor
		// a multiplexer client. The processes below it that run as
		// stagent's user again (`su -` back) are the user's own.
		for _, c := range w.s.Children(pid) {
			w.walk(c, lv, nil, muxes)
		}
		return
	}
	argv := w.s.Argv(pid)
	if kind := muxKind(p.Name, argv); kind != "" {
		*muxes = append(*muxes, muxProc{pid, kind})
		return
	}
	if h := harnessOf(p.Name, argv); h != "" {
		if parent != nil && parent.harness == h {
			parent.pids = append(parent.pids, pid)
		} else {
			parent = &candidate{harness: h, pids: []int{pid}, pane: lv.pane, foreground: lv.fg(p), tty: lv.tty, session: lv.session}
			w.agents = append(w.agents, parent)
		}
	}
	for _, c := range w.s.Children(pid) {
		w.walk(c, lv, parent, muxes)
	}
}

// paneSet is one pane a client shows.
type paneSet struct {
	label string
	roots []int
	fg    map[int]bool // foreground pids the multiplexer reports (herdr)
}

// resolution is what a multiplexer process shows.
type resolution struct {
	client    bool // an attached client (not a server or a one-shot command)
	detail    string
	panes     []paneSet
	ambiguous bool
	err       error
}

func (w *walker) resolve(m muxProc) resolution {
	switch m.kind {
	case muxTmux:
		return w.resolveTmux(m.pid)
	case muxZellij:
		return w.resolveZellij(m.pid)
	case muxScreen:
		return w.resolveScreen(m.pid)
	case muxHerdr:
		return w.resolveHerdr(m.pid)
	}
	return resolution{}
}

// paneEnv are variables a client inherits when started inside another
// multiplexer's pane; passed on, they would point its CLI at that session.
var paneEnv = map[string]bool{
	"TMUX": true, "TMUX_PANE": true,
	"ZELLIJ": true, "ZELLIJ_SESSION_NAME": true, "ZELLIJ_PANE_ID": true,
	"STY": true, "WINDOW": true,
	"HERDR_ENV": true, "HERDR_SESSION": true, "HERDR_SOCKET_PATH": true,
	"HERDR_PANE_ID": true, "HERDR_TAB_ID": true, "HERDR_WORKSPACE_ID": true,
}

// invocation runs pid's CLI as pid runs it. pid is always one of stagent's
// user's processes: walk collects no other multiplexer client.
func (w *walker) invocation(pid int, name string) invocation {
	inv := invocation{bin: name}
	if exe := w.s.Exe(pid); filepath.IsAbs(exe) {
		inv.bin = exe
	}
	for _, kv := range w.s.Env(pid) {
		if k, _, _ := strings.Cut(kv, "="); !paneEnv[k] {
			inv.env = append(inv.env, kv)
		}
	}
	return inv
}

// ---------------------------------------------------------------------------
// tmux

func (w *walker) resolveTmux(pid int) resolution {
	if baseName(w.s.Get(pid).Name) == "tmux: server" {
		return resolution{} // Linux names its processes "tmux: server" / "tmux: client"
	}
	socket := tmuxSocket(w.s.Argv(pid))
	inv := w.invocation(pid, "tmux")
	key := strings.Join(append(slices.Clip(socket), inv.bin, ptable.Lookup(inv.env, "TMUX_TMPDIR")), "\x00")
	a, ok := w.tmux[key]
	if !ok {
		a.clients, a.err = w.l.sys.tmuxClients(inv, socket)
		w.tmux[key] = a
	}
	if a.err != nil {
		return resolution{err: a.err}
	}
	for _, c := range a.clients {
		if c.clientPID == pid {
			return resolution{
				client: true,
				detail: c.session + " " + c.paneID,
				panes:  []paneSet{{label: c.paneID, roots: []int{c.panePID}}},
			}
		}
	}
	return resolution{} // the server (macOS) or a one-shot command
}

// tmuxSocket returns the -L / -S option of a tmux command line (-S wins,
// as in tmux). Global options precede the command; getopt allows grouped
// flags and attached values ("-uLwork").
func tmuxSocket(argv []string) []string {
	var name, path string
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "--" || len(a) < 2 || a[0] != '-' {
			break
		}
		for j := 1; j < len(a); j++ {
			if !strings.ContainsRune("cfLST", rune(a[j])) {
				continue
			}
			v := a[j+1:]
			if v == "" && i+1 < len(argv) {
				i++
				v = argv[i]
			}
			switch a[j] {
			case 'L':
				name = v
			case 'S':
				path = v
			}
			break
		}
	}
	switch {
	case path != "":
		return []string{"-S", path}
	case name != "":
		return []string{"-L", name}
	}
	return nil
}

// ---------------------------------------------------------------------------
// zellij

func (w *walker) resolveZellij(pid int) resolution {
	session, ok := zellijClientSession(w.s.Argv(pid))
	if !ok {
		return resolution{}
	}
	servers := w.zellijServers()
	if session == "" {
		session = guessZellijSession(w.s, pid, servers)
		if session == "" {
			return resolution{client: true, ambiguous: true}
		}
	}
	clients, err := w.l.sys.zellijClients(w.invocation(pid, "zellij"), session)
	if err != nil {
		return resolution{err: err}
	}
	// Clients are listed without their pid: when they show different panes
	// ours cannot be told apart, and every shown pane is a candidate.
	var panes []string
	for _, c := range clients {
		if n, ok := strings.CutPrefix(c.pane, "terminal_"); ok && !slices.Contains(panes, n) {
			panes = append(panes, n)
		}
	}
	r := resolution{client: true, detail: session, ambiguous: len(panes) > 1}
	if len(panes) == 1 {
		r.detail += " terminal_" + panes[0]
	}
	server := servers[session]
	for _, n := range panes {
		ps := paneSet{label: "terminal_" + n}
		if server != 0 {
			for _, c := range w.s.Children(server) {
				if w.s.Getenv(c, "ZELLIJ_PANE_ID") == n {
					ps.roots = append(ps.roots, c)
				}
			}
		}
		r.panes = append(r.panes, ps)
	}
	return r
}

// zellijClientSession reports whether a zellij command line is a client —
// no subcommand, `attach`, or `options` — and the session it names.
func zellijClientSession(argv []string) (session string, client bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--server":
			return "", false
		case a == "-s" || a == "--session":
			if i+1 < len(argv) {
				i++
				session = argv[i]
			}
		case strings.HasPrefix(a, "--session="):
			session = strings.TrimPrefix(a, "--session=")
		case a == "-c" || a == "--config" || a == "--config-dir" || a == "--data-dir" ||
			a == "-l" || a == "--layout" || a == "--max-panes" || a == "-n" || a == "--new-session-with-layout":
			i++ // value
		case strings.HasPrefix(a, "-"):
		case a == "attach" || a == "a":
			for j := i + 1; j < len(argv); j++ {
				switch b := argv[j]; {
				case b == "--index":
					j++
				case b == "options":
					return session, true
				case !strings.HasPrefix(b, "-"):
					return b, true
				}
			}
			return session, true
		case a == "options":
			return session, true
		default:
			return "", false // action, run, list-sessions, … are one-shot commands
		}
	}
	return session, true
}

// zellijServers maps session names to the servers of stagent's user; a
// server runs as `zellij --server <socket dir>/<session>`. Another user's
// server of the same name is not the client's.
func (w *walker) zellijServers() map[string]int {
	out := map[string]int{}
	for _, pid := range w.s.PIDs() {
		if baseName(w.s.Get(pid).Name) != "zellij" || !w.l.owns(w.s, pid) {
			continue
		}
		argv := w.s.Argv(pid)
		for i, a := range argv {
			if a == "--server" && i+1 < len(argv) {
				out[filepath.Base(argv[i+1])] = pid
			}
		}
	}
	return out
}

// zellijStartWindow is how long after a client its server may start: a
// client without a session name starts a new session's server itself.
const zellijStartWindow = 30 * time.Second

// guessZellijSession finds the session of a client started without a name:
// the server started right after it, or the only server.
func guessZellijSession(s *ptable.Snapshot, client int, servers map[string]int) string {
	start := s.Get(client).Start
	var after, only string
	nAfter := 0
	for name, pid := range servers {
		only = name
		if d := s.Get(pid).Start.Sub(start); d >= 0 && d <= zellijStartWindow {
			after = name
			nAfter++
		}
	}
	switch {
	case nAfter == 1:
		return after
	case len(servers) == 1:
		return only
	}
	return ""
}

// ---------------------------------------------------------------------------
// screen

func (w *walker) resolveScreen(pid int) resolution {
	argv := w.s.Argv(pid)
	if len(argv) == 0 || filepath.Base(argv[0]) == "SCREEN" {
		return resolution{} // the server renames itself SCREEN
	}
	name, ok := screenClientSession(argv)
	if !ok {
		return resolution{}
	}
	servers := w.screenServers(pid, name)
	if len(servers) == 0 {
		return resolution{}
	}
	inv := w.invocation(pid, "screen")
	r := resolution{client: true, ambiguous: len(servers) > 1}
	for _, srv := range servers {
		sty := w.screenSty(srv)
		n, err := w.l.sys.screenWindow(inv, sty)
		if err != nil {
			// Screens without -Q (macOS ships 4.0): every window counts.
			r.ambiguous = true
			r.panes = append(r.panes, paneSet{roots: w.s.Children(srv)})
			if r.detail == "" {
				r.detail = sty
			}
			continue
		}
		win := strconv.Itoa(n)
		ps := paneSet{label: win}
		for _, c := range w.s.Children(srv) {
			if w.s.Getenv(c, "WINDOW") == win {
				ps.roots = append(ps.roots, c)
			}
		}
		r.panes = append(r.panes, ps)
		if r.detail == "" {
			r.detail = sty + " " + win
		}
	}
	return r
}

// screenClientSession reports whether a screen command line is a client
// and the session it names (-r/-x/-R/-d NAME, -S NAME).
func screenClientSession(argv []string) (name string, client bool) {
	wantName := false
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if a == "-" || !strings.HasPrefix(a, "-") {
			if wantName && name == "" {
				name, wantName = a, false
				continue
			}
			break // the command a new session runs
		}
		switch a {
		case "-Q", "-X", "-ls", "-list", "-wipe", "-v", "-version", "--version", "-help", "--help":
			return "", false // one-shot commands
		case "-c", "-e", "-h", "-p", "-T", "-t", "-s", "-Logfile":
			i++ // value
			continue
		}
		flags := a[1:]
		if strings.HasSuffix(flags, "S") { // -S NAME, also grouped: -dmS NAME
			if i+1 < len(argv) {
				i++
				name = argv[i]
			}
			continue
		}
		if strings.ContainsAny(flags, "rRxdD") {
			wantName = true
		}
	}
	return name, true
}

// screenServers returns the servers of stagent's user a client may be
// attached to: the one it started itself (its child), else those matching
// the name it gave, else every server.
func (w *walker) screenServers(client int, name string) []int {
	var all []int
	for _, pid := range w.s.PIDs() {
		if baseName(w.s.Get(pid).Name) != "screen" || !w.l.owns(w.s, pid) {
			continue
		}
		if argv := w.s.Argv(pid); len(argv) == 0 || filepath.Base(argv[0]) != "SCREEN" {
			continue
		}
		if descends(w.s, pid, client) {
			return []int{pid}
		}
		all = append(all, pid)
	}
	if name == "" {
		return all
	}
	var named []int
	for _, pid := range all {
		sty := w.screenSty(pid)
		if sty == name || strings.HasPrefix(sty, name+".") || strings.HasSuffix(sty, "."+name) {
			named = append(named, pid)
		}
	}
	return named
}

// screenSty is the session id ("<pid>.<name>") of a server, read from the
// STY its windows get (the user's own: the id goes on screen's command
// line); the pid alone also selects the session.
func (w *walker) screenSty(server int) string {
	for _, c := range w.s.Children(server) {
		if !w.l.owns(w.s, c) {
			continue
		}
		if sty := w.s.Getenv(c, "STY"); sty != "" {
			return sty
		}
	}
	return strconv.Itoa(server)
}

// ---------------------------------------------------------------------------
// herdr

func (w *walker) resolveHerdr(pid int) resolution {
	session, ok := herdrClientSession(w.s.Argv(pid))
	if !ok {
		return resolution{}
	}
	inv := w.invocation(pid, "herdr")
	panes, err := w.l.sys.herdrPanes(inv, session)
	if err != nil {
		return resolution{err: err}
	}
	label := session
	if label == "" {
		label = "default"
	}
	var focused []herdrPane
	for _, p := range panes {
		if p.Focused {
			focused = append(focused, p)
		}
	}
	r := resolution{client: true, detail: label, ambiguous: len(focused) > 1}
	if len(focused) == 1 {
		r.detail += " " + focused[0].ID
	}
	for _, fp := range focused {
		info, err := w.l.sys.herdrProcessInfo(inv, session, fp.ID)
		if err != nil {
			r.err = err
			continue
		}
		ps := paneSet{label: fp.ID, roots: []int{info.ShellPID}, fg: map[int]bool{}}
		for _, f := range info.Foreground {
			ps.fg[f.PID] = true
		}
		r.panes = append(r.panes, ps)
	}
	return r
}

// herdrClientSession reports whether a herdr command line is an attached
// client — `herdr`, `herdr --session NAME`, `herdr session attach NAME` —
// and its session ("" = the default one).
func herdrClientSession(argv []string) (session string, client bool) {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--session" || a == "--remote-keybindings":
			if i+1 >= len(argv) {
				return "", false
			}
			i++
			if a == "--session" {
				session = argv[i]
			}
		case strings.HasPrefix(a, "--session="):
			session = strings.TrimPrefix(a, "--session=")
		case a == "--remote" || a == "--machine" || strings.HasPrefix(a, "--remote=") || strings.HasPrefix(a, "--machine="):
			return "", false // the panes live on another host
		case strings.HasPrefix(a, "-"):
		case a == "session" && i+2 < len(argv) && argv[i+1] == "attach":
			return argv[i+2], true
		default:
			return "", false // server, pane, agent, …
		}
	}
	return session, true
}

// ---------------------------------------------------------------------------
// Process recognition

// baseName normalizes a process name for comparison: lower case, without
// directory and Windows executable suffix.
func baseName(name string) string {
	name = strings.ToLower(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	return strings.TrimSuffix(name, ".exe")
}

func argv0(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return baseName(argv[0])
}

// muxKind names the multiplexer a process belongs to — client, server or
// one-shot CLI call — and "" for other processes.
func muxKind(name string, argv []string) string {
	for _, n := range [2]string{baseName(name), argv0(argv)} {
		switch {
		case n == "tmux" || strings.HasPrefix(n, "tmux: "):
			return muxTmux
		case n == "zellij":
			return muxZellij
		case n == "screen":
			return muxScreen
		case n == "herdr":
			return muxHerdr
		}
	}
	return ""
}

// interpreters run agents distributed as scripts.
var interpreters = map[string]bool{"node": true, "nodejs": true, "bun": true, "deno": true}

// harnessPackages are the npm packages of the agents, for launchers that
// run their script by path (Claude Code's cli.js).
var harnessPackages = []struct{ dir, harness string }{
	{"/@anthropic-ai/claude-code/", wire.HarnessClaude},
	{"/@openai/codex/", wire.HarnessCodex},
	{"/@oh-my-pi/pi-coding-agent/", wire.HarnessOmp},
}

// harnessOf recognizes an agent by its process name, its argv[0] (native
// binaries, and scripts that set their process title) or the script an
// interpreter runs; "" for other processes.
func harnessOf(name string, argv []string) string {
	if h := harnessName(baseName(name)); h != "" {
		return h
	}
	if h := harnessName(argv0(argv)); h != "" {
		return h
	}
	if !interpreters[argv0(argv)] {
		return ""
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		script := strings.ReplaceAll(a, `\`, "/")
		base := baseName(script)
		for _, ext := range []string{".js", ".mjs", ".cjs", ".ts"} {
			base = strings.TrimSuffix(base, ext)
		}
		if h := harnessName(base); h != "" {
			return h
		}
		for _, p := range harnessPackages {
			if strings.Contains(script, p.dir) {
				return p.harness
			}
		}
		return ""
	}
	return ""
}

func harnessName(n string) string {
	switch n {
	case wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp:
		return n
	}
	// Codex's npm package has shipped its native binary as
	// codex-<target triple>.
	if strings.HasPrefix(n, "codex-x86_64-") || strings.HasPrefix(n, "codex-aarch64-") {
		return wire.HarnessCodex
	}
	return ""
}
