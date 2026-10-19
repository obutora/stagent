package holder

import (
	"testing"

	"github.com/obutora/stagent/internal/harness"
)

// Every harness of the registry says how it draws its menus (menuStyles),
// even when it draws none.
func TestEveryHarnessHasMenuStyle(t *testing.T) {
	for _, id := range harness.Missing(menuStyles) {
		t.Errorf("harness %s has no entry in menuStyles (internal/holder/prompt_%s.go)", id, id)
	}
}
