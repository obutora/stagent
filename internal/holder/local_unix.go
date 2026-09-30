//go:build !windows

package holder

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/pty"
	"github.com/obutora/stagent/internal/wire"
)

// termSignals are forwarded to the program (see signalName).
var termSignals = []os.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT}

func signalName(s os.Signal) string {
	switch s {
	case syscall.SIGHUP:
		return pty.SignalHangup
	case syscall.SIGINT:
		return wire.SignalInterrupt
	case syscall.SIGTERM:
		return wire.SignalTerminate
	}
	return ""
}

// makeRaw puts the local terminal in raw mode: every byte goes to the
// program, whose own terminal does the line discipline.
func (t *localTerm) makeRaw() error {
	fd := int(t.in.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	t.restores = append(t.restores, func() { term.Restore(fd, st) })
	return nil
}

// watchSize calls fn with the local terminal's size on every SIGWINCH (and
// once at start, for a resize that raced the program's start).
func (t *localTerm) watchSize(ctx context.Context, fn func(cols, rows int)) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	if c, r, ok := t.size(); ok {
		fn(c, r)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if c, r, ok := t.size(); ok {
				fn(c, r)
			}
		}
	}
}
