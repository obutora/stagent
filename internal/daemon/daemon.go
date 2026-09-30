// Package daemon is `stagent daemon`: the per-user session registry, event
// log, hook integration (state, notifications, blocking approvals), push
// notifications, transcript access and scrollback retention. It never sees
// PTY bytes — holders keep the terminal and report state transitions only.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/eventlog"
	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// Options configures a Daemon. Zero fields take defaults.
type Options struct {
	Layout *paths.Layout
	// Roots of the harness transcripts (default: transcript.DefaultRoots).
	Roots *transcript.Roots
	// GoneGrace: a holder connection that drops without holder.ended counts
	// as a lost session unless the holder re-registers within this time.
	GoneGrace time.Duration
	// EndedLinger: how long an ended session stays listed.
	EndedLinger time.Duration
	// TranscriptPoll: tail interval while a transcript is subscribed.
	TranscriptPoll time.Duration
	Logf           func(format string, args ...any)
}

const (
	defaultGoneGrace      = 3 * time.Second
	defaultEndedLinger    = 10 * time.Minute
	defaultTranscriptPoll = time.Second
	// outboxSize bounds the messages queued for one connection; a bridge
	// that falls this far behind is disconnected (it resyncs with `since`).
	outboxSize = 1024
)

// Daemon is one running daemon.
type Daemon struct {
	opts    Options
	layout  *paths.Layout
	log     *eventlog.Log
	sender  *notify.Sender
	digest  *notify.Digester
	index   *transcript.Index
	roots   transcript.Roots
	started time.Time
	logf    func(format string, args ...any)

	mu        sync.Mutex
	cfg       wire.Config // as stored (zeros = defaults)
	eff       wire.Config // cfg.WithDefaults()
	sessions  map[string]*session
	approvals map[string]*approval
	watchers  map[*connState]struct{}
	conns     map[*connState]struct{}
	tails     map[*tailSub]struct{}
	tailing   bool
	sweep     *time.Timer
	closed    bool

	shutdown     chan struct{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup
}

// New opens the event log and loads the configuration.
func New(opts Options) (*Daemon, error) {
	if opts.Layout == nil {
		return nil, errors.New("daemon: no layout")
	}
	if opts.GoneGrace <= 0 {
		opts.GoneGrace = defaultGoneGrace
	}
	if opts.EndedLinger <= 0 {
		opts.EndedLinger = defaultEndedLinger
	}
	if opts.TranscriptPoll <= 0 {
		opts.TranscriptPoll = defaultTranscriptPoll
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	l := opts.Layout
	if err := l.EnsureDirs(); err != nil {
		return nil, err
	}
	cfg, err := loadConfig(l.Config)
	if err != nil {
		opts.Logf("daemon: config: %v (using defaults)", err)
	}
	eff := cfg.WithDefaults()
	elog, err := eventlog.Open(l.EventLog, eventlog.Options{
		MaxEvents: eff.Retention.EventsMax,
		MaxAge:    time.Duration(eff.Retention.EventsDays) * 24 * time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: event log: %w", err)
	}
	roots := transcript.DefaultRoots(l.Home, os.Getenv)
	if opts.Roots != nil {
		roots = *opts.Roots
	}
	d := &Daemon{
		opts:      opts,
		layout:    l,
		log:       elog,
		sender:    notify.NewSender(opts.Logf),
		index:     transcript.OpenIndex(l.Index, roots),
		roots:     roots,
		started:   time.Now(),
		logf:      opts.Logf,
		cfg:       cfg,
		eff:       eff,
		sessions:  map[string]*session{},
		approvals: map[string]*approval{},
		watchers:  map[*connState]struct{}{},
		conns:     map[*connState]struct{}{},
		tails:     map[*tailSub]struct{}{},
		shutdown:  make(chan struct{}),
	}
	d.digest = notify.NewDigester(func(n wire.NotificationData) {
		d.mu.Lock()
		cfg := d.eff.Notify
		d.mu.Unlock()
		d.sender.Send(cfg, n)
	})
	return d, nil
}

// Serve accepts connections on ln until Shutdown. It closes ln.
func (d *Daemon) Serve(ln net.Listener) error {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.runSweep()
	}()
	go func() {
		<-d.shutdown
		ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			select {
			case <-d.shutdown:
				return nil
			default:
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.serveConn(c)
		}()
	}
}

// Shutdown stops accepting, closes every connection and releases resources.
// Pending approvals resolve with no decision. Safe to call more than once.
func (d *Daemon) Shutdown() {
	d.shutdownOnce.Do(func() {
		close(d.shutdown)
		d.mu.Lock()
		d.closed = true
		for _, ap := range d.approvals {
			d.resolveLocked(ap, wire.DecisionNone, "cancelled", "")
		}
		for _, s := range d.sessions {
			s.stopTimers()
		}
		if d.sweep != nil {
			d.sweep.Stop()
		}
		conns := make([]*connState, 0, len(d.conns))
		var holders []*rpc.Conn
		for cs := range d.conns {
			conns = append(conns, cs)
			if cs.holderID != "" && cs.c != nil {
				holders = append(holders, cs.c)
			}
		}
		d.mu.Unlock()
		// Tell holders this stop is deliberate so they do not start a new
		// daemon while reconnecting (see holder daemonLink).
		for _, c := range holders {
			c.Notify(wire.MethodDaemonStopping, nil)
		}
		for _, cs := range conns {
			cs.nc.Close()
		}
		d.wg.Wait()
		d.digest.Flush()
		d.sender.Close()
		if err := d.log.Close(); err != nil {
			d.logf("daemon: event log: %v", err)
		}
	})
}

// Done is closed when Shutdown starts (daemon.shutdown, signals).
func (d *Daemon) Done() <-chan struct{} { return d.shutdown }

// ---------------------------------------------------------------------------
// Connections

// connState is one connection: a holder, a hook, a bridge or a CLI.
type connState struct {
	nc net.Conn
	c  *rpc.Conn // set by the first message

	// Guarded by Daemon.mu.
	out      *outbox
	holderID string
	subs     []*tailSub
}

func (d *Daemon) serveConn(nc net.Conn) {
	cs := &connState{nc: nc}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		nc.Close()
		return
	}
	d.conns[cs] = struct{}{}
	d.mu.Unlock()
	err := rpc.Serve(context.Background(), nc, func(ctx context.Context, c *rpc.Conn, m *wire.Msg) (any, error) {
		if cs.c == nil {
			d.mu.Lock()
			cs.c = c
			d.mu.Unlock()
		}
		return d.handle(ctx, cs, m)
	})
	if err != nil && !errors.Is(err, net.ErrClosed) {
		d.logf("daemon: connection: %v", err)
	}
	d.connClosed(cs)
}

func (d *Daemon) handle(ctx context.Context, cs *connState, m *wire.Msg) (any, error) {
	switch m.Method {
	// holder → daemon
	case wire.MethodHolderRegister:
		var s wire.Session
		if err := rpc.Decode(m, &s); err != nil {
			return nil, err
		}
		return d.holderRegister(cs, s)
	case wire.MethodHolderUpdate:
		var p wire.SessionPatch
		if rpc.Decode(m, &p) == nil {
			d.holderUpdate(cs, p)
		}
		return nil, nil
	case wire.MethodHolderNotify:
		var p wire.HolderNotifyParams
		if rpc.Decode(m, &p) == nil {
			d.holderNotify(cs, p)
		}
		return nil, nil
	case wire.MethodHolderEnded:
		var p wire.ClosedParams
		if rpc.Decode(m, &p) == nil {
			d.holderEnded(cs, p)
		}
		return nil, nil

	// hook → daemon
	case wire.MethodHookEvent:
		var p wire.HookEventParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return d.hookEvent(cs, m.ID, p)

	// bridge → daemon
	case wire.MethodPing:
		return struct{}{}, nil
	case wire.MethodWatch:
		var p wire.WatchParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		d.watch(cs, m.ID, p)
		return rpc.Async, nil
	case wire.MethodSessionsList:
		return wire.SessionsListResult{Sessions: d.sessionList()}, nil
	case wire.MethodConversationsList:
		var p wire.ConversationsListParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return d.conversations(p)
	case wire.MethodTranscriptGet:
		var p wire.TranscriptGetParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return d.transcriptGet(p)
	case wire.MethodTranscriptSubscribe:
		var p wire.TranscriptSubscribeParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return struct{}{}, d.transcriptSubscribe(cs, p)
	case wire.MethodTranscriptUnsubscribe:
		var p wire.TranscriptSubscribeParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		d.transcriptUnsubscribe(cs, p)
		return struct{}{}, nil
	case wire.MethodApprovalsList:
		return wire.ApprovalsListResult{Approvals: d.approvalList()}, nil
	case wire.MethodApprovalRespond:
		var p wire.ApprovalRespondParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return struct{}{}, d.approvalRespond(p)
	case wire.MethodConfigGet:
		d.mu.Lock()
		defer d.mu.Unlock()
		return wire.ConfigResult{Config: d.eff}, nil
	case wire.MethodConfigSet:
		var p wire.ConfigSetParams
		if err := rpc.Decode(m, &p); err != nil {
			return nil, err
		}
		return d.configSet(p.Config)
	case wire.MethodNotifyTest:
		d.notifyTest(cs.c, m.ID)
		return rpc.Async, nil

	// install / doctor → daemon
	case wire.MethodDaemonStatus:
		return d.status(), nil
	case wire.MethodDaemonShutdown:
		cs.c.ReplyResult(m.ID, struct{}{}, nil)
		go d.Shutdown()
		return rpc.Async, nil
	}
	if m.ID == nil {
		return nil, nil
	}
	return nil, wire.Errorf(wire.ErrUnknownMethod, "%s", m.Method)
}

func (d *Daemon) connClosed(cs *connState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.conns, cs)
	delete(d.watchers, cs)
	if cs.out != nil {
		cs.out.stop()
	}
	for _, sub := range cs.subs {
		delete(d.tails, sub)
	}
	cs.subs = nil
	if cs.holderID != "" {
		d.holderGoneLocked(cs)
	}
}

