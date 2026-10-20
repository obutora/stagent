package daemon

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/obutora/stagent/internal/wire"
)

// macOS keeps claude's login in the login keychain, which only processes
// of the GUI login session can read. A run's holder goes there through
// gui/<uid> (proc.StartHolder); with nobody logged in to the desktop it
// is started outside it, and claude can log in only from
// ~/.claude/.credentials.json (written by /login run outside the GUI).

// macHost: the host is macOS (tests change it).
var macHost = runtime.GOOS == "darwin"

// previewGUILogin warns of a claude schedule on a macOS host without
// claude's credentials file: its runs fail while nobody is logged in to
// the desktop.
func previewGUILogin(res *wire.SchedulePreview, s wire.Schedule) {
	if !macHost || s.Kind != wire.ScheduleKindAgent || s.Harness != wire.HarnessClaude {
		return
	}
	creds := filepath.Join(claudeConfigDir(), ".credentials.json")
	if _, err := os.Stat(creds); err == nil {
		return
	}
	res.Warnings = append(res.Warnings, wire.ScheduleWarning{Code: wire.WarnClaudeGUILogin,
		Message: "there is no " + creds + ": claude reads its login from the keychain, which runs cannot while nobody is logged in to this Mac's desktop"})
}

// claudeConfigDir is where claude keeps its files: $CLAUDE_CONFIG_DIR, or
// ~/.claude.
func claudeConfigDir() string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("HOME"), ".claude")
}

// runOutsideGUILocked records that pending run p's holder was started
// outside the GUI login session.
func (d *Daemon) runOutsideGUILocked(p *pendingRun) {
	p.run.OutsideGUI = true
	d.logf("daemon: schedule %s: run %s started outside the GUI login session", p.scheduleID, p.run.RunID)
	if err := d.runs.Update(p.scheduleID, p.run); err != nil {
		d.logf("daemon: schedule %s runs: %v", p.scheduleID, err)
	}
	d.scheduleIDChangedLocked(p.scheduleID)
}
