package holder

import (
	"context"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"

	"github.com/obutora/stagent/internal/wire"
)

// termSignals: ^C/^Break (only when the console is not raw) and console
// close / logoff / shutdown, which Go reports as SIGTERM.
var termSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func signalName(s os.Signal) string {
	switch s {
	case os.Interrupt:
		return wire.SignalInterrupt
	case syscall.SIGTERM:
		return wire.SignalTerminate
	}
	return ""
}

// consolePollInterval: a console read as a VT byte stream gets no resize
// notification (window-buffer events only arrive through ReadConsoleInput),
// so the size is polled while the session runs.
const consolePollInterval = 250 * time.Millisecond

// makeRaw switches the console input to raw VT input
// (ENABLE_VIRTUAL_TERMINAL_INPUT, via x/term) and the output to VT
// processing, restoring both on exit. Note that Go's console reader drops
// Ctrl-Z, so it does not reach the program.
func (t *localTerm) makeRaw() error {
	in := int(t.in.Fd())
	st, err := term.MakeRaw(in)
	if err != nil {
		return err
	}
	t.restores = append(t.restores, func() { term.Restore(in, st) })
	out := windows.Handle(t.out.Fd())
	var mode uint32
	if windows.GetConsoleMode(out, &mode) == nil {
		vt := mode | windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING | windows.DISABLE_NEWLINE_AUTO_RETURN
		if windows.SetConsoleMode(out, vt) == nil {
			t.restores = append(t.restores, func() { windows.SetConsoleMode(out, mode) })
		}
	}
	return nil
}

// watchSize polls the console size and calls fn when it changes.
func (t *localTerm) watchSize(ctx context.Context, fn func(cols, rows int)) {
	tick := time.NewTicker(consolePollInterval)
	defer tick.Stop()
	lc, lr, _ := t.size()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if c, r, ok := t.size(); ok && (c != lc || r != lr) {
				lc, lr = c, r
				fn(c, r)
			}
		}
	}
}
