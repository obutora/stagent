package follow

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// summary renders a location compactly: "mux|detail|flags|agents", an
// agent as harness[pids]@pane#tty, "*" when in the foreground.
func summary(l location) string {
	var agents []string
	for _, c := range l.agents {
		s := fmt.Sprintf("%s%v", c.harness, c.pids)
		if c.pane != "" {
			s += "@" + c.pane
		}
		if c.tty != "" {
			s += "#" + c.tty
		}
		if c.foreground {
			s += "*"
		}
		agents = append(agents, s)
	}
	var flags []string
	if l.noTerminal {
		flags = append(flags, "no_terminal")
	}
	if l.terminalAmbiguous {
		flags = append(flags, "terminal_ambiguous")
	}
	if l.ambiguous {
		flags = append(flags, "ambiguous")
	}
	return strings.Join([]string{l.mux, l.muxDetail, strings.Join(flags, ","), strings.Join(agents, " ")}, "|")
}

func TestLocateDirectShell(t *testing.T) {
	tr := sshTree()
	tr.add(102, 101, "claude", "claude").fg()
	// Suspended with ^Z: still an agent of the terminal, not in front.
	tr.add(103, 101, "node", "node", "--no-warnings", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js").bg()
	tr.add(104, 103, "node", "node", "/x/mcp-server.js").bg()
	loc := testLocator(&fakeMux{}).locate(tr.snapshot())
	if got, want := summary(loc), "none|||claude[102]* claude[103]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateMergesLauncherAndNativeBinary(t *testing.T) {
	tr := sshTree()
	tr.add(102, 101, "node", "node", "/usr/bin/codex").fg()
	tr.add(103, 102, "codex", "/usr/lib/node_modules/@openai/codex/vendor/x86_64-unknown-linux-musl/codex/codex").fg()
	tr.add(104, 103, "bash", "bash", "-c", "ls").fg()
	tr.add(105, 101, "omp", "bun", "/home/u/.bun/bin/omp").bg()
	tr.add(106, 105, "omp", "/home/u/.bun/bin/bun", "/home/u/.bun/install/global/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js", "__omp_worker_js_eval_process")
	loc := testLocator(&fakeMux{}).locate(tr.snapshot())
	if got, want := summary(loc), "none|||codex[102 103]* omp[105 106]"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateTmuxPane(t *testing.T) {
	tr := sshTree()
	// Linux names a tmux client "tmux: client" (macOS keeps "tmux").
	client := tr.add(104, 101, "tmux: client", "tmux", "-2", "-Lwork", "attach", "-t", "main").fg()
	client.exe = "/opt/homebrew/bin/tmux"
	client.env = []string{"PATH=/usr/bin", "TMUX_TMPDIR=/tmp/t", "ZELLIJ_SESSION_NAME=outer"}
	tr.add(200, 1, "tmux: server", "tmux", "-Lwork", "new", "-s", "main")
	tr.add(201, 200, "bash", "-bash")
	tr.add(202, 201, "claude", "claude").fg()
	tr.add(203, 200, "bash", "-bash")
	tr.add(204, 203, "codex", "codex").fg()
	m := &fakeMux{tmux: map[string][]tmuxClient{"-L work": {
		{clientPID: 150, panePID: 203, paneID: "%4", session: "other"},
		{clientPID: 104, panePID: 201, paneID: "%3", session: "main"},
	}}}
	var got invocation
	sys := &recordingMux{fakeMux: m, inv: &got}
	loc := testLocator(sys).locate(tr.snapshot())
	if s, want := summary(loc), "tmux|main %3||claude[202]@%3*"; s != want {
		t.Fatalf("got %q, want %q", s, want)
	}
	// The client's binary and environment, minus the variables that would
	// point the CLI at another multiplexer.
	want := invocation{bin: "/opt/homebrew/bin/tmux", env: []string{"PATH=/usr/bin", "TMUX_TMPDIR=/tmp/t"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("invocation %+v, want %+v", got, want)
	}
}

// recordingMux records the invocation of the last query.
type recordingMux struct {
	*fakeMux
	inv *invocation
}

func (r *recordingMux) tmuxClients(inv invocation, socket []string) ([]tmuxClient, error) {
	*r.inv = inv
	return r.fakeMux.tmuxClients(inv, socket)
}

func TestLocateTmuxServerAndOneShotCommandsAreNotClients(t *testing.T) {
	tr := sshTree()
	tr.add(104, 101, "tmux", "tmux", "display", "-p", "#S").fg()
	m := &fakeMux{tmux: map[string][]tmuxClient{"": {{clientPID: 150, panePID: 1, paneID: "%0", session: "x"}}}}
	loc := testLocator(m).locate(tr.snapshot())
	if got, want := summary(loc), "none|||"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func zellijTree() *tree {
	tr := sshTree()
	tr.add(104, 101, "zellij", "zellij", "attach", "work").fg()
	tr.add(300, 1, "zellij", "/home/u/.local/bin/zellij", "--server", "/run/user/1000/zellij/contract_version_1/work")
	tr.add(301, 300, "bash", "/bin/bash").withEnv("ZELLIJ_PANE_ID=0")
	tr.add(302, 301, "codex", "codex").fg()
	tr.add(303, 300, "bash", "/bin/bash").withEnv("ZELLIJ_PANE_ID=1")
	tr.add(304, 303, "claude", "claude").fg()
	tr.add(305, 300, "bash", "/bin/bash").withEnv("ZELLIJ_PANE_ID=2")
	tr.add(306, 305, "omp", "omp").fg()
	return tr
}

func TestLocateZellijSingleClient(t *testing.T) {
	m := &fakeMux{zellij: map[string][]zellijClient{"work": {{id: "1", pane: "terminal_1"}}}}
	loc := testLocator(m).locate(zellijTree().snapshot())
	if got, want := summary(loc), "zellij|work terminal_1||claude[304]@terminal_1*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateZellijClientsOnSamePaneAreNotAmbiguous(t *testing.T) {
	m := &fakeMux{zellij: map[string][]zellijClient{"work": {
		{id: "1", pane: "terminal_1"}, {id: "2", pane: "terminal_1"}, {id: "3", pane: "plugin_0"},
	}}}
	loc := testLocator(m).locate(zellijTree().snapshot())
	if got, want := summary(loc), "zellij|work terminal_1||claude[304]@terminal_1*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateZellijAmbiguousClients(t *testing.T) {
	m := &fakeMux{zellij: map[string][]zellijClient{"work": {
		{id: "1", pane: "terminal_1"}, {id: "2", pane: "terminal_0"},
	}}}
	loc := testLocator(m).locate(zellijTree().snapshot())
	if got, want := summary(loc), "zellij|work|ambiguous|claude[304]@terminal_1* codex[302]@terminal_0*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateZellijClientWithoutSessionName(t *testing.T) {
	tr := zellijTree()
	tr.procs[104].argv = []string{"zellij"}
	// A second session whose server started long before this client.
	tr.add(290, 1, "zellij", "zellij", "--server", "/run/user/1000/zellij/contract_version_1/old")
	tr.procs[290].Start = tr.base
	// The client started the "work" server right before it.
	tr.procs[300].Start = tr.procs[104].Start.Add(200e6)
	m := &fakeMux{zellij: map[string][]zellijClient{"work": {{id: "1", pane: "terminal_2"}}}}
	loc := testLocator(m).locate(tr.snapshot())
	if got, want := summary(loc), "zellij|work terminal_2||omp[306]@terminal_2*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateHerdrFocusedPane(t *testing.T) {
	tr := sshTree()
	tr.add(104, 101, "herdr", "herdr", "--session", "dev").fg()
	// Started by its first client: the server is inside the terminal's tree.
	tr.add(105, 104, "herdr", "/home/u/.local/bin/herdr", "server")
	tr.add(106, 105, "bash", "/bin/bash")
	tr.add(107, 106, "claude", "claude").fg()
	tr.add(108, 105, "bash", "/bin/bash")
	tr.add(109, 108, "omp", "omp") // no terminal info: herdr says it is in front
	m := &fakeMux{
		herdrList: map[string][]herdrPane{"dev": {{ID: "w1:p1"}, {ID: "w1:p2", Focused: true}}},
		herdrInfos: map[string]herdrProcessInfo{"dev w1:p2": {ShellPID: 108, Foreground: []struct {
			PID int `json:"pid"`
		}{{PID: 109}}}},
	}
	loc := testLocator(m).locate(tr.snapshot())
	if got, want := summary(loc), "herdr|dev w1:p2||omp[109]@w1:p2*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func screenTree() *tree {
	tr := sshTree()
	tr.add(104, 101, "screen", "screen", "-r", "work").fg()
	tr.add(400, 1, "screen", "SCREEN", "-S", "work")
	tr.add(401, 400, "bash", "/bin/bash").withEnv("STY=400.work", "WINDOW=0")
	tr.add(402, 401, "claude", "claude").fg()
	tr.add(403, 400, "bash", "/bin/bash").withEnv("STY=400.work", "WINDOW=1")
	tr.add(404, 403, "codex", "codex").fg()
	tr.add(410, 1, "screen", "SCREEN", "-S", "other")
	tr.add(411, 410, "bash", "/bin/bash").withEnv("STY=410.other", "WINDOW=0")
	tr.add(412, 411, "omp", "omp").fg()
	return tr
}

func TestLocateScreenWindow(t *testing.T) {
	m := &fakeMux{screen: map[string]int{"400.work": 1}}
	loc := testLocator(m).locate(screenTree().snapshot())
	if got, want := summary(loc), "screen|400.work 1||codex[404]@1*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateScreenWithoutQueryIsAmbiguous(t *testing.T) {
	loc := testLocator(&fakeMux{}).locate(screenTree().snapshot())
	if got, want := summary(loc), "screen|400.work|ambiguous|claude[402]* codex[404]*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateNoTerminal(t *testing.T) {
	tr := newTree()
	tr.add(50, 1, "tailscaled", "tailscaled")
	tr.add(selfPID, 50, "stagent", "stagent", "follow")
	loc := testLocator(&fakeMux{}).locate(tr.snapshot())
	if got, want := summary(loc), "none||no_terminal|"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if want := "pid=110 uid=1000 ssh_conn=unset via=tailscale:50 sessions=0 foreign=0 notty=0 muxed=0 cands=0 our_conn=none our_ip=none " +
		"withheld=0 ip_utmp=0 exact=0 same_ip=0 other=0 unknown=0 chain=50:tailscaled:1000>1:systemd:1000|top"; loc.diag != want {
		t.Fatalf("diag %q, want %q", loc.diag, want)
	}
}

// A connection root without the terminal (it runs in another connection):
// the diag names the root and its children.
func TestLocateNoTerminalDiagConnectionRoot(t *testing.T) {
	tr := newTree()
	tr.procs[1].as(rootUser)
	tr.user = rootUser
	tr.add(90, 1, "sshd", "sshd: /usr/sbin/sshd -D [listener]")
	tr.add(95, 90, "sshd-session", "sshd-session: user [priv]")
	tr.user = testUser
	tr.add(100, 95, "sshd-session", "sshd-session: user@notty")
	tr.add(selfPID, 100, "stagent", "stagent", "follow")
	loc := testLocator(&fakeMux{}).locate(tr.snapshot())
	want := "pid=110 uid=1000 ssh_conn=unset via=root:sshd-session:100 children=1 chain=100:sshd-session:1000>95:sshd-session:0>90:sshd:0>1:systemd:0|top"
	if !loc.noTerminal || loc.diag != want {
		t.Fatalf("no_terminal %v, diag %q; want %q", loc.noTerminal, loc.diag, want)
	}
}

// An SSH server that runs every channel below one daemon through root's
// login, without a per-connection process: only the environment tells the
// terminal. When the OS withholds the environment of the terminal's
// programs (macOS's System Integrity Protection) the diag counts why;
// readable, the terminal is found.
func TestLocateBySSHConnectionDiag(t *testing.T) {
	const conn = "100.1.2.3 50600 100.4.5.6 22"
	build := func(sip bool) *tree {
		tr := newTree()
		tr.procs[1].Name = "launchd"
		tr.procs[1].as(rootUser)
		tr.user = rootUser
		tr.add(40, 1, "teleport", "teleport")
		tr.add(50, 40, "login", "login", "-f", "-p", "-h", "100.1.2.3", "u")
		tr.add(60, 40, "login", "login", "-f", "-pq", "-h", "100.1.2.3", "u", "/bin/zsh", "-c", "stagent follow")
		tr.user = testUser
		shell, agent := tr.add(51, 50, "zsh", "-zsh").fg(), tr.add(52, 51, "claude", "claude").fg()
		if sip {
			shell.withhold()
			agent.withhold()
		} else {
			shell.withEnv("SSH_CONNECTION=" + conn)
			agent.withEnv("SSH_CONNECTION=" + conn)
		}
		tr.add(selfPID, 60, "stagent", "stagent", "follow").withEnv("SSH_CONNECTION=" + conn)
		tr.add(111, selfPID, "tmux", "tmux", "list-clients").withEnv("SSH_CONNECTION=" + conn)
		tr.add(70, 1, "claude", "claude").withEnv("SSH_CONNECTION=" + conn).as(bobUser)
		tr.add(200, 1, "tmux: server", "tmux").withhold()
		tr.add(201, 200, "bash", "-bash").withEnv("SSH_CONNECTION=" + conn)
		tr.add(80, 1, "node", "node").withEnv("SSH_CONNECTION=100.9.9.9 1 100.4.5.6 22")
		tr.add(90, 1, "cron", "cron").withEnv("HOME=/Users/u")
		return tr
	}
	for _, c := range []struct {
		name          string
		sip           bool
		summary, diag string
	}{
		{"readable", false, "none|||claude[52]*", ""},
		{"withheld", true, "none||no_terminal|", "pid=110 uid=1000 ssh_conn=set via=ssh_conn procs=12 env=5 withheld=3 has_conn=4 same=3 " +
			"drop_self=1 drop_owner=1 drop_nested=1 chain=60:login:0>40:teleport:0>1:launchd:0|top"},
	} {
		l := testLocator(&fakeMux{})
		l.sshConn = conn
		loc := l.locate(build(c.sip).snapshot())
		if got := summary(loc); got != c.summary || loc.diag != c.diag {
			t.Errorf("%s: got %q, diag %q; want %q, diag %q", c.name, got, loc.diag, c.summary, c.diag)
		}
	}
}

// root's login forks the shell's process, which setuids to the user and
// execs the shell, keeping its pid and start time: what stagent read of the
// process before must not stick to the shell.
func TestLocateBySSHConnectionAfterExec(t *testing.T) {
	const conn = "SSH_CONNECTION=100.1.2.3 50600 100.4.5.6 22"
	tr := newTree()
	tr.user = rootUser
	tr.add(40, 1, "teleport", "teleport")
	tr.add(50, 40, "login", "login", "-f", "-p", "u")
	child := tr.add(51, 50, "login", "login", "-f", "-p", "u")
	tr.add(60, 40, "login", "login", "-f", "-pq", "u", "/bin/zsh", "-c", "stagent follow")
	tr.user = testUser
	tr.add(selfPID, 60, "stagent", "stagent", "follow").withEnv(conn)
	l := testLocator(&fakeMux{})
	l.sshConn = strings.TrimPrefix(conn, "SSH_CONNECTION=")
	if got, want := summary(l.locate(tr.snapshot())), "none||no_terminal|"; got != want {
		t.Fatalf("before exec: got %q, want %q", got, want)
	}
	child.Name, child.argv, child.env = "zsh", []string{"-zsh"}, []string{conn}
	child.as(testUser).fg()
	if got, want := summary(l.locate(tr.snapshot())), "none|||"; got != want {
		t.Fatalf("after exec: got %q, want %q", got, want)
	}
}

func TestDiagChain(t *testing.T) {
	deep := newTree()
	for pid := 2; pid <= 10; pid++ {
		deep.add(pid, pid-1, "sh", "sh")
	}
	deep.add(selfPID, 10, "stagent", "stagent")
	odd := newTree()
	odd.add(20, 1, "tmux: server", "tmux").as("")
	odd.add(30, 20, "a-very-long-process-name", "x")
	odd.add(selfPID, 30, "stagent", "stagent")
	gone := newTree()
	gone.add(selfPID, 77, "stagent", "stagent")
	newer := newTree()
	newer.add(120, 1, "sh", "sh") // started after its "child": a recycled pid
	newer.add(selfPID, 120, "stagent", "stagent")
	for _, c := range []struct {
		name string
		tr   *tree
		want string
	}{
		{"deep", deep, "10:sh:1000>9:sh:1000>8:sh:1000>7:sh:1000>6:sh:1000>5:sh:1000>4:sh:1000>3:sh:1000|more"},
		{"odd names", odd, "30:a-very-long-proc:1000>20:tmux__server:?>1:systemd:1000|top"},
		{"parent gone", gone, "|gone:77"},
		{"parent newer", newer, "|newer:120"},
	} {
		if got := diagChain(c.tr.snapshot(), selfPID); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLocateBySSHConnection(t *testing.T) {
	const conn = "100.1.2.3 50600 100.4.5.6 22"
	tr := newTree()
	tr.add(40, 1, "tailscaled", "tailscaled")
	tr.add(50, 40, "tailscaled", "tailscaled", "be-child", "ssh").withEnv("SSH_CONNECTION=" + conn)
	tr.add(selfPID, 50, "stagent", "stagent", "follow").withEnv("SSH_CONNECTION=" + conn)
	tr.add(111, selfPID, "tmux", "tmux", "list-clients").withEnv("SSH_CONNECTION=" + conn)
	tr.add(60, 40, "tailscaled", "tailscaled", "be-child", "ssh").withEnv("SSH_CONNECTION=" + conn)
	tr.add(61, 60, "bash", "-bash").withEnv("SSH_CONNECTION=" + conn).fg()
	tr.add(62, 61, "claude", "claude").withEnv("SSH_CONNECTION=" + conn).fg()
	// Another connection.
	tr.add(70, 40, "tailscaled", "tailscaled", "be-child", "ssh").withEnv("SSH_CONNECTION=100.9.9.9 1 100.4.5.6 22")
	tr.add(71, 70, "claude", "claude").withEnv("SSH_CONNECTION=100.9.9.9 1 100.4.5.6 22").fg()
	// A tmux server started from this connection: its panes carry the same
	// SSH_CONNECTION but are only reached through a client.
	tr.add(200, 1, "tmux: server", "tmux").withEnv("SSH_CONNECTION=" + conn)
	tr.add(201, 200, "bash", "-bash").withEnv("SSH_CONNECTION=" + conn)
	tr.add(202, 201, "codex", "codex").withEnv("SSH_CONNECTION=" + conn).fg()
	l := testLocator(&fakeMux{})
	l.sshConn = conn
	loc := l.locate(tr.snapshot())
	if got, want := summary(loc), "none|||claude[62]*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// stagent as root in the SSH_CONNECTION fallback (Tailscale SSH as root):
// another user can copy the connection's SSH_CONNECTION (it is visible in
// `ss`) into processes of their own, which must neither stand in for the
// terminal nor have their executable run.
func TestLocateBySSHConnectionIgnoresOtherUsers(t *testing.T) {
	const conn = "100.1.2.3 50600 100.4.5.6 22"
	tr := newTree()
	tr.user = rootUser
	tr.add(40, 1, "tailscaled", "tailscaled")
	tr.add(50, 40, "tailscaled", "tailscaled", "be-child", "ssh").withEnv("SSH_CONNECTION=" + conn)
	tr.add(selfPID, 50, "stagent", "stagent", "follow").withEnv("SSH_CONNECTION=" + conn)
	tr.add(60, 40, "login", "login", "-f", "root", "-p").withEnv("SSH_CONNECTION=" + conn)
	tr.add(61, 60, "bash", "-bash").withEnv("SSH_CONNECTION=" + conn).fg()
	tr.add(62, 61, "claude", "claude").withEnv("SSH_CONNECTION=" + conn).fg()
	// bob's fake tmux client, in the foreground of a terminal of his.
	fake := tr.add(80, 1, "tmux", "tmux", "attach").withEnv("SSH_CONNECTION="+conn, "LD_PRELOAD=/home/bob/x.so").as(bobUser).fg()
	fake.exe = "/home/bob/tmux"
	tr.add(81, 1, "claude", "claude").withEnv("SSH_CONNECTION=" + conn).as(bobUser).fg()
	// A set-user-ID root program bob started, titled tmux (`exec -a`): it
	// runs as root, with his environment.
	tr.add(82, 1, "passwd", "tmux").withEnv("SSH_CONNECTION=" + conn).as("").fg()
	m := &fakeMux{tmux: map[string][]tmuxClient{"": {
		{clientPID: 80, panePID: 61, paneID: "%0", session: "0"},
		{clientPID: 82, panePID: 61, paneID: "%0", session: "0"},
	}}}
	l := testLocator(m)
	l.owner, l.sshConn = rootUser, conn
	loc := l.locate(tr.snapshot())
	if got, want := summary(loc), "none|||claude[62]*"; got != want || len(m.calls) != 0 {
		t.Fatalf("got %q, queries %q; want %q and none", got, m.calls, want)
	}
}

// `su bob` in root's terminal: bob's agents and multiplexer clients are not
// root's, but root's again below them (`su -`) are.
func TestLocateIgnoresOtherUsersInTerminal(t *testing.T) {
	tr := newTree()
	tr.user = rootUser
	tr.add(90, 1, "sshd", "sshd: /usr/sbin/sshd -D [listener]")
	tr.add(100, 90, "sshd-session", "sshd-session: root@pts/0,notty")
	tr.add(101, 100, "bash", "-bash")
	tr.add(selfPID, 100, "stagent", "/root/.ssh-term/agent/bin/stagent", "follow")
	tr.add(102, 101, "su", "su", "bob")
	tr.add(103, 102, "bash", "bash").as(bobUser)
	tr.add(104, 103, "claude", "claude").as(bobUser).bg()
	client := tr.add(105, 103, "tmux: client", "tmux", "attach").as(bobUser).fg()
	client.exe, client.env = "/home/bob/bin/tmux", []string{"LD_PRELOAD=/home/bob/x.so"}
	tr.add(106, 103, "su", "su", "-").as("") // set-user-ID, started by bob
	tr.add(107, 106, "bash", "-bash")
	tr.add(108, 107, "codex", "codex").bg()
	m := &fakeMux{tmux: map[string][]tmuxClient{"": {{clientPID: 105, panePID: 107, paneID: "%0", session: "0"}}}}
	l := testLocator(m)
	l.owner = rootUser
	loc := l.locate(tr.snapshot())
	if got, want := summary(loc), "none|||codex[108]"; got != want || len(m.calls) != 0 {
		t.Fatalf("got %q, queries %q; want %q and none", got, m.calls, want)
	}
}

func TestLocateNestedMultiplexer(t *testing.T) {
	tr := sshTree()
	tr.add(104, 101, "tmux", "tmux", "attach").fg()
	tr.add(200, 1, "tmux: server", "tmux")
	tr.add(201, 200, "bash", "-bash")
	tr.add(202, 201, "zellij", "zellij", "-s", "inner").fg()
	tr.add(300, 1, "zellij", "zellij", "--server", "/tmp/zellij-1000/contract_version_1/inner")
	tr.add(301, 300, "bash", "/bin/bash").withEnv("ZELLIJ_PANE_ID=0")
	tr.add(302, 301, "claude", "claude").fg()
	m := &fakeMux{
		tmux:   map[string][]tmuxClient{"": {{clientPID: 104, panePID: 201, paneID: "%0", session: "0"}}},
		zellij: map[string][]zellijClient{"inner": {{id: "1", pane: "terminal_0"}}},
	}
	loc := testLocator(m).locate(tr.snapshot())
	if got, want := summary(loc), "tmux|0 %0||claude[302]@terminal_0*"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLocateReportsFailedQueries(t *testing.T) {
	tr := sshTree()
	tr.add(104, 101, "tmux", "tmux").fg()
	loc := testLocator(&fakeMux{}).locate(tr.snapshot())
	if got, want := summary(loc), "none|||"; got != want || !slices.Equal(loc.errs, []string{"no answer"}) {
		t.Fatalf("got %q errs %q", got, loc.errs)
	}
}

func TestTmuxSocket(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want []string
	}{
		{[]string{"tmux"}, nil},
		{[]string{"tmux", "attach", "-L", "x"}, nil}, // after the command: not a global option
		{[]string{"tmux", "-L", "work", "a"}, []string{"-L", "work"}},
		{[]string{"tmux", "-2uLwork", "a"}, []string{"-L", "work"}},
		{[]string{"tmux", "-f", "/dev/null", "-S", "/tmp/s", "-L", "x"}, []string{"-S", "/tmp/s"}},
		{[]string{"tmux", "-c", "-L x", "-L", "y"}, []string{"-L", "y"}},
	} {
		if got := tmuxSocket(c.argv); !slices.Equal(got, c.want) {
			t.Errorf("tmuxSocket(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

func TestZellijClientSession(t *testing.T) {
	for _, c := range []struct {
		argv    []string
		session string
		client  bool
	}{
		{[]string{"zellij"}, "", true},
		{[]string{"zellij", "-s", "work"}, "work", true},
		{[]string{"zellij", "--session=work", "--layout", "compact"}, "work", true},
		{[]string{"zellij", "-l", "attach", "options", "--default-shell", "fish"}, "", true},
		{[]string{"zellij", "attach", "work"}, "work", true},
		{[]string{"zellij", "a", "-c", "--index", "0", "work"}, "work", true},
		{[]string{"zellij", "--server", "/run/zellij/work"}, "", false},
		{[]string{"zellij", "--session", "work", "action", "list-clients"}, "", false},
		{[]string{"zellij", "ls"}, "", false},
	} {
		s, ok := zellijClientSession(c.argv)
		if s != c.session || ok != c.client {
			t.Errorf("zellijClientSession(%q) = %q, %v; want %q, %v", c.argv, s, ok, c.session, c.client)
		}
	}
}

func TestScreenClientSession(t *testing.T) {
	for _, c := range []struct {
		argv   []string
		name   string
		client bool
	}{
		{[]string{"screen"}, "", true},
		{[]string{"screen", "vim", "x"}, "", true},
		{[]string{"screen", "-r", "work"}, "work", true},
		{[]string{"screen", "-dr", "1234.pts-0.host"}, "1234.pts-0.host", true},
		{[]string{"screen", "-x"}, "", true},
		{[]string{"screen", "-S", "work"}, "work", true},
		{[]string{"screen", "-e", "^Aa", "-DR", "work"}, "work", true},
		{[]string{"screen", "-ls"}, "", false},
		{[]string{"screen", "-S", "work", "-Q", "number"}, "", false},
		{[]string{"screen", "-S", "work", "vim"}, "work", true},
		{[]string{"screen", "-X", "quit"}, "", false},
	} {
		n, ok := screenClientSession(c.argv)
		if n != c.name || ok != c.client {
			t.Errorf("screenClientSession(%q) = %q, %v; want %q, %v", c.argv, n, ok, c.name, c.client)
		}
	}
}

func TestHerdrClientSession(t *testing.T) {
	for _, c := range []struct {
		argv    []string
		session string
		client  bool
	}{
		{[]string{"herdr"}, "", true},
		{[]string{"herdr", "--session", "dev"}, "dev", true},
		{[]string{"herdr", "session", "attach", "dev"}, "dev", true},
		{[]string{"herdr", "server"}, "", false},
		{[]string{"herdr", "--session", "dev", "pane", "list"}, "", false},
		{[]string{"herdr", "--remote", "box"}, "", false},
	} {
		s, ok := herdrClientSession(c.argv)
		if s != c.session || ok != c.client {
			t.Errorf("herdrClientSession(%q) = %q, %v; want %q, %v", c.argv, s, ok, c.session, c.client)
		}
	}
}

func TestHarnessOf(t *testing.T) {
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"claude", []string{"claude"}, "claude"},
		{"2.1.285", []string{"claude", "--resume"}, "claude"},
		{"node", []string{"node", "--enable-source-maps", `C:\Users\u\AppData\Roaming\npm\node_modules\@anthropic-ai\claude-code\cli.js`}, "claude"},
		{"claude.exe", nil, "claude"},
		{"node", []string{"node", "/usr/lib/node_modules/@openai/codex/bin/codex.js", "resume"}, "codex"},
		{"codex-x86_64-u", []string{"/x/bin/codex-x86_64-unknown-linux-musl"}, "codex"},
		{"omp", []string{"bun", "/home/u/.bun/bin/omp"}, "omp"},
		{"bun", []string{"/home/u/.bun/bin/bun", "/home/u/.bun/install/global/node_modules/@oh-my-pi/pi-coding-agent/dist/cli.js"}, "omp"},
		{"node", []string{"node", "server.js"}, ""},
		{"vim", []string{"vim", "claude"}, ""},
		{"bash", []string{"bash", "/usr/local/bin/claude"}, ""},
	} {
		if got := harnessOf(c.name, c.argv); got != c.want {
			t.Errorf("harnessOf(%q, %q) = %q, want %q", c.name, c.argv, got, c.want)
		}
	}
}
