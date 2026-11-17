package holder

import (
	"testing"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/screenrules"
)

// builtinRules are the screen rules stagent ships; the tests of the menu
// and question readers read screens with them.
var builtinRules = newScreenRules(screenrules.Builtin())

func permissionMenu(lines []string, harnessID string) (string, bool) {
	return builtinRules.permissionMenu(lines, harnessID)
}

func questionMenu(lines []string, harnessID string) (string, bool) {
	return builtinRules.questionMenu(lines, harnessID)
}

func questionShown(lines []string, harnessID string) bool {
	return builtinRules.questionShown(lines, harnessID)
}

func liveMenu(lines []string) ([]promptItem, bool) {
	return builtinRules.liveMenu(lines)
}

// Every harness of the registry has a menu style, even when it draws no
// menu.
func TestEveryHarnessHasMenuStyle(t *testing.T) {
	for _, id := range harness.Missing(builtinRules.styles) {
		t.Errorf("harness %s has no menu style (internal/screenrules/builtin/%s.json)", id, id)
	}
}
