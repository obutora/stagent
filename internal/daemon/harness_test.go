package daemon

import (
	"testing"

	"github.com/obutora/stagent/internal/harness"
)

// Every harness of the registry says how the daemon treats its hooks
// (daemonHarnesses), even when it does nothing of its own.
func TestEveryHarnessHasDaemonHandling(t *testing.T) {
	for _, id := range harness.Missing(daemonHarnesses) {
		t.Errorf("harness %s has no entry in daemonHarnesses (internal/daemon/%s.go)", id, id)
	}
}

// A harness that checks a tool's permission before its menu names the
// tool in its checkHook, which the daemon then takes as that check (not
// as an approval).
func TestCheckHookIsAPermissionCheck(t *testing.T) {
	for id, dh := range daemonHarnesses {
		if dh.checkHook == "" {
			continue
		}
		eff := classifyHook(id, dh.checkHook, hookPayload{ToolName: "run_terminal_command"})
		if !eff.permissionCheck || eff.approval {
			t.Errorf("%s: its checkHook %s gives %+v", id, dh.checkHook, eff)
		}
	}
}
