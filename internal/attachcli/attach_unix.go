//go:build !windows

package attachcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const holderDialTimeout = 2 * time.Second

// resetModes undoes terminal modes the program may have left on when
// attach ends mid-session: alternate screen, hidden cursor, application
// cursor keys and keypad, bracketed paste, mouse tracking and encodings,
// focus events, kitty keyboard flags, autowrap off, scroll region (reset
// with the cursor saved: DECSTBM homes it) and SGR.
const resetModes = "\x1b[?1049l\x1b[?25h\x1b[?1l\x1b>\x1b[?2004l" +
	"\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l\x1b[<u\x1b[?7h\x1b7\x1b[r\x1b8\x1b[m"

// Attach is the `stagent attach` entry point.
func Attach(args []string) int {
	o, ok := parseAttachArgs(args)
	if !ok {
		return exitUsage
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(os.Stderr, "stagent attach: stdin is not a terminal")
		return exitUsage
	}
	l, err := paths.Resolve()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent attach: %v\n", err)
		return exitFailure
	}
	id, err := chooseSession(l, o)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent attach: %v\n", err)
		if _, ok := errors.AsType[*errChoose](err); ok {
			return exitUsage
		}
		return exitFailure
	}
	code, err := attachTerminal(l, id, o.key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stagent attach: %v\n", err)
	}
	return code
}

// attached is one attachment of the local terminal to a holder.
type attached struct {
	id     string
	c      *rpc.Client
	fd     int // stdin
	closed chan int

	// mu orders terminal output: notifications write under it, and once
	// done is set (attach is ending) nothing more is written.
	mu    sync.Mutex
	done  bool
	saved *term.State
}

// attachTerminal runs one attachment until detach, session end, lost
// connection or a terminating signal, and returns the exit code. An error
// is returned (with its code) only before the terminal was taken over.
func attachTerminal(l *paths.Layout, id string, key byte) (int, error) {
	conn, err := ipc.Dial(l.HolderAddr(id), holderDialTimeout)
	if err != nil {
		if errors.Is(err, paths.ErrForeignOwner) {
			return exitFailure, err
		}
		if oe := l.ForeignOwned(); oe != nil {
			return exitFailure, oe
		}
		return exitFailure, sessionGone(l, id)
	}
	a := &attached{id: id, fd: int(os.Stdin.Fd()), closed: make(chan int, 1)}
	a.c = rpc.NewClient(conn, a.onNotify)
	defer a.c.Close()

	// Signals first, so one arriving after MakeRaw still restores.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(stop)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	st, err := term.MakeRaw(a.fd)
	if err != nil {
		return exitFailure, fmt.Errorf("raw mode: %w", err)
	}
	a.saved = st
	defer a.restore()

	// The size first, so the snapshot is drawn at this terminal's size.
	// A failure (the session ended meanwhile) shows in the attach reply,
	// except a refusal: the holder closes the connection after it.
	if cols, rows, ok := localSize(); ok {
		err := a.call(wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: cols, Rows: rows, Force: true}, nil)
		if we, ok := errors.AsType[*wire.Error](err); ok && we.Code == wire.ErrAgentRefused {
			a.restore()
			return exitFailure, errors.New(we.Message)
		}
	}
	var res wire.AttachResult
	if err := a.call(wire.MethodSessionAttach, wire.AttachParams{ID: id, Mode: wire.AttachRaw}, &res); err != nil {
		a.restore()
		if we, ok := errors.AsType[*wire.Error](err); ok {
			err = errors.New(we.Message)
		}
		return exitFailure, fmt.Errorf("attach %s: %w", id, err)
	}

	reads := make(chan []byte)
	go readInput(reads)
	in := newInput(key)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	process := func(text []byte, detach bool) (int, bool) {
		if len(text) > 0 {
			a.input(text)
		}
		if detach {
			a.call(wire.MethodSessionDetach, wire.SessionRef{ID: id}, nil)
			a.finish("[detached]")
			return 0, true
		}
		if d, ok := in.deadline(); ok {
			timer.Reset(time.Until(d))
		} else {
			timer.Stop()
		}
		return 0, false
	}
	for {
		select {
		case b, ok := <-reads:
			if !ok {
				// The local terminal is gone (SIGHUP usually comes too).
				a.finish("")
				return exitFailure, nil
			}
			if code, end := process(in.feed(b, time.Now())); end {
				return code, nil
			}
		case <-timer.C:
			if code, end := process(in.expire(time.Now())); end {
				return code, nil
			}
		case <-winch:
			if cols, rows, ok := localSize(); ok {
				a.notifyCall(wire.MethodSessionResize, wire.ResizeParams{ID: id, Cols: cols, Rows: rows, Force: true})
			}
		case sig := <-stop:
			a.finish(fmt.Sprintf("[detached (%v)]", sig))
			if s, ok := sig.(syscall.Signal); ok {
				return 128 + int(s), nil
			}
			return exitFailure, nil
		case code := <-a.closed:
			a.finish(fmt.Sprintf("[session ended (exit %d)]", code))
			return code, nil
		case <-a.c.Done():
			// `closed` is dispatched before the connection ends.
			select {
			case code := <-a.closed:
				a.finish(fmt.Sprintf("[session ended (exit %d)]", code))
				return code, nil
			default:
			}
			a.finish("[connection to session lost]")
			return exitFailure, nil
		}
	}
}

