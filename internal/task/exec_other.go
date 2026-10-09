//go:build !windows

package task

import (
	"context"
	"os"
	"os/exec"
	"syscall"
)

// scriptCommand is the command running plan p, its runner file at file.
func scriptCommand(ctx context.Context, p ScriptPlan, file string) *exec.Cmd {
	args := p.Args
	if file != "" {
		args = append(args[:len(args):len(args)], file)
	}
	return exec.CommandContext(ctx, p.Program, args...)
}

// execAgent replaces this process with the program at path (argv), so the
// session's program becomes the agent itself. It returns only on failure;
// started is not called (exec closes this process's files, the daemon
// connection included).
func execAgent(path string, argv []string, started func()) (int, error) {
	return ExitNotFound, syscall.Exec(path, argv, os.Environ())
}
