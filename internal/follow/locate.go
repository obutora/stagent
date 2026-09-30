package follow

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

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
	reasonNoTerminal = "no_terminal"
	reasonNoAgent    = "no_agent"
	reasonAmbiguous  = "mux_ambiguous"
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
}

// location is what locate found in one process-table snapshot.
type location struct {
	noTerminal     bool
	mux, muxDetail string
	ambiguous      bool
	agents         []candidate
	errs           []string // failed multiplexer queries
}

// locator finds the agents running in the terminal that shares stagent's
// SSH connection. Its decisions depend only on the snapshot and muxSystem.
type locator struct {
	self    int
	sshConn string // stagent's own SSH_CONNECTION ("" when unset)
	sys     muxSystem
	// sameConn caches, per process instance, whether its environment has
	// stagent's SSH_CONNECTION: reading every environment is the costly
	// part of the fallback, and a process's initial environment is fixed.
	sameConn map[instance]bool
}

// instance identifies a process across snapshots (pids are recycled).
type instance struct {
	pid   int
	start int64
}

func newLocator(sys muxSystem) *locator {
	return &locator{
		self:     os.Getpid(),
		sshConn:  os.Getenv("SSH_CONNECTION"),
		sys:      sys,
		sameConn: map[instance]bool{},
	}
}

// locate finds the agents in the terminal: in its shell's process tree, and
// in the pane a multiplexer client in that tree shows.
func (l *locator) locate(s *ptable.Snapshot) location {
	roots, ok := l.scope(s)
	if !ok || len(roots) == 0 {
		return location{noTerminal: true, mux: muxNone}
	}
	w := &walker{l: l, s: s, seen: map[int]bool{}, tmux: map[string]tmuxAnswer{}}
	w.explore(roots, level{fg: (*ptable.Proc).Foreground}, 0)
	loc := w.loc
	if loc.mux == "" {
		loc.mux = muxNone
	}
	for _, c := range w.agents {
		loc.agents = append(loc.agents, *c)
	}
	return loc
}

// scope returns the roots of the process trees belonging to the terminal.
//
// Every channel of an SSH connection — the terminal's shell and this exec
// channel — is a child of the connection's SSH server process, so the
// terminal is that process's other children. Without such an ancestor
// (Tailscale SSH serves connections from tailscaled itself) the processes
// carrying stagent's SSH_CONNECTION stand in for them.
func (l *locator) scope(s *ptable.Snapshot) ([]int, bool) {
	if root := connectionRoot(s, l.self); root != 0 {
		var roots []int
		for _, c := range s.Children(root) {
			if !descends(s, l.self, c) {
				roots = append(roots, c)
			}
		}
		return roots, true
	}
	if l.sshConn == "" {
		return nil, false
	}
	return l.sameConnection(s), true
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

// sameConnection returns the topmost processes whose environment has
// stagent's SSH_CONNECTION, except stagent's own ancestors and descendants
// and processes in a multiplexer's panes (reached through its client).
func (l *locator) sameConnection(s *ptable.Snapshot) []int {
	cache := make(map[instance]bool, len(l.sameConn))
	match := map[int]bool{}
	for _, pid := range s.PIDs() {
		if descends(s, pid, l.self) || descends(s, l.self, pid) {
			continue
		}
		id := instance{pid, s.Get(pid).Start.UnixNano()}
		m, ok := l.sameConn[id]
		if !ok {
			m = s.Getenv(pid, "SSH_CONNECTION") == l.sshConn
		}
		cache[id] = m
		if m {
			match[pid] = true
		}
	}
	l.sameConn = cache
	var roots []int
	for _, pid := range s.PIDs() {
		if match[pid] && !l.nested(s, pid, match) {
			roots = append(roots, pid)
		}
	}
	return roots
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
			w.explore(ps.roots, level{pane: ps.label, fg: func(p *ptable.Proc) bool {
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
	argv := w.s.Argv(pid)
	if kind := muxKind(p.Name, argv); kind != "" {
		*muxes = append(*muxes, muxProc{pid, kind})
		return
	}
	if h := harnessOf(p.Name, argv); h != "" {
		if parent != nil && parent.harness == h {
			parent.pids = append(parent.pids, pid)
		} else {
			parent = &candidate{harness: h, pids: []int{pid}, pane: lv.pane, foreground: lv.fg(p)}
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
	servers := zellijServers(w.s)
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

// zellijServers maps session names to server pids; a server runs as
// `zellij --server <socket dir>/<session>`.
func zellijServers(s *ptable.Snapshot) map[string]int {
	out := map[string]int{}
	for _, pid := range s.PIDs() {
		if baseName(s.Get(pid).Name) != "zellij" {
			continue
		}
		argv := s.Argv(pid)
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

// screenServers returns the servers a client may be attached to: the one
// it started itself (its child), else those matching the name it gave,
// else every server.
func (w *walker) screenServers(client int, name string) []int {
	var all []int
	for _, pid := range w.s.PIDs() {
		if baseName(w.s.Get(pid).Name) != "screen" {
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
// STY its windows get; the pid alone also selects the session.
func (w *walker) screenSty(server int) string {
	for _, c := range w.s.Children(server) {
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
