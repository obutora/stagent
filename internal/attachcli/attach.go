package attachcli

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/wire"
)

const attachUsage = `usage: stagent attach [ID | --last] [--detach-key ctrl-]]

Attaches this terminal to a running session, like tmux attach: the session's
screen is redrawn here, keystrokes go to its program and the session takes
this terminal's size. Without ID the only live session is chosen; --last
picks the most recently active one. Press the detach key (default Ctrl-])
to detach, leaving the session running; press it twice to send it to the
program. When the program exits, attach exits with its exit code.

`

const defaultDetachKey = "ctrl-]"

type attachOptions struct {
	id   string // empty: choose (see chooseSession)
	last bool
	key  byte
}

// parseAttachArgs parses the `stagent attach` command line. ok is false
// when the command must exit with exitUsage (message already printed).
func parseAttachArgs(args []string) (o attachOptions, ok bool) {
	fs := flag.NewFlagSet("stagent attach", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), attachUsage)
		fs.PrintDefaults()
	}
	last := fs.Bool("last", false, "attach to the live session with the most recent activity")
	key := fs.String("detach-key", defaultDetachKey, "detach key: ctrl-X with X one of @, a-z, \\, ], ^, _")
	// The ID may precede the flags, as in `stagent attach ID --detach-key ctrl-q`.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		o.id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return o, false
	}
	fail := func(format string, a ...any) (attachOptions, bool) {
		fmt.Fprintf(os.Stderr, "stagent attach: "+format+"\n", a...)
		return o, false
	}
	switch {
	case fs.NArg() > 1 || fs.NArg() == 1 && o.id != "":
		return fail("unexpected argument %q", fs.Arg(fs.NArg()-1))
	case fs.NArg() == 1:
		o.id = fs.Arg(0)
	}
	o.last = *last
	if o.id != "" && o.last {
		return fail("give either a session ID or --last, not both")
	}
	if o.id != "" && !validID(o.id) {
		return fail("invalid session id %q (want 16 lowercase hex characters)", o.id)
	}
	k, err := parseDetachKey(*key)
	if err != nil {
		return fail("%v", err)
	}
	o.key = k
	return o, true
}

// parseDetachKey maps "ctrl-X" to its control byte. Ctrl-[ is refused: it
// is ESC, which starts every cursor and function key, so they would all
// detach.
func parseDetachKey(s string) (byte, error) {
	rest, ok := strings.CutPrefix(strings.ToLower(s), "ctrl-")
	if ok && len(rest) == 1 {
		switch c := rest[0]; {
		case c >= 'a' && c <= 'z':
			return c - 'a' + 1, nil
		case c == '@' || c == '\\' || c == ']' || c == '^' || c == '_':
			return c & 0x1f, nil
		case c == '[':
			return 0, errors.New("--detach-key ctrl-[ is Escape, which starts every special key; pick another")
		}
	}
	return 0, fmt.Errorf("--detach-key %q: want ctrl-X with X one of @, a-z, \\, ], ^, _", s)
}

// validID accepts the 16 lowercase hex chars of wire.NewSessionID, which
// also keeps ids from escaping the holder socket directory.
func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// errChoose is a session selection failure (exit code exitUsage).
type errChoose struct{ msg string }

func (e *errChoose) Error() string { return e.msg }

// chooseSession resolves the session to attach to. An explicit id is used
// as is (the holder is asked directly, so attaching works without a
// daemon); otherwise the daemon's list decides. The session this command
// runs in (a shell inside a session) is never chosen: attaching it to its
// own terminal would feed its output back to itself.
func chooseSession(l *paths.Layout, o attachOptions) (string, error) {
	self := os.Getenv(wire.EnvSessionID)
	if o.id != "" {
		if o.id == self {
			return "", &errChoose{fmt.Sprintf("this terminal is session %s; it cannot attach to itself", self)}
		}
		return o.id, nil
	}
	list, err := listSessions(l)
	if err != nil {
		return "", err
	}
	home, _ := os.UserHomeDir()
	return pickSession(list, o.last, self, time.Now(), home)
}

// pickSession chooses among list (sorted newest activity first), skipping
// self: with last the live session with the newest activity, else the only
// live one. The error lists the live sessions to choose from.
func pickSession(list []wire.Session, last bool, self string, now time.Time, home string) (string, error) {
	var live []wire.Session
	for _, s := range list {
		if s.ExitCode == nil && s.ID != self {
			live = append(live, s)
		}
	}
	switch {
	case len(live) == 0:
		return "", &errChoose{"no live sessions to attach to (see stagent ls)"}
	case last || len(live) == 1:
		return live[0].ID, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d live sessions; give an ID or --last:\n\n", len(live))
	writeTable(&b, live, now, home)
	return "", &errChoose{strings.TrimRight(b.String(), "\n")}
}

// sessionGone explains why the holder of id did not answer, using the
// daemon's record when there is one.
func sessionGone(l *paths.Layout, id string) error {
	list, _ := listSessions(l)
	for _, s := range list {
		if s.ID == id && s.ExitCode != nil {
			return fmt.Errorf("session %s has ended (exit %d)", id, *s.ExitCode)
		}
	}
	return fmt.Errorf("session %s is not running", id)
}
