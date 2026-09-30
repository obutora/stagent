package bridge

import (
	"errors"
	"runtime"

	"github.com/obutora/stagent/internal/paths"
)

// captureLoginEnv: Windows has no login shell; an sshd session already
// carries the user's registry environment (PATH included).
func captureLoginEnv(string) ([]string, error) {
	return nil, errors.New("no login shell on windows")
}

func toolDirs(home string, getenv func(string) string) []string {
	return paths.ToolDirs(runtime.GOOS, home, getenv)
}
