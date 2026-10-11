package browser

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/coder/websocket"
)

// maxCDPMessage bounds one CDP message: a screenshot of a large viewport
// in base64 is a few MiB.
const maxCDPMessage = 64 << 20

// cdpConn is a Chrome DevTools Protocol connection to one target over its
// WebSocket.
type cdpConn struct {
	ws      *websocket.Conn
	onEvent func(method string, params json.RawMessage) // on the read goroutine

	mu      sync.Mutex
	next    int64
	pending map[int64]chan cdpMsg
	done    chan struct{} // closed when the read loop ends
}

type cdpMsg struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *cdpError       `json:"error,omitempty"`
}

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return "cdp: " + e.Message }

var errCDPClosed = errors.New("cdp: connection closed")

// dialCDP connects to url. onEvent receives the target's events in order,
// on the read goroutine: it must not wait for a call's answer.
func dialCDP(ctx context.Context, url string, onEvent func(string, json.RawMessage)) (*cdpConn, error) {
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxCDPMessage)
	c := &cdpConn{ws: ws, onEvent: onEvent, pending: map[int64]chan cdpMsg{}, done: make(chan struct{})}
	return c, nil
}

// readLoop dispatches answers and events until the connection ends.
func (c *cdpConn) readLoop() {
	defer func() {
		c.mu.Lock()
		close(c.done)
		c.pending = nil
		c.mu.Unlock()
	}()
	for {
		_, b, err := c.ws.Read(context.Background())
		if err != nil {
			return
		}
		var m cdpMsg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.Method != "" {
			c.onEvent(m.Method, m.Params)
			continue
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		delete(c.pending, m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

// call sends method and waits for its answer, decoded into result (may
// be nil).
func (c *cdpConn) call(ctx context.Context, method string, params, result any) error {
	ch := make(chan cdpMsg, 1)
	if err := c.write(ctx, method, params, ch); err != nil {
		return err
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	case <-c.done:
		return errCDPClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// send sends method without waiting for its answer, which is dropped.
func (c *cdpConn) send(ctx context.Context, method string, params any) error {
	return c.write(ctx, method, params, nil)
}

func (c *cdpConn) write(ctx context.Context, method string, params any, answer chan cdpMsg) error {
	c.mu.Lock()
	if c.pending == nil {
		c.mu.Unlock()
		return errCDPClosed
	}
	c.next++
	id := c.next
	if answer != nil {
		c.pending[id] = answer
	}
	c.mu.Unlock()
	if params == nil {
		params = struct{}{}
	}
	b, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params})
	if err == nil {
		err = c.ws.Write(ctx, websocket.MessageText, b)
	}
	if err != nil {
		c.mu.Lock()
		if c.pending != nil {
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}
	return err
}

// close drops the connection at once (no closing handshake to wait for).
func (c *cdpConn) close() { c.ws.CloseNow() }
