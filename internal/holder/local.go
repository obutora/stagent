package holder

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync/atomic"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/detect"
)

var errNotTerminal = errors.New("stdin is not a terminal; use --detached to run without one")

// localTerm is the terminal a passthrough session runs in.
type localTerm struct {
	in, out  *os.File
	gone     atomic.Bool // output stopped: terminal hung up or failed
	restores []func()
}

func openLocal() (*localTerm, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errNotTerminal
	}
	return &localTerm{in: os.Stdin, out: os.Stdout}, nil
}

// size is the local terminal's size (stdout's, else stdin's or stderr's).
func (t *localTerm) size() (cols, rows int, ok bool) {
	for _, f := range []*os.File{t.out, t.in, os.Stderr} {
		if c, r, err := term.GetSize(int(f.Fd())); err == nil && c > 0 && r > 0 {
			return c, r, true
		}
	}
	return 0, 0, false
}

// restore undoes makeRaw. Safe to call more than once.
func (t *localTerm) restore() {
	for i := len(t.restores) - 1; i >= 0; i-- {
		t.restores[i]()
	}
	t.restores = nil
}

func (t *localTerm) write(b []byte) {
	if t.gone.Load() {
		return
	}
	if _, err := t.out.Write(b); err != nil {
		t.gone.Store(true)
	}
}

func (t *localTerm) disable() { t.gone.Store(true) }

// copyInput forwards local keystrokes to the program until stdin fails or
// the session ends. A raw-mode terminal only fails a read (EOF, EIO) once
// it hung up; lost, if not nil, is then called.
func (t *localTerm) copyInput(ctx context.Context, q *inputQueue, det *detect.Detector, lost func()) {
	buf := make([]byte, 4096)
	for {
		n, err := t.in.Read(buf)
		if n > 0 {
			det.Input()
			if q.push(ctx, []inputChunk{{data: bytes.Clone(buf[:n])}}) != nil {
				return
			}
		}
		if err != nil {
			if lost != nil && ctx.Err() == nil {
				lost()
			}
			return
		}
	}
}
