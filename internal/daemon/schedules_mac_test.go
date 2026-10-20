//go:build !windows

package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// onMac makes the daemon's host macOS for the test.
func onMac(t *testing.T, mac bool) {
	old := macHost
	macHost = mac
	t.Cleanup(func() { macHost = old })
}

// On a macOS host, a claude schedule warns when there is no
// ~/.claude/.credentials.json (CLAUDE_CONFIG_DIR's when set): claude then
// reads its login from the keychain, locked while nobody is logged in to
// the desktop. Other agents and other hosts do not warn.
func TestClaudeScheduleOnMacWarnsWithoutCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	onMac(t, true)
	e := startScheduleDaemon(t, time.Now(), nil, nil)
	cwd := t.TempDir()

	p := e.preview(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "x"))
	if !slices.Equal(warningCodes(p), []string{wire.WarnClaudeGUILogin}) || p.Warnings[0].Message == "" {
		t.Errorf("no credentials: %+v", p.Warnings)
	}
	for _, s := range []wire.Schedule{
		agentSchedule(cwd, wire.HarnessCodex, wire.PermissionRead, "x"),
		agentSchedule(cwd, wire.HarnessOmp, wire.PermissionRead, "x"),
		commandSchedule(cwd, "claude -p hi", wire.ScheduleWhen{Every: wire.EveryDaily, Time: "09:00"}),
	} {
		if p := e.preview(s); slices.Contains(warningCodes(p), wire.WarnClaudeGUILogin) {
			t.Errorf("%s %s: %+v", s.Kind, s.Harness, p.Warnings)
		}
	}

	creds := filepath.Join(home, ".claude", ".credentials.json")
	os.MkdirAll(filepath.Dir(creds), 0o700)
	os.WriteFile(creds, []byte(`{"claudeAiOauth":{}}`), 0o600)
	if p := e.preview(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "x")); len(p.Warnings) != 0 {
		t.Errorf("with credentials: %+v", p.Warnings)
	}

	config := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	if p := e.preview(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "x")); !slices.Contains(warningCodes(p), wire.WarnClaudeGUILogin) {
		t.Errorf("CLAUDE_CONFIG_DIR without credentials: %+v", p.Warnings)
	}
	os.WriteFile(filepath.Join(config, ".credentials.json"), []byte(`{}`), 0o600)
	if p := e.preview(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "x")); len(p.Warnings) != 0 {
		t.Errorf("CLAUDE_CONFIG_DIR with credentials: %+v", p.Warnings)
	}

	onMac(t, false)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if p := e.preview(agentSchedule(cwd, wire.HarnessClaude, wire.PermissionRead, "x")); len(p.Warnings) != 0 {
		t.Errorf("Linux: %+v", p.Warnings)
	}
}

// A run whose holder could not go through the GUI login session (macOS
// with nobody logged in to the desktop) is recorded as outside it from
// its start; when its claude could not log in, the run says so too.
func TestRunOutsideGUILoginIsRecorded(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 19, 23, 59, 0, 0, time.UTC)}
	spawns := make(chan spawned, 8)
	te := startDaemon(t, fastConfig(), Options{
		Now:          clock.Now,
		ScheduleTick: 10 * time.Millisecond,
		LoginEnv:     func() ([]string, error) { return nil, nil },
		SpawnHolder: func(exe string, args []string, dir string, env []string, logPath string) (int, bool, error) {
			spawns <- spawned{exe, args, dir, env}
			return 0, true, nil
		},
	})
	c, _ := te.client()
	e := &scheduleEnv{testEnv: te, clock: clock, spawns: spawns, c: c}
	id := e.create(agentSchedule(t.TempDir(), wire.HarnessClaude, wire.PermissionRead, "hi")).Schedule.ID

	clock.Set(time.Date(2026, 10, 20, 0, 0, 1, 0, time.UTC))
	sp := e.awaitSpawn()
	run := e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.OutsideGUI })
	if run.Result != wire.RunRunning {
		t.Errorf("started run %+v", run)
	}

	cmd := sp.command()
	result := cmd[slices.Index(cmd, "--")-1]
	b, _ := json.Marshal(map[string]any{"result": "failed", "error": "Not logged in · Please run /login", "auth_failed": true})
	if err := os.WriteFile(result, b, 0o600); err != nil {
		t.Fatal(err)
	}
	e.holderOf(sp).Notify(wire.MethodHolderEnded, wire.ClosedParams{ID: sp.flag("id"), ExitCode: 1})
	run = e.awaitRun(id, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning })
	if run.Result != wire.RunFailed || !run.OutsideGUI || !run.AuthFailed || run.Error != "Not logged in · Please run /login" {
		t.Errorf("ended run %+v", run)
	}
}
