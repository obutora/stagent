// Package bridge terminates the app protocol (PROTOCOL.md) on the stdin and
// stdout of `stagent bridge`, which the app runs over an SSH exec channel.
//
// hello, ping and session.spawn are answered here; daemon methods are
// forwarded verbatim over one daemon connection and session methods over one
// connection per session holder. Notifications from the daemon and the
// holders are relayed to the app unchanged. The bridge holds no state of
// its own that matters: when it exits, holders and agents keep running.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// Main runs `stagent bridge` until stdin closes.
func Main(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: stagent bridge  (speaks the app protocol on stdin/stdout)")
		return 2
	}
	l, err := paths.Resolve()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent bridge:", err)
		return 1
	}
	if err := New(l, os.Stdout).Serve(context.Background(), os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "stagent bridge:", err)
		return 1
	}
	return 0
}

const (
	daemonStartWait = 5 * time.Second
	holderDialWait  = 2 * time.Second
	spawnWait       = 10 * time.Second
	maxResubBackoff = 5 * time.Second
	resubscribeFor  = 2 * time.Minute
	drainWait       = 3 * time.Second
)

// Queue keys of the per-target workers (holders use "h:<session id>").
const (
	targetDaemon = "daemon"
	targetSpawn  = "spawn"
)

// Bridge serves one app connection.
type Bridge struct {
	l   *paths.Layout
	out *wire.Codec // to the app

	// Replaceable in tests.
	dialDaemon        func() (net.Conn, error) // starts the daemon if needed
	dialRunningDaemon func() (net.Conn, error)
	dialHolder        func(id string, timeout time.Duration) (net.Conn, error)
	spawnWait         time.Duration
	resubscribeFor    time.Duration

	envOnce  sync.Once
	baseEnv  []string                 // see sessionEnv
	loginEnv func() ([]string, error) // replaceable in tests

	ctx     context.Context
	cancel  context.CancelCauseFunc
	wg      sync.WaitGroup // every goroutine of the bridge
	workers sync.WaitGroup // per-target workers only

	dialMu    sync.Mutex // serializes daemon (re)connects
	rewatchMu sync.Mutex // one watch restoration at a time

	mu      sync.Mutex
	closed  bool
	queues  map[string]*queue
	holders map[string]*rpc.Client
	daemon  *daemonConn
	// What the app has been told through its watch, used to resubscribe
	// transparently when the daemon connection drops.
	lastSeq   int64
	known     map[string]bool
	watchLost bool // the app's watch died with a daemon connection
}

// daemonConn is one connection to the daemon.
type daemonConn struct {
	c        *rpc.Client
	watching bool // the app's watch is live on this connection (guarded by Bridge.mu)
}

// New creates a bridge writing app messages to w.
func New(l *paths.Layout, w io.Writer) *Bridge {
	ctx, cancel := context.WithCancelCause(context.Background())
	b := &Bridge{
		l:              l,
		spawnWait:      spawnWait,
		resubscribeFor: resubscribeFor,
		ctx:            ctx,
		cancel:         cancel,
		queues:         map[string]*queue{},
		holders:        map[string]*rpc.Client{},
		known:          map[string]bool{},
	}
	b.out = wire.NewCodec(nil, appWriter{w, b})
	b.dialDaemon = func() (net.Conn, error) { return daemonclient.DialOrStart(l, daemonStartWait) }
	b.dialRunningDaemon = func() (net.Conn, error) { return daemonclient.Dial(l, 300*time.Millisecond) }
	b.dialHolder = func(id string, timeout time.Duration) (net.Conn, error) {
		return ipc.Dial(l.HolderAddr(id), timeout)
	}
	b.loginEnv = func() ([]string, error) {
		exe, err := daemonclient.Exe()
		if err != nil {
			return nil, err
		}
		return captureLoginEnv(exe)
	}
	return b
}

// appWriter stops the bridge when the app side can no longer be written.
type appWriter struct {
	w io.Writer
	b *Bridge
}

func (a appWriter) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	if err != nil {
		a.b.cancel(fmt.Errorf("write to app: %w", err))
	}
	return n, err
}

// Serve reads app requests from r until it ends (EOF is a clean exit) or
// the app output fails, then closes every daemon and holder connection.
func (b *Bridge) Serve(ctx context.Context, r io.Reader) error {
	stop := context.AfterFunc(ctx, func() { b.cancel(ctx.Err()) })
	defer stop()

	in := wire.NewCodec(r, nil)
	readErr := make(chan error, 1)
	go func() {
		for {
			m, err := in.Read()
			if err != nil {
				var se *json.SyntaxError
				var te *json.UnmarshalTypeError
				if errors.As(err, &se) || errors.As(err, &te) {
					fmt.Fprintln(os.Stderr, "stagent bridge: dropping undecodable message:", err)
					continue
				}
				readErr <- err
				return
			}
			b.dispatch(m)
		}
	}()

	var err error
	select {
	case err = <-readErr:
		if errors.Is(err, io.EOF) {
			// The app is gone, but what it sent (e.g. input) still goes out.
			err = nil
			b.finishQueued(drainWait)
		}
	case <-b.ctx.Done():
		if cause := context.Cause(b.ctx); !errors.Is(cause, context.Canceled) {
			err = cause
		}
	}
	b.shutdown()
	return err
}

