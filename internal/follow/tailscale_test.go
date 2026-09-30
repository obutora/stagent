package follow

import (
	"fmt"
	"testing"
)

const (
	tsConn  = "100.1.2.3 50600 100.4.5.6 22"
	tsIP    = "100.1.2.3"
	otherIP = "100.9.9.9"
)

// macTailscale is Tailscale SSH on macOS with System Integrity Protection
// on, stagent running as the user: every channel runs below the daemon
// through root's login, whose command line only root may read, and the
// kernel withholds the environment of /bin/zsh and of the agents. The
// sessions: the tab's terminal (51, tty 3), another terminal (71, tty 4),
// bob's terminal, carrying a copy of stagent's SSH_CONNECTION (81), and a
// command without a terminal (91).
func macTailscale() *tree {
	tr := newTree()
	tr.procs[1].Name = "launchd"
	tr.procs[1].as(rootUser)
	tr.user = rootUser
	tr.add(40, 1, "tailscaled", "tailscaled")
	for _, pid := range []int{50, 60, 70, 80, 90} {
		tr.add(pid, 40, "login")
	}
	tr.user = testUser
	tr.add(51, 50, "zsh", "-zsh").withhold().onTTY(3)
	tr.add(52, 51, "claude", "claude").withhold().onTTY(3).fg()
	tr.add(selfPID, 60, "stagent", "stagent", "follow").withEnv("SSH_CONNECTION=" + tsConn)
	tr.add(71, 70, "zsh", "-zsh").withhold().onTTY(4)
	tr.add(72, 71, "codex", "codex").withhold().onTTY(4).fg()
	tr.add(81, 80, "zsh", "-zsh").withEnv("SSH_CONNECTION=" + tsConn).as(bobUser).onTTY(5)
	tr.add(82, 81, "claude", "claude").as(bobUser).onTTY(5).fg()
	tr.add(91, 90, "zsh", "zsh", "-c", "rsync --server").withhold()
	return tr
}

func TestLocateTailscaleMac(t *testing.T) {
	for _, c := range []struct {
		name          string
		hosts         map[uint64]string // utmpx
		agentEnv      bool              // the tab's agent's environment is readable
		summary, diag string
	}{
		{"utmpx tells the tab", map[uint64]string{3: tsIP, 4: otherIP, 5: tsIP}, false, "none|||claude[52]*", ""},
		{"no utmpx", nil, false, "none||terminal_ambiguous|claude[52]#ttys003* codex[72]#ttys004*", ""},
		{"two tabs from this device", map[uint64]string{3: tsIP, 4: "::ffff:" + tsIP}, false,
			"none||terminal_ambiguous|claude[52]#ttys003* codex[72]#ttys004*", ""},
		{"readable agent", nil, true, "none|||claude[52]*", ""},
		{"only other devices", map[uint64]string{3: otherIP, 4: otherIP}, false, "none||no_terminal|",
			"pid=110 uid=1000 ssh_conn=set via=tailscale:40 sessions=4 foreign=1 notty=1 muxed=0 cands=2 our_conn=env our_ip=env " +
				"withheld=2 ip_utmp=2 exact=0 same_ip=0 other=2 unknown=0 chain=60:login:0>40:tailscaled:0>1:launchd:0|top"},
	} {
		tr := macTailscale()
		if c.agentEnv {
			tr.procs[52].withheld = false
			tr.procs[52].withEnv("SSH_CONNECTION=" + tsConn)
		}
		m := &fakeMux{}
		l := testLocator(m)
		l.sshConn = tsConn
		l.ttyHosts = func() map[uint64]string { return c.hosts }
		loc := l.locate(tr.snapshot())
		if got := summary(loc); got != c.summary || loc.diag != c.diag || len(m.calls) != 0 {
			t.Errorf("%s: got %q, diag %q, queries %q; want %q, diag %q", c.name, got, loc.diag, m.calls, c.summary, c.diag)
		}
	}
}