// onNotify runs on the client's read goroutine. Writing output there makes
// a slow terminal back-pressure the holder, which resynchronizes an
// attachment that falls too far behind with a snapshot.
func (a *attached) onNotify(m *wire.Msg) {
	switch m.Method {
	case wire.NotifyOutput:
		var p wire.OutputParams
		if json.Unmarshal(m.Params, &p) != nil {
			return
		}
		// A reset snapshot starts by clearing the screen itself.
		a.mu.Lock()
		if !a.done {
			os.Stdout.Write(p.Data)
		}
		a.mu.Unlock()
	case wire.NotifyClosed:
		var p wire.ClosedParams
		if json.Unmarshal(m.Params, &p) != nil {
			p.ExitCode = -1
		}
		select {
		case a.closed <- p.ExitCode:
		default:
		}
	}
}

// call makes a request, waiting at most callTimeout.
func (a *attached) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return a.c.Call(ctx, method, params, result)
}

// notifyCall sends a request without waiting for its reply. The holder
// handles a connection's requests in order, so keystrokes stay ordered;
// a failure (the session ended) is reported by `closed`.
func (a *attached) notifyCall(method string, params any) {
	b, err := json.Marshal(params)
	if err != nil {
		return
	}
	a.c.Go(method, b, func(*wire.Msg) {})
}

// input sends keyboard bytes, marked as typed at a terminal on the host
// (presence, last_local_input_at). They travel as a JSON string, so bytes
// that are not UTF-8 (8-bit meta keys) arrive as U+FFFD.
func (a *attached) input(text []byte) {
	a.notifyCall(wire.MethodSessionInput, wire.InputParams{ID: a.id, Text: string(text), Local: true})
}

// finish stops terminal output, resets the modes the program may have
// left on, prints msg on its own line and restores the terminal.
func (a *attached) finish(msg string) {
	a.mu.Lock()
	if !a.done {
		a.done = true
		out := resetModes + "\r\n"
		if msg != "" {
			out += msg + "\r\n"
		}
		os.Stdout.WriteString(out)
	}
	a.mu.Unlock()
	a.restore()
}

// restore leaves raw mode; safe to call more than once.
func (a *attached) restore() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.saved != nil {
		term.Restore(a.fd, a.saved)
		a.saved = nil
	}
}

// readInput forwards stdin reads until stdin fails, then closes reads.
func readInput(reads chan<- []byte) {
	defer close(reads)
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			reads <- bytes.Clone(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// localSize is the local terminal's size (stdout's, else stdin's or
// stderr's).
func localSize() (cols, rows int, ok bool) {
	for _, f := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		if c, r, err := term.GetSize(int(f.Fd())); err == nil && c > 0 && r > 0 {
			return c, r, true
		}
	}
	return 0, 0, false
}
