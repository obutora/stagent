package task

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// Exit codes of `stagent task`.
const (
	ExitFailure  = 1
	ExitUsage    = 2
	ExitNotFound = 127
)

const usage = `usage: stagent task run [--switch-branch] [--setup] <id> -- <command…>
       stagent task remove [--force] <id>

Internal: the bridge's task.create runs it as a task's first session.
It prepares task <id> (fetch, branch, worktree, copy, setup), reporting each
stage to the daemon (task.progress), then runs <command…> in its place.

The bridge's task.remove runs remove in a session of the source checkout
when the task has scripts.archive, after stopping the task's sessions:
archive, unlink the shared-directory links, git worktree remove (--force:
with changes), delete the branch, then the record (task.forget).
`

// daemonWait bounds starting the daemon and each call to it.
const daemonWait = 10 * time.Second

// Main is `stagent task`.
func Main(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "run":
			return runMain(args[1:])
		case "remove":
			return removeMain(args[1:])
		}
	}
	fmt.Fprint(os.Stderr, usage)
	return ExitUsage
}

// runMain is `stagent task run`: it prepares the task (Prepare) and then
// becomes its agent — exec on Linux and macOS, a child on Windows. A
// failure is printed for the terminal's scrollback, reported to the daemon
// (the task becomes failed) and ends with a non-zero exit code.
func runMain(args []string) int {
	fs := flag.NewFlagSet("stagent task run", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	switchBranch := fs.Bool("switch-branch", false, "in_place: switch the checkout to the task's branch")
	setup := fs.Bool("setup", false, "in_place: run orca.yaml's scripts.setup")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	rest := fs.Args()
	if len(rest) < 3 || rest[1] != "--" {
		fs.Usage()
		return ExitUsage
	}
	id, command := rest[0], rest[2:]

	d, t, err := dialTask(id)
	if err != nil {
		return fail("%v", err)
	}
	defer d.Close()
	progress := d.progress

	// ^C reaches git, gh, the setup script (and on Windows the agent) on
	// the terminal; this process stays to report what it ended.
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	// The app's patterns of files to copy come from config.json, read here
	// as holders read their settings (only the daemon writes it).
	err = Prepare(context.Background(), Prep{
		Task: *t, SwitchBranch: *switchBranch, Setup: *setup,
		WorktreeInclude: SavedPatterns(d.layout.Config, t.Repo),
		Stdin:           os.Stdin, Out: os.Stdout,
		Stage: func(stage string) { progress(stage, "") },
	})
	if err != nil {
		var se *StageError
		stage := wire.TaskStageFetch
		if errors.As(err, &se) {
			stage = se.Stage
		}
		progress(stage, err.Error())
		return fail("%v", err)
	}
	fmt.Fprintf(os.Stdout, "\n[stagent] %s\n", wire.TaskStageAgent)
	path, err := exec.LookPath(command[0])
	if err != nil {
		progress(wire.TaskStageAgent, err.Error())
		fail("%v", err)
		return ExitNotFound
	}
	progress(wire.TaskStageAgent, "")
	// The connection stays until the agent runs (POSIX: closed on exec;
	// Windows: once the child started) so that a failure to start it still
	// fails the task, as this prepared session.
	code, err := execAgent(path, command, func() { d.Close() })
	if err != nil {
		fmt.Fprintf(os.Stderr, "[stagent] %s: %v\n", command[0], err)
		if serr := d.call(wire.MethodTaskProgress, wire.TaskProgressParams{
			ID: d.id, Stage: wire.TaskStageAgent, Error: fmt.Sprintf("%s: %v", command[0], err),
			SessionID: os.Getenv(wire.EnvSessionID),
		}, nil); serr != nil {
			fmt.Fprintf(os.Stderr, "[stagent] task.progress %s: %v\n", wire.TaskStageAgent, serr)
		}
	}
	return code
}

// removeMain is `stagent task remove`: the removal (Remove) with archive,
// in a session whose cwd is the source checkout (Windows cannot remove a
// worktree that is a process's cwd), then task.forget naming the branch
// it kept. The bridge has stopped the task's sessions and reported the
// stop stage. A failure is printed, reported (the task goes back to where
// it was, keeping the error) and ends with a non-zero exit code.
func removeMain(args []string) int {
	fs := flag.NewFlagSet("stagent task remove", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(fs.Output(), usage) }
	force := fs.Bool("force", false, "remove the worktree with its changes")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return ExitUsage
	}
	d, t, err := dialTask(fs.Arg(0))
	if err != nil {
		return fail("%v", err)
	}
	defer d.Close()
	signal.Notify(make(chan os.Signal, 1), os.Interrupt)
	kept, err := Remove(context.Background(), Removal{
		Task: *t, Archive: true, Force: *force,
		Stdin: os.Stdin, Out: os.Stdout,
		Stage: func(stage string) { d.progress(stage, "") },
	})
	if err != nil {
		var se *StageError
		stage := wire.TaskStageArchive
		if errors.As(err, &se) {
			stage = se.Stage
		}
		d.progress(stage, err.Error())
		return fail("%v", err)
	}
	if err := d.call(wire.MethodTaskForget, wire.TaskRemoved{ID: t.ID, KeptBranch: kept}, nil); err != nil {
		return fail("task.forget: %v", err)
	}
	if kept != "" {
		fmt.Fprintf(os.Stdout, "\n[stagent] kept branch %s\n", kept)
	}
	fmt.Fprintf(os.Stdout, "\n[stagent] removed\n")
	return 0
}

// taskDaemon is a daemon connection of `stagent task`.
type taskDaemon struct {
	*rpc.Client
	id     string
	layout *paths.Layout
}

// dialTask connects to the daemon (starting it if needed) and finds task
// id.
func dialTask(id string) (*taskDaemon, *wire.Task, error) {
	l, err := paths.Resolve()
	if err != nil {
		return nil, nil, err
	}
	conn, err := daemonclient.DialOrStart(l, daemonWait)
	if err != nil {
		return nil, nil, fmt.Errorf("daemon: %w", err)
	}
	d := &taskDaemon{Client: rpc.NewClient(conn, nil), id: id, layout: l}
	var list wire.TaskListResult
	if err := d.call(wire.MethodTaskList, struct{}{}, &list); err != nil {
		d.Close()
		return nil, nil, fmt.Errorf("task.list: %w", err)
	}
	for i := range list.Tasks {
		if list.Tasks[i].ID == id {
			return d, &list.Tasks[i], nil
		}
	}
	d.Close()
	return nil, nil, fmt.Errorf("no task %s", id)
}

func (d *taskDaemon) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonWait)
	defer cancel()
	return d.Call(ctx, method, params, result)
}

// progress reports a stage (task.progress); failing to is only printed.
func (d *taskDaemon) progress(stage, msg string) {
	if err := d.call(wire.MethodTaskProgress, wire.TaskProgressParams{ID: d.id, Stage: stage, Error: msg}, nil); err != nil {
		fmt.Fprintf(os.Stderr, "[stagent] task.progress %s: %v\n", stage, err)
	}
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "\n[stagent] failed: "+format+"\n", args...)
	return ExitFailure
}
