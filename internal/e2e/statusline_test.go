package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// statusLineHost is an isolated host with stagent installed at its usual
// place and Claude's settings.json holding settings (none when "").
func statusLineHost(t *testing.T, settings string) *paths.Layout {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("runs the relay through sh")
	}
	bin := compileStagent(t)
	home := t.TempDir()
	t.Setenv(paths.EnvHome, home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(daemonclient.EnvExe, l.Bin)
	if err := os.MkdirAll(filepath.Dir(l.Bin), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(bin)
	if err == nil {
		err = os.WriteFile(l.Bin, b, 0o700)
	}
	if err != nil {
		t.Fatal(err)
	}
	if settings != "" {
		settingsPath := filepath.Join(home, ".claude", "settings.json")
		os.MkdirAll(filepath.Dir(settingsPath), 0o700)
		if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"install", "--json"}, {"integrate", "--apply", "--statusline", "--json"}} {
		if out, err := exec.Command(l.Bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("stagent %v: %v\n%s", args, err, out)
		}
	}
	t.Cleanup(func() { shutdownDaemon(l) })
	return l
}

// statusLineCommand is the statusLine command Claude Code runs on the host.
func statusLineCommand(t *testing.T, l *paths.Layout) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.Home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		StatusLine struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	if err := json.Unmarshal(b, &s); err != nil || s.StatusLine.Type != "command" || !strings.Contains(s.StatusLine.Command, "statusline claude") {
		t.Fatalf("statusLine not relayed (%v):\n%s", err, b)
	}
	return s.StatusLine.Command
}

// runShell runs command as Claude Code does (sh -c, input on stdin) and
// returns its stdout and exit code.
func runShell(t *testing.T, command, input string) (string, int, time.Duration) {
	t.Helper()
	cmd := exec.Command("sh", "-c", command)
	cmd.Stdin = strings.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	start := time.Now()
	err := cmd.Run()
	d := time.Since(start)
	code := 0
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), code, d
}

func statusInput(session string, fiveHour float64) string {
	return fmt.Sprintf(`{"session_id":%q,"transcript_path":"/t/%s.jsonl","model":{"display_name":"Opus"},"rate_limits":{"five_hour":{"used_percentage":%g,"resets_at":%d},"seven_day":{"used_percentage":12,"resets_at":%d}}}`,
		session, session, fiveHour, time.Now().Add(3*time.Hour).Unix(), time.Now().Add(72*time.Hour).Unix())
}

