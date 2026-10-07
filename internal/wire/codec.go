// Package wire is the message envelope and codec shared by every stagent
// connection: app ↔ bridge (over the SSH exec channel) and the local IPC
// links (holder, daemon, hook, bridge). One JSON object per line.
package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"unicode/utf8"
)

// MaxLine bounds a single message. Output chunks are capped well below this
// by their producers; anything larger is a protocol violation.
const MaxLine = 16 << 20

// Msg is the envelope. Requests carry ID+Method(+Params), responses ID and
// Result or Error, notifications Method(+Params) without ID.
type Msg struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// IsRequest reports whether m expects a response.
func (m *Msg) IsRequest() bool { return m.ID != nil && m.Method != "" }

// IsResponse reports whether m answers an earlier request.
func (m *Msg) IsResponse() bool { return m.ID != nil && m.Method == "" }

// Error is a protocol-level failure. Code is a stable machine-readable
// string (see the Err* constants); Message is for humans.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Stable error codes.
const (
	ErrBadRequest    = "bad_request"
	ErrUnknownMethod = "unknown_method"
	ErrNotFound      = "not_found"
	ErrUnsupported   = "unsupported"
	ErrInternal      = "internal"
	ErrUnavailable   = "unavailable"
	ErrVersion       = "version_mismatch"
	ErrSessionEnded  = "session_ended"
	ErrNotSizeOwner  = "not_size_owner"
	ErrNotConfigured = "not_configured"
	// ErrAgentRefused: the connection comes from a coding agent's process
	// tree or, on macOS, a sandbox (ADR 0004); the holder refuses every
	// method, the daemon the ones that change settings or stop it.
	ErrAgentRefused = "agent_refused"
	// ErrMenuOpen: session.input with paste and submit was not written
	// because an approval menu is on the screen.
	ErrMenuOpen = "menu_open"
	// ErrForeignOwner: the stagent location (socket, run directory, named
	// pipe) belongs to another user; nothing was sent to it.
	ErrForeignOwner = "foreign_owner"
)

// Errorf builds an *Error.
func Errorf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Codec reads and writes newline-delimited JSON messages. Write is safe for
// concurrent use; Read must be called from one goroutine.
type Codec struct {
	r  *bufio.Reader
	mu sync.Mutex
	w  io.Writer
}

// NewCodec wraps a reader and writer (often the same net.Conn).
func NewCodec(r io.Reader, w io.Writer) *Codec {
	return &Codec{r: bufio.NewReaderSize(r, 64<<10), w: w}
}

// ErrTooLong is returned by Read for a line over MaxLine.
var ErrTooLong = errors.New("wire: message exceeds MaxLine")

// Read decodes the next message. Blank lines are skipped. A trailing '\r'
// is tolerated so CRLF-translating transports still work.
func (c *Codec) Read() (*Msg, error) {
	line, err := c.ReadLine()
	if err != nil {
		return nil, err
	}
	var m Msg
	if err := json.Unmarshal(line, &m); err != nil {
		return nil, fmt.Errorf("wire: decode: %w", err)
	}
	return &m, nil
}

// ReadLine returns the next non-blank line without its line ending, for
// line protocols whose objects are not Msg envelopes (`stagent follow`).
// Blank lines are skipped and a trailing '\r' is removed, as for Read.
func (c *Codec) ReadLine() ([]byte, error) {
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) != 0 {
			return line, nil
		}
	}
}

func (c *Codec) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := c.r.ReadSlice('\n')
		if len(buf)+len(chunk) > MaxLine {
			return nil, ErrTooLong
		}
		buf = append(buf, chunk...)
		if err == nil {
			return buf[:len(buf)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(buf) > 0 {
			return buf, nil
		}
		return nil, err
	}
}

// Write encodes m as one line.
func (c *Codec) Write(m *Msg) error { return c.Encode(m) }

// Encode writes v as one JSON line. The output is pure ASCII (non-ASCII
// runes are \u-escaped) so shells or consoles that re-encode stdout —
// PowerShell on Windows — cannot corrupt it.
func (c *Codec) Encode(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = asciiEscape(b)
	b = append(b, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// Request sends a request with the given id.
func (c *Codec) Request(id int64, method string, params any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.Write(&Msg{ID: &id, Method: method, Params: p})
}

// Notify sends a notification.
func (c *Codec) Notify(method string, params any) error {
	p, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.Write(&Msg{Method: method, Params: p})
}

// Reply answers request id with result (nil → {}).
func (c *Codec) Reply(id *int64, result any) error {
	if result == nil {
		result = struct{}{}
	}
	r, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return c.Write(&Msg{ID: id, Result: r})
}

// ReplyError answers request id with an error.
func (c *Codec) ReplyError(id *int64, e *Error) error {
	return c.Write(&Msg{ID: id, Error: e})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}

// asciiEscape rewrites non-ASCII runes in JSON text as \uXXXX escapes
// (surrogate pairs above the BMP). Only valid inside JSON strings, which is
// the only place json.Marshal emits non-ASCII.
func asciiEscape(b []byte) []byte {
	ascii := true
	for _, c := range b {
		if c >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return b
	}
	out := make([]byte, 0, len(b)+len(b)/4)
	const hex = "0123456789abcdef"
	put := func(r rune) {
		out = append(out, '\\', 'u', hex[r>>12&0xF], hex[r>>8&0xF], hex[r>>4&0xF], hex[r&0xF])
	}
	for len(b) > 0 {
		if b[0] < 0x80 {
			out = append(out, b[0])
			b = b[1:]
			continue
		}
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		if r >= 0x10000 {
			r -= 0x10000
			put(0xD800 + (r>>10)&0x3FF)
			put(0xDC00 + r&0x3FF)
			continue
		}
		put(r)
	}
	return out
}
