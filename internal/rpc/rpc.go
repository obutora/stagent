// Package rpc runs request/response/notification exchanges over a
// wire.Codec: a Client for the calling side and Serve for the serving side.
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/obutora/stagent/internal/wire"
)

// ErrClosed is returned by calls on a closed client.
var ErrClosed = errors.New("rpc: connection closed")

// Client issues requests and dispatches incoming notifications.
type Client struct {
	codec  *wire.Codec
	closer io.Closer

	mu      sync.Mutex
	next    int64
	pending map[int64]func(*wire.Msg) // nil argument: connection ended
	err     error
	done    chan struct{}
}

// NewClient starts reading rw. onNotify (may be nil) receives every
// notification, sequentially, on the read goroutine — it must not block for
// long. Incoming requests are answered with unknown_method.
func NewClient(rw io.ReadWriteCloser, onNotify func(*wire.Msg)) *Client {
	c := &Client{
		codec:   wire.NewCodec(rw, rw),
		closer:  rw,
		pending: map[int64]func(*wire.Msg){},
		done:    make(chan struct{}),
	}
	go c.readLoop(onNotify)
	return c
}

func (c *Client) readLoop(onNotify func(*wire.Msg)) {
	var err error
	for {
		var m *wire.Msg
		m, err = c.codec.Read()
		if err != nil {
			break
		}
		switch {
		case m.IsResponse():
			c.mu.Lock()
			f := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if f != nil {
				f(m)
			}
		case m.IsRequest():
			c.codec.ReplyError(m.ID, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method))
		default:
			if onNotify != nil {
				onNotify(m)
			}
		}
	}
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	orphans := make([]func(*wire.Msg), 0, len(c.pending))
	for id, f := range c.pending {
		orphans = append(orphans, f)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	for _, f := range orphans {
		f(nil)
	}
	close(c.done)
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is the reason the connection ended (after Done).
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close closes the underlying connection.
func (c *Client) Close() error { return c.closer.Close() }

// Codec exposes the codec (e.g. to write pre-encoded messages).
func (c *Client) Codec() *wire.Codec { return c.codec }

// CallRaw sends a request with raw params and returns the raw response
// message (Result or Error set). Used by the bridge to forward verbatim.
func (c *Client) CallRaw(ctx context.Context, method string, params json.RawMessage) (*wire.Msg, error) {
	ch := make(chan *wire.Msg, 1)
	id, err := c.send(method, params, func(m *wire.Msg) { ch <- m })
	if err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if m == nil {
			return nil, ErrClosed
		}
		return m, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Go sends a request without waiting for it. onResponse is called exactly
// once: with the response on the read goroutine — before any later message
// of the connection is dispatched, so a forwarded response stays ordered
// with the notifications around it — or with nil when the request could not
// be sent or the connection ended first. It must not block for long.
func (c *Client) Go(method string, params json.RawMessage, onResponse func(*wire.Msg)) {
	if _, err := c.send(method, params, onResponse); err != nil {
		onResponse(nil)
	}
}

// send registers f for the response and writes the request. On error f is
// unregistered and will not be called.
func (c *Client) send(method string, params json.RawMessage, f func(*wire.Msg)) (int64, error) {
	c.mu.Lock()
	if c.err != nil || isClosed(c.done) {
		c.mu.Unlock()
		return 0, ErrClosed
	}
	c.next++
	id := c.next
	c.pending[id] = f
	c.mu.Unlock()

	if err := c.codec.Write(&wire.Msg{ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		_, owned := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if !owned {
			// The read loop already took f (connection ended meanwhile) and
			// calls or called it; report success so the caller does not
			// handle the failure twice.
			return id, nil
		}
		return 0, err
	}
	return id, nil
}

// Call sends a request and decodes the result into result (may be nil).
// A protocol error is returned as *wire.Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	var p json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return err
		}
		p = b
	}
	m, err := c.CallRaw(ctx, method, p)
	if err != nil {
		return err
	}
	if m.Error != nil {
		return m.Error
	}
	if result != nil && len(m.Result) > 0 {
		return json.Unmarshal(m.Result, result)
	}
	return nil
}

// Notify sends a notification.
func (c *Client) Notify(method string, params any) error {
	return c.codec.Notify(method, params)
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Serving

// Async, returned by a Handler as its result, means the handler will answer
// the request itself later via Conn.Reply / Conn.ReplyError (typically from
// a goroutine, so a slow request does not block the connection).
var Async = &struct{ async bool }{true}

// Handler handles one request or notification. For notifications (m.ID ==
// nil) the return values are ignored. Returning a *wire.Error sends that
// error; any other error is sent as "internal".
type Handler func(ctx context.Context, c *Conn, m *wire.Msg) (any, error)

// Conn is the serving side of one connection.
type Conn struct {
	*wire.Codec
	closer io.Closer
	ctx    context.Context
	cancel context.CancelFunc
}

// Context is cancelled when the connection ends.
func (c *Conn) Context() context.Context { return c.ctx }

// Close ends the connection.
func (c *Conn) Close() error {
	c.cancel()
	return c.closer.Close()
}

// ReplyResult answers id with result or err (same rules as Handler).
func (c *Conn) ReplyResult(id *int64, result any, err error) error {
	if id == nil {
		return nil
	}
	if err != nil {
		var we *wire.Error
		if errors.As(err, &we) {
			return c.ReplyError(id, we)
		}
		return c.ReplyError(id, wire.Errorf(wire.ErrInternal, "%v", err))
	}
	return c.Reply(id, result)
}

// Serve reads requests from rw and dispatches them to h sequentially, in
// arrival order, until the connection ends or ctx is cancelled. It closes rw
// before returning.
func Serve(ctx context.Context, rw io.ReadWriteCloser, h Handler) error {
	cctx, cancel := context.WithCancel(ctx)
	c := &Conn{Codec: wire.NewCodec(rw, rw), closer: rw, ctx: cctx, cancel: cancel}
	defer c.Close()
	go func() {
		<-cctx.Done()
		rw.Close()
	}()
	for {
		m, err := c.Read()
		if err != nil {
			if errors.Is(err, io.EOF) || cctx.Err() != nil {
				return nil
			}
			return err
		}
		if m.Method == "" {
			continue // stray response
		}
		res, herr := h(cctx, c, m)
		if m.ID == nil || res == Async {
			continue
		}
		if err := c.ReplyResult(m.ID, res, herr); err != nil {
			return err
		}
	}
}

// Decode unmarshals request params into v, mapping failures to bad_request.
func Decode(m *wire.Msg, v any) error {
	if len(m.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(m.Params, v); err != nil {
		return wire.Errorf(wire.ErrBadRequest, "%s: %v", m.Method, err)
	}
	return nil
}