// startDaemon runs `stagent daemon` for l until the test ends.
func startStatusDaemon(t *testing.T, l *paths.Layout) {
	t.Helper()
	cmd := exec.Command(l.Bin, "daemon")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdownDaemon(l)
		cmd.Wait()
	})
	deadline := time.Now().Add(waitLong)
	for {
		if c, err := ipc.Dial(l.DaemonAddr, time.Second); err == nil {
			c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not come up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fiveHour is the 5-hour share usage.get returns for ~/.claude (-1: none).
func fiveHour(t *testing.T, l *paths.Layout) float64 {
	t.Helper()
	conn, err := ipc.Dial(l.DaemonAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r wire.UsageGetResult
	if err := c.Call(ctx, wire.MethodUsageGet, wire.UsageGetParams{}, &r); err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Limits {
		if a.ConfigDir != filepath.Join(l.Home, ".claude") {
			continue
		}
		for _, w := range a.Windows {
			if w.Window == wire.UsageWindowFiveHour && w.UsedPercent != nil {
				return *w.UsedPercent
			}
		}
	}
	return -1
}

func awaitFiveHour(t *testing.T, l *paths.Layout, want float64) {
	t.Helper()
	deadline := time.Now().Add(waitLong)
	for {
		got := fiveHour(t, l)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("five_hour = %g, want %g", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The relay installed by `integrate --statusline` runs the user's own
// statusLine with the same input and returns its output and exit code,
// whether the daemon runs or not and whatever the input; it sends the
// limit windows to the daemon at most every 15 s per session.
func TestStatusLineRelay(t *testing.T) {
	const mine = `printf 'mine %s %s' "$(wc -c | tr -d ' ')" "$SL_MARK"; exit 3`
	settings := `{
  "model": "opus",
  "statusLine": {"type": "command", "command": ` + string(jsonString(mine)) + `, "padding": 1}
}
`
	l := statusLineHost(t, settings)
	command := statusLineCommand(t, l)
	t.Setenv("SL_MARK", "env-kept")

	in := statusInput("s1", 42)
	want := fmt.Sprintf("mine %d env-kept", len(in))
	// Daemon down; then input that is not JSON.
	for _, input := range []string{in, "not json{"} {
		out, code, _ := runShell(t, command, input)
		if w := fmt.Sprintf("mine %d env-kept", len(input)); out != w || code != 3 {
			t.Fatalf("relay with input %.12q: %q exit %d, want %q exit 3", input, out, code, w)
		}
	}

	startStatusDaemon(t, l)
	// The mark of the run while the daemon was down holds s1 back: clear it.
	backdateMarks(t, l)
	if out, code, _ := runShell(t, command, in); out != want || code != 3 {
		t.Fatalf("relay with the daemon up: %q exit %d", out, code)
	}
	awaitFiveHour(t, l, 42)

	// Within 15 s the same session sends nothing; another session does.
	runShell(t, command, statusInput("s1", 50))
	time.Sleep(300 * time.Millisecond)
	if got := fiveHour(t, l); got != 42 {
		t.Fatalf("second report within 15 s arrived: %g", got)
	}
	runShell(t, command, statusInput("s2", 55))
	awaitFiveHour(t, l, 55)

	// 15 s later the session sends again.
	backdateMarks(t, l)
	runShell(t, command, statusInput("s1", 60))
	awaitFiveHour(t, l, 60)
}

// Without a statusLine of the user's, the relay prints nothing (Claude
// Code shows its default) and exits 0.
func TestStatusLineRelayWithoutOriginal(t *testing.T) {
	l := statusLineHost(t, "")
	out, code, _ := runShell(t, statusLineCommand(t, l), statusInput("s1", 1))
	if out != "" || code != 0 {
		t.Fatalf("relay without an original: %q exit %d", out, code)
	}
}

// What the relay adds to the user's statusLine, with the daemon down and
// up, stays within 150 ms (median of several runs).
func TestStatusLineRelayOverhead(t *testing.T) {
	const mine = `cat >/dev/null; echo ok`
	l := statusLineHost(t, `{"statusLine": {"type": "command", "command": "cat >/dev/null; echo ok"}}`)
	command := statusLineCommand(t, l)
	const runs = 15
	median := func(command string, session func(i int) string) time.Duration {
		var ds []time.Duration
		for i := range runs {
			out, _, d := runShell(t, command, statusInput(session(i), 1))
			if out != "ok\n" {
				t.Fatalf("%q printed %q", command, out)
			}
			ds = append(ds, d)
		}
		slices.Sort(ds)
		return ds[runs/2]
	}
	fresh := func(i int) string { return fmt.Sprintf("session-%d-%d", time.Now().UnixNano(), i) }
	alone := median(mine, fresh)
	down := median(command, fresh)
	startStatusDaemon(t, l)
	up := median(command, fresh)
	t.Logf("statusLine median of %d runs: original alone %v; through the relay, daemon down %v (+%v), daemon up %v (+%v)", runs, alone, down, down-alone, up, up-alone)
	for name, d := range map[string]time.Duration{"daemon down": down - alone, "daemon up": up - alone} {
		if d > 150*time.Millisecond {
			t.Errorf("relay adds %v with the %s, want ≤ 150 ms", d, name)
		}
	}
}

// backdateMarks makes every session's last report 16 s old.
func backdateMarks(t *testing.T, l *paths.Layout) {
	t.Helper()
	marks, _ := filepath.Glob(filepath.Join(l.StatusLineMarks, "*"))
	old := time.Now().Add(-16 * time.Second)
	for _, m := range marks {
		if err := os.Chtimes(m, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func jsonString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
