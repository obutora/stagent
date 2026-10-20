package bridge

import (
	"runtime"

	"github.com/obutora/stagent/internal/loginenv"
	"github.com/obutora/stagent/internal/paths"
)

// shellCommand is what a `shell: true` session runs: the shell an SSH
// terminal of this account gets. sshd sets SHELL to its DefaultShell
// (cmd.exe when that is unset); %ComSpec% stands in when SHELL is empty.
// No "-l": Windows PowerShell 5.1 would run it as a command and exit.
func shellCommand(env []string) []string {
	sh := loginenv.Get(env, "SHELL")
	if sh == "" {
		sh = loginenv.Get(env, "ComSpec")
	}
	if sh == "" {
		sh = "cmd.exe"
	}
	return []string{sh}
}

// terminalCommand is what a task opened as a terminal runs (task.create
// without a command): cmd.exe (%ComSpec%), whatever sshd's DefaultShell.
func terminalCommand(env []string) []string {
	if sh := loginenv.Get(env, "ComSpec"); sh != "" {
		return []string{sh}
	}
	return []string{"cmd.exe"}
}

func toolDirs(home string, getenv func(string) string) []string {
	return paths.ToolDirs(runtime.GOOS, home, getenv)
}
