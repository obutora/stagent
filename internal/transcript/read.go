package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/obutora/stagent/internal/wire"
)

const (
	// DefaultLimit is the page size when the caller gives none.
	DefaultLimit = 50
	// maxLimit bounds one page.
	maxLimit = 500
	// maxLine skips records larger than this (inline images, huge tool
	// output) instead of holding them in memory.
	maxLine = 8 << 20
	// readChunk is the unit of backward reads.
	readChunk = 64 << 10
	// maxTailRead bounds one ReadFrom call; the caller continues from the
	// returned offset.
	maxTailRead = 4 << 20
)

// Page is one page of a transcript.
type Page struct {
	Messages []wire.Message // oldest first
	// Cursor is the byte offset to pass as before for older messages;
	// 0 means the beginning was reached.
	Cursor int64
	// End is the offset just after the newest complete record: tail from
	// here to receive only what comes after this page.
	End int64
}

// ReadPage returns up to limit messages that end before byte offset before
// (0 = end of file), reading the file backwards so large transcripts cost
// only what the page needs. A record's messages are never split across
// pages, so a page may exceed limit by the few messages of its first record.
func ReadPage(path, harness string, before int64, limit int) (Page, error) {
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, maxLimit)
	f, err := os.Open(path)
	if err != nil {
		return Page{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Page{}, err
	}
	size := st.Size()
	end := completeEnd(f, size)
	if before <= 0 || before > end {
		before = end
	}
	page := Page{End: end}
	sc := newReverseScanner(f, before)
	var newestFirst [][]wire.Message
	count := 0
	cursor := before
	for count < limit {
		line, start, ok, err := sc.prev()
		if err != nil {
			return Page{}, err
		}
		if !ok {
			cursor = 0
			break
		}
		cursor = start
		if msgs := ParseLine(harness, line); len(msgs) > 0 {
			newestFirst = append(newestFirst, msgs)
			count += len(msgs)
		}
	}
	if cursor > 0 && sc.atStart() {
		cursor = 0
	}
	page.Cursor = cursor
	page.Messages = make([]wire.Message, 0, count)
	for i := len(newestFirst) - 1; i >= 0; i-- {
		page.Messages = append(page.Messages, newestFirst[i]...)
	}
	return page, nil
}

// ReadFrom parses the complete records from offset on and returns their
// messages with the offset to continue from. A trailing partial record is
// left for the next call. If the file shrank (rewritten), reading restarts
// at its current end without replaying it.
func ReadFrom(path, harness string, offset int64) ([]wire.Message, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	size := st.Size()
	if size < offset {
		return nil, completeEnd(f, size), nil
	}
	if size == offset {
		return nil, offset, nil
	}
	n := min(size-offset, maxTailRead)
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, offset, err
	}
	var out []wire.Message
	consumed := 0
	for {
		i := bytes.IndexByte(buf[consumed:], '\n')
		if i < 0 {
			break
		}
		out = append(out, ParseLine(harness, buf[consumed:consumed+i])...)
		consumed += i + 1
	}
	rest := buf[consumed:]
	switch {
	case consumed == 0 && n == maxTailRead:
		// One record bigger than a whole read: skip past it without parsing.
		next, err := skipLine(f, offset+n, size)
		return out, next, err
	case offset+n == size && len(rest) > 0 && json.Valid(bytes.TrimSpace(rest)):
		// Final record without a newline yet but complete.
		out = append(out, ParseLine(harness, rest)...)
		consumed = len(buf)
	}
	return out, offset + int64(consumed), nil
}

// EndOffset is where a tail of path should start to see only new records.
func EndOffset(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return completeEnd(f, st.Size()), nil
}

// completeEnd is the offset after the last complete record of a file of the
// given size: after the last newline, or size when the unterminated tail is
// itself a complete JSON value.
func completeEnd(f io.ReaderAt, size int64) int64 {
	nl := afterLastNewline(f, size)
	if nl == size {
		return size
	}
	if tail := size - nl; tail <= maxLine {
		t := make([]byte, tail)
		if _, err := f.ReadAt(t, nl); err == nil && json.Valid(bytes.TrimSpace(t)) {
			return size
		}
	}
	return nl
}

// afterLastNewline is the offset just after the last '\n' in [0, size), or
// 0 when there is none.
func afterLastNewline(f io.ReaderAt, size int64) int64 {
	buf := make([]byte, min(int64(readChunk), size))
	for pos := size; pos > 0; {
		n := min(int64(len(buf)), pos)
		if _, err := f.ReadAt(buf[:n], pos-n); err != nil && !errors.Is(err, io.EOF) {
			return 0
		}
		if i := bytes.LastIndexByte(buf[:n], '\n'); i >= 0 {
			return pos - n + int64(i) + 1
		}
		pos -= n
	}
	return 0
}

// skipLine returns the offset after the next newline at or after from.
func skipLine(f io.ReaderAt, from, size int64) (int64, error) {
	buf := make([]byte, readChunk)
	for pos := from; pos < size; {
		n, err := f.ReadAt(buf, pos)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, nil
		}
		pos += int64(n)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return pos, nil
			}
			return from, err
		}
	}
	return size, nil
}

// reverseScanner yields the lines of r[0:end) from last to first. Lines
// longer than maxLine are skipped without being held in memory.
type reverseScanner struct {
	r   io.ReaderAt
	pos int64  // bytes before pos are unread
	buf []byte // data [pos, pos+len(buf)) not yet returned
	// skipping: the line being assembled exceeded maxLine; drop bytes until
	// its start is found.
	skipping bool
	done     bool
}

func newReverseScanner(r io.ReaderAt, end int64) *reverseScanner {
	return &reverseScanner{r: r, pos: end}
}

func (s *reverseScanner) atStart() bool { return s.done || (s.pos == 0 && len(s.buf) == 0) }

// prev returns the previous non-empty line and its start offset. The line
// aliases internal storage and is valid until the next call.
func (s *reverseScanner) prev() (line []byte, start int64, ok bool, err error) {
	for {
		if s.done {
			return nil, 0, false, nil
		}
		if i := bytes.LastIndexByte(s.buf, '\n'); i >= 0 {
			line, start = s.buf[i+1:], s.pos+int64(i)+1
			s.buf = s.buf[:i]
			if s.skipping {
				s.skipping = false
				continue
			}
			if len(line) > maxLine || len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			return line, start, true, nil
		}
		if s.pos == 0 {
			s.done = true
			line := s.buf
			s.buf = nil
			if s.skipping || len(line) > maxLine || len(bytes.TrimSpace(line)) == 0 {
				return nil, 0, false, nil
			}
			return line, 0, true, nil
		}
		if len(s.buf) >= maxLine {
			s.skipping = true
			s.buf = s.buf[:0]
		}
		// Grow geometrically so a long line costs O(length) copying.
		n := min(max(int64(readChunk), int64(len(s.buf))), s.pos)
		nb := make([]byte, n+int64(len(s.buf)))
		if _, err := s.r.ReadAt(nb[:n], s.pos-n); err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, false, err
		}
		copy(nb[n:], s.buf)
		s.buf = nb
		s.pos -= n
	}
}
