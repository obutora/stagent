// Package attachcli implements the session commands for a terminal user on
// the server: `stagent ls` lists the daemon's sessions and `stagent attach`
// connects the local terminal to a session's holder, like `tmux attach`.
package attachcli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// Exit codes.
const (
	exitFailure = 1
	exitUsage   = 2
)

const (
	daemonDialTimeout = 500 * time.Millisecond
	callTimeout       = 5 * time.Second
)

// Column widths (runes) past which COMMAND and TITLE/CWD are cut.
const (
	commandWidth = 40
	whereWidth   = 50
)

const lsUsage = `usage: stagent ls [--json]

Lists the sessions known to the daemon, most recently active first. Without
a running daemon the list is empty.

`

// Ls is the `stagent ls` entry point.
func Ls(args []string) int {
	fs := flag.NewFlagSet("stagent ls", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), lsUsage)
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print the sessions.list result as one JSON line")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "stagent ls: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	l, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent ls: %v\n", err)
		return exitFailure
	}
	list, err := listSessions(l)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent ls: %v\n", err)
		return exitFailure
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(wire.SessionsListResult{Sessions: list}); err != nil {
			fmt.Fprintf(os.Stderr, "stagent ls: %v\n", err)
			return exitFailure
		}
		return 0
	}
	home, _ := os.UserHomeDir()
	writeTable(os.Stdout, list, time.Now(), home)
	return 0
}

// listSessions returns the daemon's sessions, newest activity first (never
// nil). A daemon that is not running has none; it is not started for a
// listing. A daemon address or run directory another user owns is an
// error (*paths.OwnerError).
func listSessions(l *paths.Layout) ([]wire.Session, error) {
	conn, err := daemonclient.Dial(l, daemonDialTimeout)
	if err != nil {
		if errors.Is(err, paths.ErrForeignOwner) {
			return nil, err
		}
		if oe := l.ForeignOwned(); oe != nil {
			return nil, oe
		}
		return []wire.Session{}, nil
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	var res wire.SessionsListResult
	if err := c.Call(ctx, wire.MethodSessionsList, nil, &res); err != nil {
		return nil, fmt.Errorf("%s: %w", wire.MethodSessionsList, err)
	}
	if res.Sessions == nil {
		res.Sessions = []wire.Session{}
	}
	sortSessions(res.Sessions)
	return res.Sessions, nil
}

// sortSessions orders newest last_activity_at first; ties by start time,
// then id, so the order is stable across calls.
func sortSessions(list []wire.Session) {
	slices.SortFunc(list, func(a, b wire.Session) int {
		if a.LastActivityAt != b.LastActivityAt {
			return cmpDesc(a.LastActivityAt, b.LastActivityAt)
		}
		if a.StartedAt != b.StartedAt {
			return cmpDesc(a.StartedAt, b.StartedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
}

func cmpDesc(a, b int64) int {
	if a > b {
		return -1
	}
	return 1
}

// writeTable prints list (already sorted) as the human `ls` table. now and
// home are parameters so the output is reproducible.
func writeTable(w io.Writer, list []wire.Session, now time.Time, home string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODE\tSTATE\tLAST ACTIVITY\tPC INPUT\tCOMMAND\tTITLE/CWD")
	for _, s := range list {
		state := s.State
		if s.ExitCode != nil {
			state = fmt.Sprintf("%s (%d)", wire.StateExited, *s.ExitCode)
		}
		where := s.Title
		if where == "" {
			where = shortenHome(s.Cwd, home)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			s.ID, s.Mode, state, sinceOrDash(now, s.LastActivityAt), sinceOrDash(now, s.LastLocalInputAt),
			clip(printable(strings.Join(s.Command, " ")), commandWidth),
			clip(printable(where), whereWidth))
	}
	tw.Flush()
}

// sinceOrDash renders the unix ms time at with ago, or "-" when unset (0).
func sinceOrDash(now time.Time, at int64) string {
	if at <= 0 {
		return "-"
	}
	return ago(now.Sub(time.UnixMilli(at)))
}

// ago renders an elapsed time in its largest whole unit: 5s, 3m, 2h, 4d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", max(d, 0)/time.Second)
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", d/time.Minute)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", d/time.Hour)
	}
	return fmt.Sprintf("%dd ago", d/(24*time.Hour))
}

func shortenHome(dir, home string) string {
	if home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
		return "~" + string(filepath.Separator) + rest
	}
	return dir
}

// printable replaces control characters, which titles and arguments may
// contain, so they cannot drive the terminal or break the table.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return '?'
		}
		return r
	}, s)
}

// clip cuts s to n runes, marking the cut with an ellipsis.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
