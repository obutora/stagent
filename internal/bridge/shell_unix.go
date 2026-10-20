//go:build !windows

package bridge

import (
	"runtime"

	"github.com/obutora/stagent/internal/loginenv"
	"github.com/obutora/stagent/internal/paths"
)

// shellCommand is what a `shell: true` session runs: the user's $SHELL as
// their login session sets it (/bin/sh when unset), as a login shell.
func shellCommand(env []string) []string {
	sh := loginenv.Get(env, "SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	return []string{sh, "-l"}
}

// terminalCommand is what a task opened as a terminal runs (task.create
// without a command): the login shell, as for `shell: true`.
func terminalCommand(env []string) []string { return shellCommand(env) }

func toolDirs(home string, getenv func(string) string) []string {
	return paths.ToolDirs(runtime.GOOS, home, getenv)
}
