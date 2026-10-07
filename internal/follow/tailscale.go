package follow

import (
	"net/netip"
	"strings"

	"github.com/obutora/stagent/internal/ptable"
)

// Tailscale SSH serves every connection from tailscaled itself: each
// channel runs below the daemon — through root's login (macOS, and
// terminals on Linux), `su -l` (commands on Linux) or the incubator
// `tailscaled be-child ssh` — so no process stands for one connection.
// The terminal is then one of the user's other sessions below the daemon
// that has a controlling terminal, told apart from the rest by what is
// known of each session's connection. Environments often cannot be read:
// macOS withholds those of restricted programs such as /bin/zsh
// (ptable.ErrEnvWithheld), and on Linux `su -l` clears SSH_CONNECTION from
// this exec channel. The client's address is known more often: from
// `login -h IP` and `--remote-ip=IP` of the incubator (command lines Linux
// shows everyone, macOS root only) and from utmpx, where macOS's login
// records it for the terminal.

// maxSessionEnvs bounds the processes of a session whose environment is
// read for its connection.
const maxSessionEnvs = 32

// tsStats counts what the Tailscale session search saw, for the diag.
type tsStats struct {
	daemon   int
	sessions int // the daemon's children besides stagent's own
	foreign  int // topmost processes of another user
	noTTY    int // of stagent's user, without a controlling terminal
	muxed    int // below a multiplexer
	cands    int // candidate terminals
	// ourConn and ourIP tell where stagent's own SSH_CONNECTION and client
	// address came from: "env", "anc" (an ancestor's environment), "argv"
	// (an ancestor's command line), "" unknown.
	ourConn, ourIP string
	withheld       int // candidates whose environment the OS withholds
	ipUtmp         int // candidates whose address utmpx gave
	// The candidates by verdict.
	exact, sameIP, other, unknown int
}

// tailscaleRoot returns the tailscaled daemon among pid's ancestors and the
// daemon's child whose subtree holds pid, zeros when there is none. The
// incubator runs under the daemon's name, so of adjacent tailscaled
// processes the topmost is the daemon.
func tailscaleRoot(s *ptable.Snapshot, pid int) (daemon, session int) {
	p := s.Get(pid)
	for range maxAncestry {
		if p == nil || p.PPID == 0 || p.PPID == p.PID {
			break
		}
		parent := s.Get(p.PPID)
		if parent == nil {
			break
		}
		if ptable.BaseName(parent.Name) == "tailscaled" {
			daemon, session = parent.PID, p.PID
		} else if daemon != 0 {
			break
		}
		p = parent
	}
	return daemon, session
}

// tailscaleSessions returns the terminals that may be the tab's, below
// daemon and outside stagent's own session own: the topmost processes of
// stagent's user with a controlling terminal, narrowed to those sharing
// stagent's SSH_CONNECTION, else its client address, else all the
// connection of which is unknown. Several mean the tab cannot be told
// apart.
func (l *locator) tailscaleSessions(s *ptable.Snapshot, daemon, own int, st *tsStats) []int {
	ours := l.ownConnection(s, daemon)
	st.ourConn, st.ourIP = ours.connSrc, ours.ipSrc
	var cands []int
	for _, c := range s.Children(daemon) {
		if c != own {
			st.sessions++
			l.sessionRoots(s, c, 0, st, &cands)
		}
	}
	st.cands = len(cands)
	var hosts map[uint64]string
	loaded := false
	var tiers [3][]int // exact, same address, unknown
	for _, r := range cands {
		c, withheld := l.sessionConnection(s, daemon, r)
		if withheld {
			st.withheld++
		}
		if c.ip == "" && ours.ip != "" && (ours.conn == "" || c.conn == "") {
			if !loaded {
				hosts, loaded = l.ttyHosts(), true
			}
			if h := hosts[s.Get(r).TTY]; h != "" {
				c.ip = normIP(h)
				st.ipUtmp++
			}
		}
		switch {
		case ours.conn != "" && c.conn != "":
			if c.conn != ours.conn {
				st.other++
				continue
			}
			st.exact++
			tiers[0] = append(tiers[0], r)
		case ours.ip != "" && c.ip != "":
			if c.ip != ours.ip {
				st.other++
				continue
			}
			st.sameIP++
			tiers[1] = append(tiers[1], r)
		default:
			st.unknown++
			tiers[2] = append(tiers[2], r)
		}
	}
	for _, t := range tiers {
		if len(t) > 0 {
			return t
		}
	}
	return nil
}

