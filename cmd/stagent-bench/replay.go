package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// markerPrefix precedes the unix-microsecond timestamp embedded in the
// replayed stream. The synthetic stream shows it on the spinner line (so it
// survives screen mode); fixture replays insert it as an unknown OSC that
// terminals ignore (raw mode only).
const markerPrefix = "T="

const oscMarkerFmt = "\x1b]5379;" + markerPrefix + "%016d\x07"

// replayMain is `stagent-bench replay`: the fake agent run inside each
// benchmark session. It writes a TUI-like stream to its terminal until it
// reads a "quiet" line (then it stays silent, as an agent waiting for
// input) and exits on EOF.
func replayMain(args []string) int {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	rate := fs.Float64("rate", 20, "frames per second at 1x")
	speed := fs.Float64("speed", 1, "playback speed multiplier (1, 10, ...)")
	fixture := fs.String("fixture", "", "replay raw bytes from this file instead of the synthetic stream")
	chunk := fs.Int("chunk", 2048, "fixture bytes per frame")
	seed := fs.Int64("seed", 1, "synthetic stream variation")
	fs.Parse(args)

	cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || cols <= 0 || rows <= 0 {
		cols, rows = 80, 24
	}
	var src frameSource
	if *fixture != "" {
		data, err := os.ReadFile(*fixture)
		if err != nil || len(data) == 0 {
			fmt.Fprintln(os.Stderr, "replay: fixture:", err)
			return 1
		}
		src = &fixtureSource{data: data, chunk: max(*chunk, 1)}
	} else {
		src = newSynth(cols, rows, *seed)
	}

	quiet := make(chan struct{})
	eof := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == "quiet" {
				close(quiet)
				break
			}
		}
		for sc.Scan() {
		}
		close(eof)
	}()

	out := bufio.NewWriterSize(os.Stdout, 64<<10)
	tick := time.NewTicker(time.Duration(float64(time.Second) / (*rate * *speed)))
	defer tick.Stop()
	var buf []byte
	for {
		select {
		case <-tick.C:
			buf = src.frame(buf[:0], time.Now())
			out.Write(buf)
			if out.Flush() != nil {
				return 0
			}
		case <-quiet:
			tick.Stop()
			quiet = nil
		case <-eof:
			return 0
		}
	}
}

// frameSource produces one frame of output per tick.
type frameSource interface {
	frame(dst []byte, now time.Time) []byte
}

type fixtureSource struct {
	data  []byte
	chunk int
	off   int
}

func (f *fixtureSource) frame(dst []byte, now time.Time) []byte {
	end := min(f.off+f.chunk, len(f.data))
	dst = append(dst, f.data[f.off:end]...)
	f.off = end
	if f.off == len(f.data) {
		f.off = 0
	}
	return fmt.Appendf(dst, oscMarkerFmt, now.UnixMicro())
}

// synth imitates a coding agent's TUI: a status/spinner line rewritten in
// place with CR every frame, an Ink-style dynamic region erased and redrawn
// below the streamed text, streamed colored text lines, and periodic
// synchronized-output (DEC 2026) full-screen redraws.
type synth struct {
	cols, rows int
	n          int
	words      []string
	w          int
	started    time.Time
}

func newSynth(cols, rows int, seed int64) *synth {
	words := strings.Fields(`the agent reads files edits code runs tests and reports results while
the user waits for the next turn of the conversation with tools like bash grep read write
edit glob search task and many more lines of streamed markdown output appear here`)
	return &synth{cols: cols, rows: rows, words: words, w: int(seed) % len(words)}
}

var spinner = []string{"·", "✢", "✳", "✶", "✻", "✽"}

const dynLines = 4 // spinner + input box (3 lines)

