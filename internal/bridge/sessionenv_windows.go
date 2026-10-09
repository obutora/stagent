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

// shellCommand is what a `shell: true` session runs: the shell an SSH
// terminal of this account gets. sshd sets SHELL to its DefaultShell
// (cmd.exe when that is unset); %ComSpec% stands in when SHELL is empty.
// No "-l": Windows PowerShell 5.1 would run it as a command and exit.
func shellCommand(env []string) []string {
	sh := envGet(env, "SHELL")
	if sh == "" {
		sh = envGet(env, "ComSpec")
	}
	if sh == "" {
		sh = "cmd.exe"
	}
	return []string{sh}
}

// terminalCommand is what a task opened as a terminal runs (task.create
// without a command): cmd.exe (%ComSpec%), whatever sshd's DefaultShell.
func terminalCommand(env []string) []string {
	if sh := envGet(env, "ComSpec"); sh != "" {
		return []string{sh}
	}
	return []string{"cmd.exe"}
}

func toolDirs(home string, getenv func(string) string) []string {
	return paths.ToolDirs(runtime.GOOS, home, getenv)
}