// sessionRoots collects below pid the topmost processes of stagent's user
// that have a controlling terminal. It descends only through root's
// processes (login, su, the incubator): a session of another user is
// theirs, even where it runs stagent's user again.
func (l *locator) sessionRoots(s *ptable.Snapshot, pid, depth int, st *tsStats, roots *[]int) {
	p := s.Get(pid)
	switch {
	case p == nil || depth >= maxAncestry:
	case l.owns(s, pid):
		if p.TTY == 0 {
			st.noTTY++
			return
		}
		*roots = append(*roots, pid)
	case s.Owner(pid) != "0":
		st.foreign++
	case muxKind(p.Name, s.Argv(pid)) != "":
		st.muxed++
	default:
		for _, c := range s.Children(pid) {
			l.sessionRoots(s, c, depth+1, st, roots)
		}
	}
}

// connection is what is known of an SSH connection.
type connection struct {
	conn, ip       string // SSH_CONNECTION, the client's address
	connSrc, ipSrc string // where they came from (see tsStats)
}

// fromEnv takes what env tells.
func (c *connection) fromEnv(env []string, src string) {
	if c.conn == "" {
		if v := ptable.Lookup(env, "SSH_CONNECTION"); v != "" {
			c.conn, c.connSrc = v, src
			if c.ip == "" {
				c.ip, c.ipSrc = clientIP(v), src
			}
		}
	}
	if c.ip == "" {
		if v := ptable.Lookup(env, "SSH_CLIENT"); v != "" {
			c.ip, c.ipSrc = clientIP(v), src
		}
	}
}

// fromArgv takes the client address a command line names.
func (c *connection) fromArgv(argv []string) {
	if c.ip == "" {
		if ip := argvIP(argv); ip != "" {
			c.ip, c.ipSrc = ip, "argv"
		}
	}
}

// ownConnection is what stagent knows of its own connection: its
// environment, else its ancestors' below daemon (readable when they are
// its user's, or stagent is root), else their command lines.
func (l *locator) ownConnection(s *ptable.Snapshot, daemon int) connection {
	var c connection
	c.fromEnv([]string{"SSH_CONNECTION=" + l.sshConn, "SSH_CLIENT=" + l.sshClient}, "env")
	anc := ancestorsBelow(s, l.self, daemon)
	for _, pid := range anc {
		c.fromEnv(s.Env(pid), "anc")
	}
	for _, pid := range anc {
		c.fromArgv(s.Argv(pid))
	}
	return c
}

// sessionConnection is what is known of the connection of the session at
// root: the environment of its processes of stagent's user (root first;
// macOS may withhold the shell's but not an agent's), else of the
// processes that started it (root's login or su, readable when stagent is
// root), else their command lines. withheld reports whether the OS
// withheld root's environment.
func (l *locator) sessionConnection(s *ptable.Snapshot, daemon, root int) (c connection, withheld bool) {
	queue := []int{root}
	for n := 0; len(queue) > 0 && n < maxSessionEnvs && c.conn == ""; n++ {
		pid := queue[0]
		queue = queue[1:]
		if pid != root && !l.owns(s, pid) {
			continue
		}
		env := s.Env(pid)
		if pid == root && env == nil {
			withheld = s.EnvWithheld(pid)
		}
		c.fromEnv(env, "env")
		queue = append(queue, s.Children(pid)...)
	}
	anc := ancestorsBelow(s, root, daemon)
	for _, pid := range anc {
		c.fromEnv(s.Env(pid), "anc")
	}
	for _, pid := range anc {
		c.fromArgv(s.Argv(pid))
	}
	return c, withheld
}

// ancestorsBelow returns the ancestors of pid below top, nearest first.
func ancestorsBelow(s *ptable.Snapshot, pid, top int) []int {
	var out []int
	p := s.Get(pid)
	for range maxAncestry {
		if p == nil || p.PPID == 0 || p.PPID == p.PID || p.PPID == top {
			break
		}
		if p = s.Get(p.PPID); p != nil {
			out = append(out, p.PID)
		}
	}
	return out
}

// argvIP returns the client address a login command line (`login … -h IP
// …`) or an incubator's (`tailscaled be-child ssh … --remote-ip=IP`)
// names, "" for other command lines.
func argvIP(argv []string) string {
	switch {
	case ptable.Argv0(argv) == "login":
		for i, a := range argv[1:] {
			if a == "-h" && i+2 < len(argv) {
				return normIP(argv[i+2])
			}
		}
	case len(argv) > 1 && argv[1] == "be-child":
		for _, a := range argv[2:] {
			if v, ok := strings.CutPrefix(a, "--remote-ip="); ok {
				return normIP(v)
			}
		}
	}
	return ""
}

// clientIP returns the client address of an SSH_CONNECTION or SSH_CLIENT
// value (its first field).
func clientIP(v string) string {
	f, _, _ := strings.Cut(strings.TrimSpace(v), " ")
	return normIP(f)
}

// normIP returns an address in one spelling (IPv4-mapped IPv6 as IPv4);
// other strings stay as they are.
func normIP(v string) string {
	if a, err := netip.ParseAddr(v); err == nil {
		return a.Unmap().String()
	}
	return v
}
