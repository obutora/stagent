package bridge

import (
	"os/exec"

	"github.com/obutora/stagent/internal/logind"
)

// holderScope returns the argv prefix that starts detached holders in a
// transient scope of the user's service manager, or nil for a plain start.
//
// With logind's KillUserProcesses=yes every process of a login session is
// killed when the session ends, and the bridge runs in one (the app's SSH
// connection): plainly started holders would take every detached session
// down at logout. systemd-run --user --scope moves the holder out of the
// session; it runs the command itself, so the pid is the holder's.
func holderScope() []string {
	if kill, known := logind.KillUserProcesses(); !known || !kill {
		return nil
	}
	run, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil
	}
	return []string{run, "--user", "--scope", "--collect", "--quiet", "--"}
}