// linuxTailscale is Tailscale SSH on Linux: the terminals run below root's
// login (`login -f u -h IP -p`, a command line anyone may read), this exec
// channel below root's `su -l`, which cleared SSH_CONNECTION. The tab's
// terminal is 51 (pts/1), another device's 71 (pts/2).
func linuxTailscale() *tree {
	tr := newTree()
	tr.procs[1].as(rootUser)
	tr.user = rootUser
	tr.add(40, 1, "tailscaled", "/usr/sbin/tailscaled")
	tr.add(50, 40, "login", "login", "-f", "u", "-h", tsIP, "-p")
	tr.add(60, 40, "su", "su", "-w", "SSH_AUTH_SOCK", "-l", "u", "-c", "stagent follow")
	tr.add(70, 40, "login", "login", "-f", "u", "-h", otherIP, "-p")
	tr.user = testUser
	tr.add(51, 50, "bash", "-bash").withEnv("SSH_CONNECTION=" + tsConn).onTTY(1)
	tr.add(52, 51, "claude", "claude").withEnv("SSH_CONNECTION=" + tsConn).onTTY(1).fg()
	tr.add(61, 60, "bash", "bash", "-c", "stagent follow").withEnv("HOME=/home/u")
	tr.add(selfPID, 61, "stagent", "stagent", "follow").withEnv("HOME=/home/u")
	tr.add(71, 70, "bash", "-bash").withEnv("SSH_CONNECTION=" + otherIP + " 1 100.4.5.6 22").onTTY(2)
	tr.add(72, 71, "codex", "codex").withEnv("SSH_CONNECTION=" + otherIP + " 1 100.4.5.6 22").onTTY(2).fg()
	return tr
}

func TestLocateTailscaleLinux(t *testing.T) {
	for _, c := range []struct {
		name    string
		edit    func(tr *tree, l *locator)
		summary string
	}{
		// Nothing tells stagent's own connection: the only terminal is the
		// tab's; of two, either may be.
		{"one terminal", func(tr *tree, l *locator) { tr.remove(70); tr.remove(71); tr.remove(72) }, "none|||claude[52]*"},
		{"two terminals", func(*tree, *locator) {}, "none||terminal_ambiguous|claude[52]#pts/1* codex[72]#pts/2*"},
		// The incubator runs the command itself: its command line names
		// the client.
		{"incubator", func(tr *tree, _ *locator) {
			tr.procs[60].Name = "tailscaled"
			tr.procs[60].argv = []string{"/usr/sbin/tailscaled", "be-child", "ssh", "--uid=1000", "--remote-ip=" + tsIP, "--has-tty=false"}
		}, "none|||claude[52]*"},
		// As root, su's environment is readable, and root's sessions are
		// the login processes themselves.
		{"root", func(tr *tree, l *locator) {
			l.owner = rootUser
			tr.procs[60].env = []string{"SSH_CONNECTION=" + tsConn}
			for _, pid := range []int{51, 52, 61, selfPID, 71, 72} {
				tr.procs[pid].as(rootUser)
			}
			tr.procs[50].onTTY(1).env = []string{"SSH_CONNECTION=" + tsConn}
			tr.procs[70].onTTY(2)
		}, "none|||claude[52]*"},
	} {
		tr := linuxTailscale()
		l := testLocator(&fakeMux{})
		l.ttyName = func(dev uint64) string { return fmt.Sprintf("pts/%d", dev) }
		c.edit(tr, l)
		if got := summary(l.locate(tr.snapshot())); got != c.summary {
			t.Errorf("%s: got %q, want %q", c.name, got, c.summary)
		}
	}
}

func TestArgvIP(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want string
	}{
		{[]string{"/usr/bin/login", "-f", "-p", "-h", "100.1.2.3", "u", "/bin/zsh", "-c", "x -h 1.1.1.1"}, "100.1.2.3"},
		{[]string{"login", "-fp", "-h", "fd7a:115c:a1e0::1", "u"}, "fd7a:115c:a1e0::1"},
		{[]string{"tailscaled", "be-child", "ssh", "--remote-ip=::ffff:100.1.2.3"}, "100.1.2.3"},
		{[]string{"ssh", "-h", "100.1.2.3"}, ""},
		{[]string{"login", "-f", "u", "-h"}, ""},
		{[]string{"tailscaled", "--remote-ip=100.1.2.3"}, ""},
	} {
		if got := argvIP(c.argv); got != c.want {
			t.Errorf("%q: got %q, want %q", c.argv, got, c.want)
		}
	}
}