func (d *Daemon) status() wire.DaemonStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, s := range d.sessions {
		if !s.ended {
			n++
		}
	}
	return wire.DaemonStatus{
		PID:       os.Getpid(),
		Version:   version.Version,
		StartedAt: d.started.UnixMilli(),
		Sessions:  n,
		Bridges:   len(d.watchers),
	}
}

// ---------------------------------------------------------------------------
// Outbound queue (watch pushes, transcript notifications)

// outbox serializes pushes to one connection through a bounded queue. When
// the queue overflows the connection is closed: a slow bridge must never
// block the daemon, and it recovers everything with `watch {since}`.
type outbox struct {
	c    *rpc.Conn
	q    chan *wire.Msg
	done chan struct{}
	once sync.Once
}

func newOutbox(c *rpc.Conn) *outbox {
	o := &outbox{c: c, q: make(chan *wire.Msg, outboxSize), done: make(chan struct{})}
	go o.run()
	return o
}

func (o *outbox) run() {
	for {
		select {
		case m := <-o.q:
			if err := o.c.Write(m); err != nil {
				o.c.Close()
				return
			}
		case <-o.done:
			return
		}
	}
}

func (o *outbox) push(m *wire.Msg) {
	select {
	case o.q <- m:
	default:
		o.stop()
		o.c.Close()
	}
}

