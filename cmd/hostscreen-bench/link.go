package main

import (
	"io"
	"sync"
	"time"
)

// Link is one direction of the emulated connection between the app and
// stagent: bytes go out one after another at Bps bits per second, then
// arrive Delay later (one-way delay). Every stream of one direction of the
// SSH connection shares one Link, so a second channel (the binary frame
// channel of #1974) must be wrapped in the same Link as the bridge's.
type Link struct {
	bps   float64
	delay time.Duration

	mu   sync.Mutex
	free time.Time // when the sender is done with what was written so far

	// OnDeliver, when set before use, is told each chunk as it arrives at
	// the far end (at is its arrival time). Called from the delivering
	// goroutine.
	OnDeliver func(at time.Time, n int)
}

// NewLink returns a link of bps bits per second and delay one way.
func NewLink(bps float64, delay time.Duration) *Link {
	return &Link{bps: bps, delay: delay}
}

// schedule takes n bytes written at the time at and returns when their last
// byte arrives at the far end.
func (l *Link) schedule(at time.Time, n int) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	start := at
	if l.free.After(start) {
		start = l.free
	}
	l.free = start.Add(time.Duration(float64(n) * 8 / l.bps * float64(time.Second)))
	return l.free.Add(l.delay)
}

// chunkSize is the most read from a source at once: a frame line arrives
// over several chunks, as it would over the wire.
const chunkSize = 16 << 10

type chunk struct {
	data []byte
	at   time.Time
}

// deliver waits until c's arrival time and reports it.
func (l *Link) deliver(c chunk) {
	if d := time.Until(c.at); d > 0 {
		time.Sleep(d)
	}
	if l.OnDeliver != nil {
		l.OnDeliver(c.at, len(c.data))
	}
}

// Reader returns what src yields, each chunk when it arrives at the far
// end. src is read eagerly (as an SSH channel's window would let it write);
// how fast stagent sends is bounded by host_screen.ack, not by the link.
func (l *Link) Reader(src io.Reader) io.Reader {
	r := &linkReader{l: l, ch: make(chan chunk, 4096)}
	go func() {
		defer close(r.ch)
		for {
			buf := make([]byte, chunkSize)
			n, err := src.Read(buf)
			if n > 0 {
				r.ch <- chunk{data: buf[:n], at: l.schedule(time.Now(), n)}
			}
			if err != nil {
				r.err = err
				return
			}
		}
	}()
	return r
}

type linkReader struct {
	l    *Link
	ch   chan chunk
	err  error // set before ch is closed
	left []byte
}

func (r *linkReader) Read(p []byte) (int, error) {
	if len(r.left) == 0 {
		c, ok := <-r.ch
		if !ok {
			return 0, r.err
		}
		r.l.deliver(c)
		r.left = c.data
	}
	n := copy(p, r.left)
	r.left = r.left[n:]
	return n, nil
}

// Writer returns a writer whose bytes reach dst when they arrive at the far
// end. Write never waits for the link; Close delivers what is queued, then
// closes dst.
func (l *Link) Writer(dst io.WriteCloser) io.WriteCloser {
	w := &linkWriter{ch: make(chan chunk, 4096), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var werr error
		for c := range w.ch {
			l.deliver(c)
			if werr == nil {
				_, werr = dst.Write(c.data)
			}
		}
		dst.Close()
	}()
	w.l = l
	return w
}

type linkWriter struct {
	l    *Link
	mu   sync.Mutex
	ch   chan chunk
	done chan struct{}
	shut bool
}

func (w *linkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.shut {
		return 0, io.ErrClosedPipe
	}
	w.ch <- chunk{data: append([]byte(nil), p...), at: w.l.schedule(time.Now(), len(p))}
	return len(p), nil
}

func (w *linkWriter) Close() error {
	w.mu.Lock()
	if !w.shut {
		w.shut = true
		close(w.ch)
	}
	w.mu.Unlock()
	<-w.done
	return nil
}
