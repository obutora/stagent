// Package logind asks systemd-logind whether processes a user leaves
// running survive the end of the login session they were started from.
//
// With KillUserProcesses=yes (logind.conf) logind kills every process of a
// session's scope when the session ends, which would take detached holders
// spawned by the bridge (an SSH session) with it. Processes moved into a
// scope of the user's service manager (systemd-run --user --scope) leave the
// session; they still end with the service manager at the user's last
// logout unless lingering is enabled.
package logind

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// probeTimeout bounds one busctl call; logind may be slow or absent.
const probeTimeout = 2 * time.Second

// KillUserProcessesCmd reads logind's KillUserProcesses setting; the output
// is parsed with ParseBusctlBool.
var KillUserProcessesCmd = []string{
	"busctl", "get-property", "org.freedesktop.login1", "/org/freedesktop/login1",
	"org.freedesktop.login1.Manager", "KillUserProcesses",
}

// ParseBusctlBool parses a boolean property as printed by busctl
// get-property ("b true"). ok is false for any other output.
func ParseBusctlBool(out string) (v, ok bool) {
	switch strings.TrimSpace(out) {
	case "b true":
		return true, true
	case "b false":
		return false, true
	}
	return false, false
}

// KillUserProcesses reports logind's KillUserProcesses setting. known is
// false off Linux and when logind cannot be asked.
func KillUserProcesses() (kill, known bool) {
	if runtime.GOOS != "linux" {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, KillUserProcessesCmd[0], KillUserProcessesCmd[1:]...).Output()
	if err != nil {
		return false, false
	}
	return ParseBusctlBool(string(out))
}
