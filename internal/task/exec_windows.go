package task

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/obutora/stagent/internal/pty"
)

// scriptCommand is the command running plan p, its runner file at file. A
// .cmd runner gets its command line as cmd.exe wants it: /s /c with the
// quoted path quoted once more.
func scriptCommand(ctx context.Context, p ScriptPlan, file string) *exec.Cmd {
	if p.RunnerExt == ".cmd" {
		cmd := exec.CommandContext(ctx, p.Program)
		line := syscall.EscapeArg(p.Program) + " " + strings.Join(p.Args, " ") + ` ""` + file + `""`
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
		return cmd
	}
	args := p.Args
	if file != "" {
		args = append(args[:len(args):len(args)], file)
	}
	return exec.CommandContext(ctx, p.Program, args...)
}

// execAgent runs the program at path (argv) as a child on this console
// and returns its exit code: Windows has no exec. Batch files (npm's
// claude.cmd …) go through cmd.exe as the holder starts them. started is
// called once the child runs.
func execAgent(path string, argv []string, started func()) (int, error) {
	app, line, err := pty.CommandLine(path, argv[1:])
	if err != nil {
		return ExitFailure, err
	}
	cmd := exec.Command(app)
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: line}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return ExitFailure, err
	}
	started()
	err = cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return ExitFailure, err
	}
	return 0, nil
}