// finishQueued waits up to d for the queued requests to be handled.
func (b *Bridge) finishQueued(d time.Duration) {
	done := make(chan struct{})
	go func() {
		b.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-b.ctx.Done():
	case <-time.After(d):
	}
}

// shutdown closes all upstream connections and waits for the workers, which
// fail their in-flight requests as the connections go away.
func (b *Bridge) shutdown() {
	b.cancel(context.Canceled)
	b.mu.Lock()
	b.closed = true
	conns := make([]*rpc.Client, 0, len(b.holders)+1)
	for _, c := range b.holders {
		conns = append(conns, c)
	}
	if b.daemon != nil {
		conns = append(conns, b.daemon.c)
	}
	b.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	b.wg.Wait()
}

// dispatch routes one app message. It never blocks on an upstream: work
// for the daemon and for each holder is queued on that target's worker.
func (b *Bridge) dispatch(m *wire.Msg) {
	if !m.IsRequest() {
		return // the app sends no notifications; stray responses are ignored
	}
	switch {
	case m.Method == wire.MethodHello:
		res, err := b.hello(m)
		b.replyResult(m.ID, res, err)
	case m.Method == wire.MethodPing:
		b.replyResult(m.ID, struct{}{}, nil)
	case m.Method == wire.MethodSessionSpawn:
		b.enqueue(targetSpawn, m, b.spawn)
	case wire.DaemonMethods[m.Method]:
		b.enqueue(targetDaemon, m, b.forwardDaemon)
	case wire.HolderMethods[m.Method]:
		var ref wire.SessionRef
		if err := rpc.Decode(m, &ref); err != nil {
			b.replyResult(m.ID, nil, err)
			return
		}
		if !validSessionID(ref.ID) {
			b.replyResult(m.ID, nil, wire.Errorf(wire.ErrBadRequest, "%s: invalid session id %q", m.Method, ref.ID))
			return
		}
		b.enqueue("h:"+ref.ID, m, func(m *wire.Msg) { b.forwardHolder(ref.ID, m) })
	default:
		b.replyResult(m.ID, nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method))
	}
}

func (b *Bridge) hello(m *wire.Msg) (any, error) {
	var p wire.HelloParams
	if err := rpc.Decode(m, &p); err != nil {
		return nil, err
	}
	// A protocol mismatch is still answered with ours: the app decides
	// whether to offer an update.
	return wire.HelloResult{
		Protocol: version.Protocol,
		Version:  version.Version,
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Home:     b.l.Home,
		Capabilities: []string{
			wire.CapScreenMode, wire.CapSpawn, wire.CapHooks, wire.CapTranscript, wire.CapPush,
		},
	}, nil
}

// validSessionID accepts the 16 lowercase hex chars of wire.NewSessionID,
// which also keeps ids from escaping the holder socket directory.
func validSessionID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Replies

func (b *Bridge) replyResult(id *int64, result any, err error) {
	if err != nil {
		var we *wire.Error
		if !errors.As(err, &we) {
			we = wire.Errorf(wire.ErrInternal, "%v", err)
		}
		b.out.ReplyError(id, we)
		return
	}
	b.out.Reply(id, result)
}

// relayResponse answers the app request id with an upstream response.
func (b *Bridge) relayResponse(id *int64, resp *wire.Msg) {
	out := &wire.Msg{ID: id, Result: resp.Result, Error: resp.Error}
	if out.Error == nil && len(out.Result) == 0 {
		out.Result = json.RawMessage("{}")
	}
	b.out.Write(out)
}

// relay passes an upstream notification to the app unchanged.
func (b *Bridge) relay(m *wire.Msg) { b.out.Write(m) }

// ---------------------------------------------------------------------------
// Per-target FIFO workers

// queue is the backlog of one target. A worker goroutine exists only while
// the queue is non-empty.
type queue struct {
	jobs    []job
	running bool
}

type job struct {
	m  *wire.Msg
	fn func(*wire.Msg)
}

