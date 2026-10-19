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