func (o *outbox) stop() { o.once.Do(func() { close(o.done) }) }

// outLocked returns cs's outbox, creating it on first use.
func (cs *connState) outLocked() *outbox {
	if cs.out == nil {
		cs.out = newOutbox(cs.c)
	}
	return cs.out
}

func notification(method string, params any) *wire.Msg {
	b, err := json.Marshal(params)
	if err != nil {
		panic(err) // params are daemon-built values
	}
	return &wire.Msg{Method: method, Params: b}
}

// broadcastLocked pushes a notification to every watching connection.
func (d *Daemon) broadcastLocked(method string, params any) {
	if len(d.watchers) == 0 {
		return
	}
	m := notification(method, params)
	for cs := range d.watchers {
		cs.out.push(m)
	}
}

// emitLocked appends an event to the log and pushes it to watchers.
func (d *Daemon) emitLocked(sessionID, kind string, data any) wire.Event {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			panic(err)
		}
		raw = b
	}
	ev, err := d.log.Append(wire.Event{SessionID: sessionID, Kind: kind, Data: raw})
	if err != nil {
		d.logf("daemon: event log: %v", err)
	}
	d.broadcastLocked(wire.NotifyEvent, ev)
	return ev
}

// notifyLocked emits a notification event and queues it for push channels
// (folded into digests). push, when non-nil, replaces the pushed copy.
func (d *Daemon) notifyLocked(sessionID string, n wire.NotificationData, push *wire.NotificationData) {
	d.emitLocked(sessionID, wire.EventNotification, n)
	if !notify.Enabled(d.eff.Notify) {
		return
	}
	if push == nil {
		push = &n
	}
	d.digest.Add(*push, time.Duration(d.eff.Notify.DigestWindowMs)*time.Millisecond)
}