// enqueue runs fn(m) after every earlier job of the same target has
// finished. Targets are independent, so a slow daemon call never delays
// input to a holder.
func (b *Bridge) enqueue(key string, m *wire.Msg, fn func(*wire.Msg)) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	q := b.queues[key]
	if q == nil {
		q = &queue{}
		b.queues[key] = q
	}
	q.jobs = append(q.jobs, job{m, fn})
	start := !q.running
	q.running = true
	if start {
		b.wg.Add(1)
		b.workers.Add(1)
	}
	b.mu.Unlock()
	if start {
		go b.drain(key, q)
	}
}

func (b *Bridge) drain(key string, q *queue) {
	defer b.wg.Done()
	defer b.workers.Done()
	for {
		b.mu.Lock()
		if len(q.jobs) == 0 {
			q.running = false
			delete(b.queues, key)
			b.mu.Unlock()
			return
		}
		j := q.jobs[0]
		q.jobs[0] = job{}
		q.jobs = q.jobs[1:]
		b.mu.Unlock()
		j.fn(j.m)
	}
}

// forward sends m upstream and relays the answer, returning once the app
// has been answered. The response is written on the upstream's read
// goroutine, so it reaches the app before any notification the upstream
// sent after it (e.g. an attach result before the first output).
func (b *Bridge) forward(c *rpc.Client, m *wire.Msg, lost *wire.Error, onResult func(*wire.Msg)) {
	done := make(chan struct{})
	c.Go(m.Method, m.Params, func(resp *wire.Msg) {
		defer close(done)
		if resp == nil {
			b.out.ReplyError(m.ID, lost)
			return
		}
		if onResult != nil && resp.Error == nil {
			onResult(resp)
		}
		b.relayResponse(m.ID, resp)
	})
	<-done
}

// ---------------------------------------------------------------------------
// Holders

func (b *Bridge) forwardHolder(id string, m *wire.Msg) {
	c, err := b.holderConn(id)
	if err != nil {
		b.out.ReplyError(m.ID, wire.Errorf(wire.ErrNotFound, "session %s: no holder answers", id))
		return
	}
	b.forward(c, m, wire.Errorf(wire.ErrNotFound, "session %s: holder went away", id), nil)
}

// holderConn returns the cached connection to a session's holder, dialing
// it on first use.
func (b *Bridge) holderConn(id string) (*rpc.Client, error) {
	b.mu.Lock()
	c := b.holders[id]
	b.mu.Unlock()
	if c != nil && !isDone(c) {
		return c, nil
	}
	conn, err := b.dialHolder(id, holderDialWait)
	if err != nil {
		return nil, err
	}
	return b.adoptHolder(id, rpc.NewClient(conn, b.relay))
}

// adoptHolder caches c as the connection for session id, dropping it from
// the cache when it closes.
func (b *Bridge) adoptHolder(id string, c *rpc.Client) (*rpc.Client, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		c.Close()
		return nil, errors.New("bridge closed")
	}
	if old := b.holders[id]; old != nil && old != c && !isDone(old) {
		b.mu.Unlock()
		c.Close()
		return old, nil
	}
	b.holders[id] = c
	b.mu.Unlock()
	go func() {
		<-c.Done()
		b.mu.Lock()
		if b.holders[id] == c {
			delete(b.holders, id)
		}
		b.mu.Unlock()
	}()
	return c, nil
}

