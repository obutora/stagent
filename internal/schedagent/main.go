package schedagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

const usage = `usage: stagent schedule agent --result FILE -- <agent> [args...]
  internal (agent schedules): run a coding agent non-interactively with JSON
  output, show it as text and leave how the run went in FILE
`

// exitNotStarted is the exit code when the agent could not be started.
const exitNotStarted = 127

// Main is `stagent schedule …`.
func Main(args []string) int {
	if len(args) == 0 || args[0] != "agent" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("stagent schedule agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	result := fs.String("result", "", "file to leave the run's outcome in (JSON)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 || *result == "" {
		fmt.Fprint(os.Stderr, usage)
		return 2
	}
	return run(fs.Args(), *result, os.Stdout, os.Stderr)
}

// run runs the agent of argv with nothing on its standard input, shows
// its output on out (its standard error goes to errOut as it is), leaves
// the outcome in resultPath and returns the agent's exit code (128+n when
// signal n ended it). Signals this process gets are passed on to the
// agent.
func run(argv []string, resultPath string, out, errOut io.Writer) int {
	h := harness.Command(argv)
	if !slices.Contains(wire.ScheduleHarnesses, h) {
		return finish(out, resultPath, Outcome{Result: wire.RunFailed, Error: argv[0] + " is not " + wire.ScheduleHarnessNames()}, 2)
	}
	fmt.Fprintf(out, "[stagent] %s\n", shellJoin(argv))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = errOut
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		msg := "start: " + err.Error()
		if errors.Is(err, exec.ErrNotFound) {
			msg = argv[0] + " not found on PATH"
		}
		return finish(out, resultPath, Outcome{Result: wire.RunFailed, Error: msg}, exitNotStarted)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			cmd.Process.Signal(s)
		}
	}()

	r := newReader(h, out)
	br := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			r.line(line)
		}
		if err != nil {
			break
		}
	}
	code := exitCode(cmd.Wait(), cmd.ProcessState)
	return finish(out, resultPath, r.outcome(code), code)
}

// exitCode is the agent's exit code, 128+n when signal n ended it.
func exitCode(err error, ps *os.ProcessState) int {
	if ps == nil {
		if err != nil {
			return 1
		}
		return 0
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// finish shows o, leaves it in path and returns code.
func finish(out io.Writer, path string, o Outcome, code int) int {
	fmt.Fprintf(out, "\n[stagent] %s\n", verdict(o, code))
	if err := writeOutcome(path, o); err != nil {
		fmt.Fprintf(out, "[stagent] %v\n", err)
	}
	return code
}

func verdict(o Outcome, code int) string {
	var s string
	switch o.Result {
	case wire.RunSucceeded:
		s = "succeeded"
	case wire.RunDenied:
		s = fmt.Sprintf("finished with %d refused operation(s)", o.Denials)
	default:
		s = fmt.Sprintf("failed (exit code %d)", code)
		if o.Error != "" {
			s += ": " + oneLine(o.Error, 300)
		}
	}
	if o.Unjudged {
		s += " (output not readable: judged by the exit code)"
	}
	return s
}

// writeOutcome leaves o in path (0600, whole or not at all).
func writeOutcome(path string, o Outcome) error {
	b, err := json.Marshal(o)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".outcome-*")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// ReadOutcome reads the outcome a run's wrapper left in path.
func ReadOutcome(path string) (Outcome, error) {
	var o Outcome
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &o)
	}
	return o, err
}

// shellJoin is argv as a person would type it.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(parts, " ")
}
