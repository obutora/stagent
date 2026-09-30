package holder

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/debug"

	"github.com/obutora/stagent/internal/paths"
)

// memoryLimit keeps a holder's heap small next to the agent it wraps unless
// the user set GOMEMLIMIT.
const memoryLimit = 64 << 20

const runUsage = `usage: stagent run [--detached] [--id ID] [--cols N --rows N] [--cwd DIR] -- <cmd> [args...]

Runs <cmd> on a PTY as an agent session the SSH Term app can view and
control. Without --detached the session is mirrored on this terminal and
follows its size; with --detached it runs without a terminal and the app
owns the size.

`

// Main is the `stagent run` entry point. It returns the program's exit code.
func Main(args []string) int {
	fs := flag.NewFlagSet("stagent run", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), runUsage)
		fs.PrintDefaults()
	}
	detached := fs.Bool("detached", false, "run without a local terminal; the app owns the size")
	id := fs.String("id", "", "session id: 16 lowercase hex characters (default: random)")
	cols := fs.Int("cols", 0, "columns of a detached session (default 80)")
	rows := fs.Int("rows", 0, "rows of a detached session (default 24)")
	cwd := fs.String("cwd", "", "working directory of the program (default: current)")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	command := fs.Args()
	if len(command) == 0 {
		fs.Usage()
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
	code, err := Run(context.Background(), Options{
		Command:  command,
		Detached: *detached,
		ID:       *id,
		Cols:     *cols,
		Rows:     *rows,
		Dir:      *cwd,
		Layout:   layout,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent run: %v\n", err)
	}
	return code
}
