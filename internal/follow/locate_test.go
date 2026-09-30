package follow

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// summary renders a location compactly: "mux|detail|flags|agents".
func summary(l location) string {
	var agents []string
	for _, c := range l.agents {
		s := fmt.Sprintf("%s%v", c.harness, c.pids)
		if c.pane != "" {
			s += "@" + c.pane
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
