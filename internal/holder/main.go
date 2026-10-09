package holder

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/bootstrap"
	"github.com/obutora/stagent/internal/logind"
	"github.com/obutora/stagent/internal/paths"
)

// memoryLimit keeps a holder's heap small next to the agent it wraps unless
// the user set GOMEMLIMIT.
const memoryLimit = 64 << 20

// EnvHandoff decides --handoff=auto when it is exactly "0" (no handoff) or
// "1" (handoff); other values are ignored.
const EnvHandoff = "STAGENT_HANDOFF"

const runUsage = `usage: stagent run [--detached | --handoff[=auto]] [--id ID] [--cols N --rows N] [--cwd DIR] [--task ID] -- <cmd> [args...]

Runs <cmd> on a PTY as an agent session the SSH Term app can view and
control. Without --detached the session is mirrored on this terminal and
follows its size; with --detached it runs without a terminal and the app
owns the size. With --handoff a mirrored session outlives this terminal:
when it hangs up, the session continues detached instead of ending.
--handoff=auto (what the shell wrappers pass) hands off unless
STAGENT_HANDOFF=0, or config.json has "disable_handoff": true and
STAGENT_HANDOFF is not 1.

`

// handoffFlag is --handoff (on) or --handoff=auto (decided at start by
// runFlags.wantHandoff).
type handoffFlag struct{ on, auto bool }

func (f *handoffFlag) IsBoolFlag() bool { return true }

func (f *handoffFlag) String() string {
	if f.auto {
		return "auto"
	}
	return strconv.FormatBool(f.on)
}

func (f *handoffFlag) Set(s string) error {
	if s == "auto" {
		f.on, f.auto = false, true
		return nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return errors.New("want true, false or auto")
	}
	f.on, f.auto = v, false
	return nil
}

// runFlags are the options of `stagent run`.
type runFlags struct {
	detached   bool
	handoff    handoffFlag
	id, cwd    string
	cols, rows int
	task       string
}

func (r *runFlags) flagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("stagent run", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), runUsage)
		fs.PrintDefaults()
	}
	fs.BoolVar(&r.detached, "detached", false, "run without a local terminal; the app owns the size")
	fs.Var(&r.handoff, "handoff", "when this terminal hangs up, keep the session running detached; --handoff=auto: unless STAGENT_HANDOFF or disable_handoff in config.json says no")
	fs.StringVar(&r.id, "id", "", "session id: 16 lowercase hex characters (default: random)")
	fs.IntVar(&r.cols, "cols", 0, "columns of a detached session (default 80)")
	fs.IntVar(&r.rows, "rows", 0, "rows of a detached session (default 24)")
	fs.StringVar(&r.cwd, "cwd", "", "working directory of the program (default: current)")
	fs.StringVar(&r.task, "task", "", "task the session belongs to (session.spawn task_id); the daemon decides")
	return fs
}

// wantHandoff resolves the handoff choice. --handoff=auto is decided at
// every start: STAGENT_HANDOFF "0" / "1", else config.json's
// disable_handoff, else handoff.
func (r *runFlags) wantHandoff(l *paths.Layout) bool {
	if !r.handoff.auto {
		return r.handoff.on
	}
	switch os.Getenv(EnvHandoff) {
	case "0":
		return false
	case "1":
		return true
	}
	return !loadConfig(l).DisableHandoff
}

// recordWrapperRun stores when a shell wrapper (the only caller passing
// --handoff=auto) last started a program, where esc left this process
// (Linux) and whether bs makes it outlive the user's logout (macOS), for
// `stagent doctor` (shell_wrapper.last_run_at, last_run_survives_logout,
// last_run_bootstrap_error).
func recordWrapperRun(l *paths.Layout, esc logind.Result, bs bootstrap.Result) {
	rec := paths.WrapperRunRecord{At: time.Now().UnixMilli(), Placement: logind.Place(esc.Cgroup).String(), BootstrapError: bs.Err}
	if v, known := bs.SurvivesLogout(); known {
		rec.SurvivesLogout = &v
	}
	l.WriteWrapperRun(rec)
}

// Main is the `stagent run` entry point. It returns the program's exit code.
func Main(args []string) int {
	var rf runFlags
	fs := rf.flagSet()
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	command := fs.Args()
	if len(command) == 0 {
		fs.Usage()
		return ExitUsage
	}
	if (rf.handoff.on || rf.handoff.auto) && rf.detached {
		fmt.Fprintln(os.Stderr, "stagent run: --handoff applies to passthrough sessions only (not with --detached)")
		return ExitUsage
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	layout, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent run: %v\n", err)
		return ExitFailure
	}
	// Leave the login session (Linux) or its bootstrap namespace (macOS)
	// before anything else starts (the daemon, the program), so that they
	// start where this process ends up too.
	esc := logind.Escape("stagent-run")
	bs := bootstrap.Swap()
	if bs.Err != "" && rf.detached {
		// A detached holder's stderr is its log file; a terminal's is the
		// user's screen, so a passthrough start only records it.
		fmt.Fprintf(os.Stderr, "stagent run: per-user bootstrap port: %s (programs started here cannot resolve host names after you log out)\n", bs.Err)
	}
	if rf.handoff.auto {
		recordWrapperRun(layout, esc, bs)
	}
	code, err := Run(context.Background(), Options{
		Command:  command,
		Detached: rf.detached,
		Handoff:  rf.wantHandoff(layout),
		ID:       rf.id,
		Cols:     rf.cols,
		Rows:     rf.rows,
		Dir:      rf.cwd,
		TaskID:   rf.task,
		Layout:   layout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent run: %v\n", err)
	}
	return code
}