func (s *synth) frame(dst []byte, now time.Time) []byte {
	if s.started.IsZero() {
		s.started = now
	}
	s.n++
	switch {
	case s.n%100 == 1:
		// Synchronized full redraw of the whole screen.
		dst = append(dst, "\x1b[?2026h\x1b[H\x1b[2J"...)
		for r := 0; r < s.rows-dynLines-1; r++ {
			dst = fmt.Appendf(dst, "\x1b[38;5;%dm%s\x1b[0m\r\n", 16+(r*7)%216, s.line(s.cols-1))
		}
		dst = s.dynamic(dst, now)
		return append(dst, "\x1b[?2026l"...)
	case s.n%4 == 0:
		// Stream a line of text above the dynamic region (Ink: erase the
		// region, print the static line, redraw the region).
		dst = s.erase(dst)
		dst = fmt.Appendf(dst, "\x1b[1m●\x1b[0m \x1b[38;2;%d;%d;200m%s\x1b[0m\r\n", 100+s.n%100, 150, s.line(s.cols-3))
		return s.dynamic(dst, now)
	default:
		// Spinner tick: rewrite the status line in place with CR.
		dst = fmt.Appendf(dst, "\x1b[%dA\r\x1b[2K", dynLines)
		dst = s.status(dst, now)
		return fmt.Appendf(dst, "\x1b[%dB\r", dynLines)
	}
}

func (s *synth) erase(dst []byte) []byte {
	for range dynLines {
		dst = append(dst, "\x1b[1A\x1b[2K"...)
	}
	return append(dst, '\r')
}

// dynamic draws the spinner line and a 3-line input box, leaving the
// cursor on the line below it.
func (s *synth) dynamic(dst []byte, now time.Time) []byte {
	dst = s.status(dst, now)
	dst = append(dst, "\r\n"...)
	w := max(s.cols-2, 4)
	dst = append(dst, "\x1b[2m╭"...)
	dst = append(dst, strings.Repeat("─", w)...)
	dst = append(dst, "╮\x1b[0m\r\n\x1b[2m│\x1b[0m > "...)
	dst = append(dst, strings.Repeat(" ", max(w-3, 0))...)
	dst = append(dst, "\x1b[2m│\x1b[0m\r\n\x1b[2m╰"...)
	dst = append(dst, strings.Repeat("─", w)...)
	return append(dst, "╯\x1b[0m\r\n"...)
}

func (s *synth) status(dst []byte, now time.Time) []byte {
	sec := int(now.Sub(s.started).Seconds())
	return fmt.Appendf(dst, "\x1b[38;5;208m%s\x1b[0m Working… (%ds · esc to interrupt) \x1b[2m%s%016d\x1b[0m",
		spinner[s.n%len(spinner)], sec, markerPrefix, now.UnixMicro())
}

// line returns up to n columns of words.
func (s *synth) line(n int) string {
	var b strings.Builder
	for b.Len() < n-12 {
		b.WriteString(s.words[s.w%len(s.words)])
		b.WriteByte(' ')
		s.w++
	}
	return b.String()[:min(b.Len(), max(n, 0))]
}

// streamRate reports the bytes per second one replay session produces, by
// running its generator for ten simulated seconds.
func streamRate(fixture string, chunk int, rate, speed float64, cols, rows int) float64 {
	var src frameSource
	if fixture != "" {
		f, err := os.Open(fixture)
		if err != nil {
			return 0
		}
		defer f.Close()
		data, _ := io.ReadAll(f)
		if len(data) == 0 {
			return 0
		}
		src = &fixtureSource{data: data, chunk: max(chunk, 1)}
	} else {
		src = newSynth(cols, rows, 1)
	}
	frames := int(rate * speed * 10)
	start := time.Now()
	var total int
	var buf []byte
	for i := range frames {
		buf = src.frame(buf[:0], start.Add(time.Duration(float64(i)*float64(time.Second)/(rate*speed))))
		total += len(buf)
	}
	return float64(total) / 10
}

// parseMarkers calls fn with each embedded timestamp in data.
func parseMarkers(data []byte, fn func(us int64)) {
	for {
		i := bytes.Index(data, []byte(markerPrefix))
		if i < 0 {
			return
		}
		data = data[i+len(markerPrefix):]
		if len(data) < 16 {
			return
		}
		if us, err := strconv.ParseInt(string(data[:16]), 10, 64); err == nil {
			fn(us)
		}
		data = data[16:]
	}
}
