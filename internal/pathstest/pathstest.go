// Package pathstest isolates a stagent installation for tests.
package pathstest

import (
	"os"
	"testing"

	"github.com/obutora/stagent/internal/paths"
)

// SetHome points STAGENT_HOME at home for the rest of the test and returns
// the resolved layout. A temporary home is too long for socket paths, so
// RunDir lands in the shared tmp (stagent-<uid>-<hash of home>) where
// t.TempDir's cleanup never reaches; SetHome removes it when the test ends.
// The hash makes the directory this home's own, and daemons or holders the
// test started (also subprocesses given the same STAGENT_HOME) use the same
// one. Call it after creating home so the removal runs before home's.
func SetHome(t testing.TB, home string) *paths.Layout {
	t.Helper()
	t.Setenv(paths.EnvHome, home)
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	run := l.RunDir // the caller may change l
	t.Cleanup(func() { os.RemoveAll(run) })
	return l
}
