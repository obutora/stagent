package holder

import (
	"bytes"
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/detect"
)

var errNotTerminal = errors.New("stdin is not a terminal; use --detached to run without one")

// Output to the local terminal goes through a queue written by its own
// goroutine. The terminal holds the program back, as any terminal does,
// only while it keeps taking output: a write that makes no progress for
// localStallAfter (a Windows console with a selection, an SSH client gone
// without a hangup) means it stopped reading. Its queued output is then
// dropped and later output skipped, so attached clients keep getting
// output, and once it reads again it is redrawn from a snapshot (#281).
const (
	// localQueueLimit is the queued output past which the pump waits for
	// the terminal to take some.
	localQueueLimit = 64 << 10
	// localWriteChunk bounds one write, so a slow terminal that still
	// reads shows progress well within localStallAfter.
	localWriteChunk = 4 << 10
	localStallAfter = 2 * time.Second
)

// localTerm is the terminal a passthrough session runs in.
type localTerm struct {
	in, out  *os.File
	restores []func()

	mu     sync.Mutex
	wake   *sync.Cond // the writer: queue, stalled or gone changed
	queue  [][]byte
	queued int
	busy   bool // the writer is writing bytes it took off the queue
	// gone: output stopped for good (terminal hung up, failed, or
	// disabled).
	gone bool
	// stalled: output was dropped; the terminal is redrawn before it gets
	// more.
	stalled bool
	// lastProgress is when the writer last started or finished a write, or
	// got output while idle.
	lastProgress time.Time
	progress     chan struct{} // a write finished or output stopped
}

func openLocal() (*localTerm, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, errNotTerminal
	}
	t := &localTerm{in: os.Stdin, out: os.Stdout, progress: make(chan struct{}, 1)}
	t.wake = sync.NewCond(&t.mu)
	return t, nil
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

// waitRoom blocks the pump while the queue is full and the terminal is
// taking output. It reports whether the terminal was found stalled now.
func (t *localTerm) waitRoom() (stalled bool) {
	for {
		t.mu.Lock()
		if t.gone || t.stalled || t.queued < localQueueLimit {
			t.mu.Unlock()
			return false
		}
		idle := time.Since(t.lastProgress)
		if idle >= localStallAfter {
			t.dropLocked()
			t.stalled = true
			t.mu.Unlock()
			return true
		}
		t.mu.Unlock()
		select {
		case <-t.progress:
		case <-time.After(localStallAfter - idle):
		}
	}
}

// push queues output for the terminal and returns the copy it queued, nil
// when the terminal takes no output now. Called with h.mu held, so a
// redraw snapshot lines up with the output queued after it.
func (t *localTerm) push(b []byte) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gone || t.stalled {
		return nil
	}
	if len(t.queue) == 0 && !t.busy {
		t.lastProgress = time.Now()
	}
	c := bytes.Clone(b)
	t.queue = append(t.queue, c)
	t.queued += len(c)
	t.wake.Signal()
	return c
}

// restart replaces the dropped output of a stalled terminal with snap,
// which redraws it. Called with h.mu held; reports whether it did.
func (t *localTerm) restart(snap []byte) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.gone || !t.stalled {
		return false
	}
	t.stalled = false
	t.queue = append(t.queue[:0], snap)
	t.queued = len(snap)
	t.lastProgress = time.Now()
	return true
}

// writeOutput writes queued output until the terminal fails or output is
// disabled. When the terminal reads again after it stalled, resync is
// called (without t.mu) to restart it.
func (t *localTerm) writeOutput(resync func()) {
	for {
		t.mu.Lock()
		for !t.gone && !t.stalled && len(t.queue) == 0 {
			t.wake.Wait()
		}
		if t.gone {
			t.mu.Unlock()
			return
		}
		if t.stalled {
			t.mu.Unlock()
			resync()
			continue
		}
		b := t.queue[0]
		if len(b) > localWriteChunk {
			t.queue[0] = b[localWriteChunk:]
			b = b[:localWriteChunk]
		} else {
			t.queue[0] = nil
			t.queue = t.queue[1:]
		}
		t.queued -= len(b)
		t.busy = true
		t.lastProgress = time.Now()
		t.mu.Unlock()

		_, err := t.out.Write(b)

		t.mu.Lock()
		t.busy = false
		t.lastProgress = time.Now()
		if err != nil {
			t.stopLocked()
		}
		t.mu.Unlock()
		t.signalProgress()
	}
}

// drain waits until the queued output has been written, output stopped,
// or the terminal made no progress for localStallAfter.
func (t *localTerm) drain() {
	for {
		t.mu.Lock()
		idle := time.Since(t.lastProgress)
		done := t.gone || (len(t.queue) == 0 && !t.busy && !t.stalled) || idle >= localStallAfter
		t.mu.Unlock()
		if done {
			return
		}
		select {
		case <-t.progress:
		case <-time.After(localStallAfter - idle):
		}
	}
}

// disable stops output to the terminal for good.
func (t *localTerm) disable() {
	t.mu.Lock()
	t.stopLocked()
	t.mu.Unlock()
	t.signalProgress()
}

func (t *localTerm) stopLocked() {
	t.gone = true
	t.stalled = false
	t.dropLocked()
	t.wake.Broadcast()
}

func (t *localTerm) dropLocked() {
	clear(t.queue)
	t.queue = t.queue[:0]
	t.queued = 0
}

func (t *localTerm) signalProgress() {
	select {
	case t.progress <- struct{}{}:
	default:
	}
}

// copyInput forwards local keystrokes to the program until stdin fails or
// the session ends; input is called with each read before it is forwarded.
// A raw-mode terminal only fails a read (EOF, EIO) once it hung up; lost,
// if not nil, is then called.
func (t *localTerm) copyInput(ctx context.Context, q *inputQueue, det *detect.Detector, input func([]byte), lost func()) {
	buf := make([]byte, 4096)
	for {
		n, err := t.in.Read(buf)
		if n > 0 {
			det.Input()
			input(buf[:n])
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
