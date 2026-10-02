package attachcli

import (
	"bytes"
	"time"
	"unicode/utf8"
)

const (
	// holdFlush releases bytes held back for being a possible split query
	// reply or a split UTF-8 sequence once no more input followed: replies
	// and the rest of a rune arrive within milliseconds, a bare ESC
	// keypress must not wait noticeably.
	holdFlush = 50 * time.Millisecond
	// detachWindow is how long a detach key press waits for a second
	// press. A terminal delivers one keypress per read, so "pressed twice
	// in a row" cannot be told from "pressed once" without waiting; one
	// press therefore detaches after this delay, two within it send the
	// key once to the program.
	detachWindow = 300 * time.Millisecond
)

// input turns local terminal reads into session.input text: it drops query
// replies (replyFilter), handles the detach key and keeps UTF-8 sequences
// whole across reads — the text travels as a JSON string, in which a split
// rune would arrive as two U+FFFD.
type input struct {
	filter replyFilter
	key    byte
	// armedAt is when a lone detach key press was read (zero: none
	// pending). The next byte decides: the key again sends it literally,
	// anything else detaches and is discarded, as does detachWindow
	// passing.
	armedAt time.Time
	heldAt  time.Time // last read that left bytes held back
	tail    []byte    // incomplete UTF-8 sequence ending the last read
	filt    []byte    // scratch: filter output
	buf     []byte    // result of feed / expire, valid until the next call
}

func newInput(key byte) *input { return &input{key: key} }

// feed processes one read and returns the text to send now; detach reports
// that the user detached (the text, typed before the key, is still sent).
func (in *input) feed(b []byte, now time.Time) (text []byte, detach bool) {
	in.filt = in.filter.filter(in.filt[:0], b)
	text, detach = in.keys(in.filt, now)
	if in.holding() {
		in.heldAt = now
	}
	return text, detach
}

// deadline is when expire must run next, if anything is pending.
func (in *input) deadline() (time.Time, bool) {
	var d time.Time
	if in.holding() {
		d = in.heldAt.Add(holdFlush)
	}
	if !in.armedAt.IsZero() {
		if a := in.armedAt.Add(detachWindow); d.IsZero() || a.Before(d) {
			d = a
		}
	}
	return d, !d.IsZero()
}

// expire releases held bytes and ends the detach window once their time
// has come.
func (in *input) expire(now time.Time) (text []byte, detach bool) {
	in.buf = in.buf[:0]
	text = in.buf
	if in.holding() && !now.Before(in.heldAt.Add(holdFlush)) {
		in.filt = in.filter.flush(in.filt[:0])
		if text, detach = in.keys(in.filt, now); detach {
			return text, true
		}
		// A partial rune that timed out goes out as is: nothing will
		// complete it.
		in.buf = append(text, in.tail...)
		in.tail = in.tail[:0]
		text = in.buf
	}
	if !in.armedAt.IsZero() && !now.Before(in.armedAt.Add(detachWindow)) {
		in.armedAt = time.Time{}
		return text, true
	}
	return text, false
}

func (in *input) holding() bool { return in.filter.holding() || len(in.tail) > 0 }

// keys applies the detach key to filtered bytes b and returns the text to
// send in in.buf, with an incomplete trailing rune moved to in.tail (unless
// a key press followed it: then nothing can complete it).
func (in *input) keys(b []byte, now time.Time) ([]byte, bool) {
	in.buf = append(in.buf[:0], in.tail...)
	in.tail = in.tail[:0]
	for len(b) > 0 {
		if !in.armedAt.IsZero() {
			in.armedAt = time.Time{}
			if b[0] != in.key {
				return in.buf, true
			}
			in.buf = append(in.buf, in.key)
			b = b[1:]
			continue
		}
		i := bytes.IndexByte(b, in.key)
		if i < 0 {
			in.buf = append(in.buf, b...)
			break
		}
		in.buf = append(in.buf, b[:i]...)
		b = b[i+1:]
		in.armedAt = now
	}
	if n := incompleteRune(in.buf); n > 0 && in.armedAt.IsZero() {
		in.tail = append(in.tail, in.buf[len(in.buf)-n:]...)
		in.buf = in.buf[:len(in.buf)-n]
	}
	return in.buf, false
}

// incompleteRune returns the length of a UTF-8 sequence cut off at the end
// of b (0 when b ends on a rune boundary or with invalid bytes, which no
// later byte can fix).
func incompleteRune(b []byte) int {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(b); i++ {
		c := b[len(b)-i]
		if c < utf8.RuneSelf {
			return 0
		}
		if utf8.RuneStart(c) {
			if utf8.FullRune(b[len(b)-i:]) {
				return 0
			}
			return i
		}
	}
	return 0
}