func isDone(c *rpc.Client) bool {
	select {
	case <-c.Done():
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// Daemon

func (b *Bridge) forwardDaemon(m *wire.Msg) {
	dc, err := b.daemonConn(b.dialDaemon)
	if err != nil {
		b.out.ReplyError(m.ID, wire.Errorf(wire.ErrUnavailable, "daemon: %v", err))
		return
	}
	var onResult func(*wire.Msg)
	if m.Method == wire.MethodWatch {
		onResult = func(resp *wire.Msg) { b.noteWatch(dc, resp.Result, false) }
	} else {
		// A lost watch comes back before the request that reconnected.
		b.rewatch(dc)
	}
	b.forward(dc.c, m, wire.Errorf(wire.ErrUnavailable, "daemon connection lost"), onResult)
}

// daemonConn returns the daemon connection, (re)connecting lazily with dial.
func (b *Bridge) daemonConn(dial func() (net.Conn, error)) (*daemonConn, error) {
	b.dialMu.Lock()
	defer b.dialMu.Unlock()
	b.mu.Lock()
	dc := b.daemon
	b.mu.Unlock()
	if dc != nil && !isDone(dc.c) {
		return dc, nil
	}
	conn, err := dial()
	if err != nil {
		return nil, err
	}
	dc = &daemonConn{}
	dc.c = rpc.NewClient(conn, func(m *wire.Msg) { b.onDaemonNotify(m) })
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		dc.c.Close()
		return nil, errors.New("bridge closed")
	}
	b.daemon = dc
	b.wg.Add(1)
	b.mu.Unlock()
	go b.watchDaemon(dc)
	return dc, nil
}

// watchDaemon waits for dc to drop. If the app's watch lived on it, the
// subscription is restored on a new connection (see resubscribe).
func (b *Bridge) watchDaemon(dc *daemonConn) {
	defer b.wg.Done()
	<-dc.c.Done()
	b.mu.Lock()
	if b.daemon == dc {
		b.daemon = nil
	}
	lost := dc.watching && !b.closed
	if lost {
		b.watchLost = true
	}
	b.mu.Unlock()
	if lost {
		b.resubscribe()
	}
}

// onDaemonNotify relays a daemon notification, keeping track of what the
// app knows so a resubscription can report only the difference.
func (b *Bridge) onDaemonNotify(m *wire.Msg) {
	switch m.Method {
	case wire.NotifyEvent:
		var ev struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(m.Params, &ev) == nil {
			b.mu.Lock()
			b.lastSeq = max(b.lastSeq, ev.Seq)
			b.mu.Unlock()
		}
	case wire.NotifySessionUpdated, wire.NotifySessionRemoved:
		var ref wire.SessionRef
		if json.Unmarshal(m.Params, &ref) == nil && ref.ID != "" {
			b.mu.Lock()
			if m.Method == wire.NotifySessionUpdated {
				b.known[ref.ID] = true
			} else {
				delete(b.known, ref.ID)
			}
			b.mu.Unlock()
		}
	}
	b.relay(m)
}

// noteWatch records a successful watch on dc. For the app's own watch the
// result replaces what the app knows; for a resubscription (publish) the
// difference is sent as notifications instead. It runs on dc's read
// goroutine, ahead of any later notification of that connection.
func (b *Bridge) noteWatch(dc *daemonConn, result json.RawMessage, publish bool) {
	var res wire.WatchResult
	if err := json.Unmarshal(result, &res); err != nil {
		return
	}
	b.mu.Lock()
	dc.watching = true
	b.watchLost = false
	b.lastSeq = max(b.lastSeq, res.Seq)
	for _, ev := range res.Missed {
		b.lastSeq = max(b.lastSeq, ev.Seq)
	}
	var gone []string
	current := make(map[string]bool, len(res.Sessions))
	for _, s := range res.Sessions {
		current[s.ID] = true
	}
	if publish {
		for id := range b.known {
			if !current[id] {
				gone = append(gone, id)
			}
		}
	}
	b.known = current
	b.mu.Unlock()
	if !publish {
		return
	}
	for _, ev := range res.Missed {
		b.out.Notify(wire.NotifyEvent, ev)
	}
	for _, s := range res.Sessions {
		b.out.Notify(wire.NotifySessionUpdated, s)
	}
	for _, id := range gone {
		b.out.Notify(wire.NotifySessionRemoved, wire.SessionRef{ID: id})
	}
}

// resubscribe restores the app's watch after the daemon connection dropped
// (daemon restart, update or crash). The daemon comes back through the
// holders' reconnect loops or the login service; the bridge only waits for
// it — it never restarts a daemon that was stopped on purpose — for
// resubscribeFor. After that the next app request to the daemon reconnects
// and restores the watch (forwardDaemon). Requests that were in flight when
// the connection dropped already failed as unavailable.
func (b *Bridge) resubscribe() {
	backoff := 100 * time.Millisecond
	deadline := time.Now().Add(b.resubscribeFor)
	for time.Now().Before(deadline) {
		select {
		case <-b.ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxResubBackoff)
		b.mu.Lock()
		lost := b.watchLost
		b.mu.Unlock()
		if !lost {
			return
		}
		dc, err := b.daemonConn(b.dialRunningDaemon)
		if err != nil {
			continue
		}
		if b.rewatch(dc) {
			return
		}
	}
}

// rewatch re-issues a lost app watch on dc, replaying everything after the
// last event the app saw as ordinary notifications, so the app needs no
// daemon-restart handling. It reports false if dc dropped before answering;
// without a lost watch it does nothing.
func (b *Bridge) rewatch(dc *daemonConn) bool {
	b.rewatchMu.Lock()
	defer b.rewatchMu.Unlock()
	b.mu.Lock()
	lost, since := b.watchLost, b.lastSeq
	b.mu.Unlock()
	if !lost {
		return true
	}
	params, _ := json.Marshal(wire.WatchParams{Since: since})
	done := make(chan bool, 1)
	dc.c.Go(wire.MethodWatch, params, func(resp *wire.Msg) {
		switch {
		case resp == nil:
			done <- false
		case resp.Error != nil:
			fmt.Fprintln(os.Stderr, "stagent bridge: restoring watch:", resp.Error)
			b.mu.Lock()
			b.watchLost = false
			b.mu.Unlock()
			done <- true
		default:
			b.noteWatch(dc, resp.Result, true)
			done <- true
		}
	})
	return <-done
}
