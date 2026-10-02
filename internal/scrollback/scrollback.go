// Package scrollback keeps a session's raw PTY output on disk: append-only
// segment files of SegmentSize bytes under the session's data directory,
// oldest segments deleted once the per-session cap is reached. Offsets are
// positions in the session's whole output stream, so a client can page
// backwards with `before` even after old segments were dropped.
//
// Writes are plain write(2) calls without fsync: the data is only a
// convenience copy of what the terminal showed, and readers in the same
// process see it through the page cache immediately.
package scrollback

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

var errClosed = errors.New("scrollback: closed")

// SegmentSize is the size of one segment file.
const SegmentSize = 1 << 20

// Read size limits (PROTOCOL.md session.scrollback).
const (
	DefaultReadBytes = 256 << 10
	MaxReadBytes     = 1 << 20
)

const segExt = ".seg"

type segment struct {
	start int64 // stream offset of the first byte
	size  int64
}

func (s segment) end() int64 { return s.start + s.size }

// Store is one session's scrollback. It is safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	dir     string
	segSize int64
	maxSegs int
	segs    []segment // oldest first; the last one is being appended to
	cur     *os.File
	end     int64
	err     error // sticky write error (e.g. disk full); writes become no-ops
	errAt   int64 // stream offset where err began: later bytes were not stored
}

// Open creates (or empties) dir and returns a store keeping at most maxSegs
// segments (<= 0 means 8, the default of scrollback_session_mib).
func Open(dir string, maxSegs int) (*Store, error) {
	return open(dir, maxSegs, SegmentSize)
}

func open(dir string, maxSegs int, segSize int64) (*Store, error) {
	if maxSegs <= 0 {
		maxSegs = 8
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A session id is never reused; leftovers can only come from a crashed
	// holder with the same id and are not part of this stream.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), segExt) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return &Store{dir: dir, segSize: segSize, maxSegs: maxSegs}, nil
}

func (s *Store) segPath(start int64) string {
	return filepath.Join(s.dir, fmt.Sprintf("%020d%s", start, segExt))
}

// Write appends p to the stream. After the first I/O error the store stops
// writing (the error is returned once per call) but offsets keep advancing
// so readers see a gap rather than shifted data.
func (s *Store) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if s.err != nil {
		s.end += int64(n)
		return n, s.err
	}
	for len(p) > 0 {
		if s.cur == nil || s.segs[len(s.segs)-1].size >= s.segSize {
			if err := s.rotate(); err != nil {
				s.fail(err)
				s.end += int64(len(p))
				return n, err
			}
		}
		last := &s.segs[len(s.segs)-1]
		chunk := p
		if room := s.segSize - last.size; int64(len(chunk)) > room {
			chunk = chunk[:room]
		}
		w, err := s.cur.Write(chunk)
		last.size += int64(w)
		s.end += int64(w)
		p = p[w:]
		if err != nil {
			s.fail(err)
			s.end += int64(len(p))
			return n, err
		}
	}
	return n, nil
}

func (s *Store) fail(err error) {
	s.err = err
	s.errAt = s.end
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
}

// rotate closes the current segment, opens a new one at the stream end and
// drops the oldest segments beyond the cap.
func (s *Store) rotate() error {
	if s.cur != nil {
		s.cur.Close()
		s.cur = nil
	}
	f, err := os.OpenFile(s.segPath(s.end), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.cur = f
	s.segs = append(s.segs, segment{start: s.end})
	for len(s.segs) > s.maxSegs {
		os.Remove(s.segPath(s.segs[0].start))
		s.segs = s.segs[1:]
	}
	return nil
}

// Bounds returns the oldest retained offset and the stream end.
func (s *Store) Bounds() (first, end int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.first(), s.end
}

func (s *Store) first() int64 {
	if len(s.segs) == 0 {
		return s.end
	}
	return s.segs[0].start
}

// Read returns the stream bytes [start, end) ending at before (0 = the
// latest byte), at most maxBytes long (<= 0 → DefaultReadBytes, capped at
// MaxReadBytes), and the oldest retained offset. start == first means the
// beginning of the retained stream was reached.
func (s *Store) Read(before int64, maxBytes int) (data []byte, start, end, first int64, err error) {
	if maxBytes <= 0 {
		maxBytes = DefaultReadBytes
	}
	if maxBytes > MaxReadBytes {
		maxBytes = MaxReadBytes
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	first = s.first()
	end = s.end
	if before > 0 && before < end {
		end = before
	}
	if end < first {
		end = first
	}
	start = max(first, end-int64(maxBytes))
	if start == end {
		return []byte{}, start, end, first, nil
	}
	if data, err = s.read(start, end); err != nil {
		return nil, 0, 0, 0, err
	}
	return data, start, end, first, nil
}

// Range returns exactly the stream bytes [start, end), at most MaxReadBytes.
// ok is false when any of them is no longer retained or was never stored
// (after a write error), so a caller resuming a client from start falls back
// to something else rather than send a gap.
func (s *Store) Range(start, end int64) (data []byte, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.end
	if s.err != nil {
		stored = s.errAt
	}
	if start < s.first() || end > stored || start > end || end-start > MaxReadBytes {
		return nil, false
	}
	if start == end {
		return []byte{}, true
	}
	data, err := s.read(start, end)
	if err != nil {
		return nil, false
	}
	return data, true
}

// read copies [start, end) out of the retained segments (s.mu held).
func (s *Store) read(start, end int64) ([]byte, error) {
	data := make([]byte, end-start)
	i := sort.Search(len(s.segs), func(i int) bool { return s.segs[i].end() > start })
	for off := start; off < end && i < len(s.segs); i++ {
		sg := s.segs[i]
		lo, hi := off, min(end, sg.end())
		if err := readAt(s.segPath(sg.start), data[lo-start:hi-start], lo-sg.start); err != nil {
			return nil, err
		}
		off = hi
	}
	return data, nil
}

func readAt(path string, buf []byte, off int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.ReadAt(buf, off)
	if errors.Is(err, io.EOF) {
		// Bytes counted after a write error were never stored; leave zeros.
		clear(buf[n:])
		err = nil
	}
	return err
}

// Close closes the current segment; later writes are dropped. The files
// stay for the daemon's retention to delete.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.cur != nil {
		err = s.cur.Close()
		s.cur = nil
	}
	if s.err == nil {
		s.err = errClosed
		s.errAt = s.end
	}
	return err
}
