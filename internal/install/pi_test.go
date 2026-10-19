package install

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// Integrating Pi writes stagent's extension into Pi's agent directory;
// removing it deletes the file, and doctor reports Pi.
func TestPiExtension(t *testing.T) {
	te := newTestEnv(t, "linux")
	ext := te.home(".pi", "agent", "extensions", "ssh-term-stagent.ts")
	r := te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessPi}})
	if got := strings.Join(changeIDs(r.Changes), ","); got != "pi" {
		t.Fatalf("changes = %s", got)
	}
	if got := readFile(t, ext); got != piExtensionSource {
		t.Fatalf("extension:\n%s", got)
	}
	if h := piReport(t, te); !h.Integrated || h.ConfigPath != ext {
		t.Fatalf("doctor: %+v", h)
	}
	te.integrate(t, integrateOpts{apply: true, remove: []string{wire.HarnessPi}})
	if exists(ext) || exists(te.home(".pi")) {
		t.Fatalf("extension left behind: %v, ~/.pi %v", exists(ext), exists(te.home(".pi")))
	}
	if h := piReport(t, te); h.Integrated {
		t.Fatalf("doctor after remove: %+v", h)
	}
}

// PI_CODING_AGENT_DIR moves Pi's agent directory, ~ expanded as Pi does.
func TestPiExtensionHonorsAgentDir(t *testing.T) {
	for _, c := range []struct{ dir, want string }{
		{"custom", "custom"}, // made absolute below
		{"~/pi-agent", "pi-agent"},
	} {
		te := newTestEnv(t, "linux")
		dir, want := c.dir, te.home(c.want, "extensions", "ssh-term-stagent.ts")
		if c.dir == "custom" {
			dir = filepath.Join(t.TempDir(), "custom")
			want = filepath.Join(dir, "extensions", "ssh-term-stagent.ts")
		}
		te.vars["PI_CODING_AGENT_DIR"] = dir
		te.integrate(t, integrateOpts{apply: true, harness: []string{wire.HarnessPi}})
		if !exists(want) {
			t.Errorf("PI_CODING_AGENT_DIR=%s: no extension at %s", c.dir, want)
		}
		if exists(te.home(".pi")) {
			t.Errorf("PI_CODING_AGENT_DIR=%s: ~/.pi written", c.dir)
		}
	}
}

// A file of the user's at the extension's path is left alone.
func TestPiExtensionLeavesUsersFile(t *testing.T) {
	te := newTestEnv(t, "linux")
	ext := te.home(".pi", "agent", "extensions", "ssh-term-stagent.ts")
	writeFile(t, ext, "export default function () {}\n")
	te.reload()
	r, err := te.env.integrate(integrateOpts{apply: true, harness: []string{wire.HarnessPi}})
	if err != nil || len(r.Changes) != 1 || r.Changes[0].ErrorCode != codeUnmanagedFile || readFile(t, ext) != "export default function () {}\n" {
		t.Fatalf("changes %+v, %v", r.Changes, err)
	}
}

// The extension reports through Pi's own events: session_start for every
// start (Pi has no session_switch), before_agent_start for a prompt, agent_settled for
// the end of a turn (agent_end may be followed by an automatic
// continuation), session_shutdown; and it stays silent in a Pi started by
// Pi's bash tool or run without its terminal UI.
func TestPiExtensionSource(t *testing.T) {
	for _, want := range []string{
		`pi.on("session_start"`, `pi.on("before_agent_start"`, `pi.on("agent_settled"`, `pi.on("session_shutdown"`,
		`process.env.PI_SESSION_ID`, piHeadlessSource, `headless(process.argv.slice(2))`,
		`["hook", "pi"]`, `argv: process.argv.slice(2)`,
	} {
		if !strings.Contains(piExtensionSource, want) {
			t.Errorf("extension lacks %s", want)
		}
	}
	for _, unwanted := range []string{"session_switch", "willContinue", "OMPCODE", `"hook", "omp"`} {
		if strings.Contains(piExtensionSource, unwanted) {
			t.Errorf("extension uses omp's %s", unwanted)
		}
	}
	if !strings.HasPrefix(piExtensionSource, extensionMarker+"\n") {
		t.Error("extension does not start with the marker")
	}
}

// The extension tells a Pi without its terminal UI by its arguments, as
// Pi reports a UI in rpc mode: -p / --print, --mode json / rpc in both
// forms. Run with node when it is installed.
func TestPiExtensionHeadless(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	cases := []struct {
		argv []string
		want bool
	}{
		{nil, false},
		{[]string{"fix the bug"}, false},
		{[]string{"-p", "hi"}, true},
		{[]string{"--print"}, true},
		{[]string{"--model", "sonnet", "-p"}, true},
		{[]string{"--mode", "rpc"}, true},
		{[]string{"--mode=rpc"}, true},
		{[]string{"--mode", "json", "hi"}, true},
		{[]string{"--mode=json"}, true},
		{[]string{"--mode", "text"}, false},
		{[]string{"--mode=text"}, false},
		{[]string{"--mode"}, false},
		{[]string{"--session", "rpc"}, false},
		{[]string{"--continue", "--name", "print"}, false},
	}
	var argvs [][]string
	for _, c := range cases {
		argvs = append(argvs, append([]string{}, c.argv...))
	}
	in, _ := json.Marshal(argvs)
	script := piHeadlessSource + "\nconsole.log(JSON.stringify(" + string(in) + ".map(headless)));\n"
	out, err := exec.Command(node, "-e", script).Output()
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	var got []bool
	if err := json.Unmarshal(out, &got); err != nil || len(got) != len(cases) {
		t.Fatalf("node printed %q (%v)", out, err)
	}
	for i, c := range cases {
		if got[i] != c.want {
			t.Errorf("headless(%q) = %v, want %v", c.argv, got[i], c.want)
		}
	}
}

func piReport(t *testing.T, te *testEnv) HarnessReport {
	t.Helper()
	for _, h := range te.doctor().Harnesses {
		if h.ID == wire.HarnessPi {
			return h
		}
	}
	t.Fatal("doctor has no pi")
	return HarnessReport{}
}