// ---------------------------------------------------------------------------
// watch

func (d *Daemon) watch(cs *connState, id *int64, p wire.WatchParams) {
	d.mu.Lock()
	defer d.mu.Unlock()
	missed, truncated := d.log.Since(p.Since)
	if missed == nil {
		missed = []wire.Event{}
	}
	res := wire.WatchResult{
		Sessions:  d.sessionListLocked(),
		Approvals: d.approvalListLocked(),
		Seq:       d.log.Seq(),
		Missed:    missed,
		Truncated: truncated,
	}
	b, err := json.Marshal(res)
	if err != nil {
		cs.c.ReplyError(id, wire.Errorf(wire.ErrInternal, "%v", err))
		return
	}
	// The reply goes through the outbox so it precedes every push made
	// after this snapshot.
	cs.outLocked().push(&wire.Msg{ID: id, Result: b})
	d.watchers[cs] = struct{}{}
}

// ---------------------------------------------------------------------------
// Configuration

func loadConfig(path string) (wire.Config, error) {
	var c wire.Config
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return wire.Config{}, err
	}
	return c, nil
}

func (d *Daemon) configSet(c wire.Config) (wire.ConfigResult, error) {
	if c.Notify.Webhook.Enabled && c.Notify.Webhook.URL != "" &&
		!strings.HasPrefix(c.Notify.Webhook.URL, "https://") && !strings.HasPrefix(c.Notify.Webhook.URL, "http://") {
		return wire.ConfigResult{}, wire.Errorf(wire.ErrBadRequest, "webhook url must be http(s)")
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return wire.ConfigResult{}, err
	}
	if err := writeFileAtomic(d.layout.Config, b); err != nil {
		return wire.ConfigResult{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cfg, d.eff = c, c.WithDefaults()
	r := d.eff.Retention
	if err := d.log.SetRetention(r.EventsMax, time.Duration(r.EventsDays)*24*time.Hour); err != nil {
		d.logf("daemon: event log retention: %v", err)
	}
	return wire.ConfigResult{Config: d.eff}, nil
}

// writeFileAtomic writes b to path via a temporary file and rename, 0600.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// notifyTest emits a test notification and pushes it right away (no
// digest), answering with the push error if a channel failed.
func (d *Daemon) notifyTest(c *rpc.Conn, id *int64) {
	n := wire.NotificationData{Title: "SSH Term", Body: "Test notification from stagent", Level: wire.LevelInfo, Reason: reasonTest}
	d.mu.Lock()
	d.emitLocked("", wire.EventNotification, n)
	cfg := d.eff.Notify
	d.mu.Unlock()
	if !notify.Enabled(cfg) {
		c.ReplyResult(id, struct{}{}, nil)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notify.Timeout)
		defer cancel()
		if err := d.sender.Push(ctx, cfg, n); err != nil {
			c.ReplyResult(id, nil, wire.Errorf(wire.ErrUnavailable, "%v", err))
			return
		}
		c.ReplyResult(id, struct{}{}, nil)
	}()
}
