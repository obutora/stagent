// Package holder implements `stagent run`: it starts a program on a PTY and
// owns everything derived from its output — the VT screen, the on-disk
// scrollback and the output-based state — while serving attach/input
// requests from the bridge on its own local IPC endpoint and reporting state
// changes (never output) to the daemon.
//
// Passthrough mode (default) mirrors the program on the local terminal like
// script(1); detached mode has no local terminal and the app owns the size.
package holder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/detect"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/pty"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/screen"
	"github.com/obutora/stagent/internal/scrollback"
	"github.com/obutora/stagent/internal/wire"
)

// Options configures one session.
type Options struct {
	Command  []string
	Detached bool
	// Handoff keeps a passthrough session running when its local terminal
	// hangs up: the session becomes detached instead of the program being
	// hung up (stagent run --handoff).
	Handoff bool
	// ID is the session id (16 lowercase hex chars); empty = random.
	ID string
	// Cols/Rows size a detached session; passthrough follows the local
	// terminal.
	Cols, Rows int
	// Dir is the program's working directory; empty = current directory.
	Dir string
	// Env is the program's environment before STAGENT_SESSION_ID (and, when
	// detached, TERM/COLORTERM defaults) are added; nil = os.Environ().
	Env    []string
	Layout *paths.Layout
	// DialDaemon connects to the daemon; nil = daemonclient.DialOrStart.
	DialDaemon func() (net.Conn, error)
	// Logf receives diagnostics; nil = stderr when detached (moved to the
	// session's log file once the program runs when stderr is a pipe or
	// socket), the holder log file in passthrough (stderr is the user's
	// terminal there).
	Logf func(format string, args ...any)
	// Guard refuses connections from coding agents' process trees (ADR
	// 0004); nil = harness.NewGuard(Layout).
	Guard *harness.Guard
}

// Exit codes of Run for failures before the program ran.
const (
	ExitUsage    = 2
	ExitNotFound = 127
	ExitFailure  = 1
)

const (
	daemonDialWait = 5 * time.Second
	attachDrain    = 2 * time.Second // wait for attached clients to get `closed`
	linkDrain      = 3 * time.Second // wait for holder.ended to reach the daemon
)

// Holder is one running session.
type Holder struct {
	o      Options
	layout *paths.Layout
	id     string
	logf   func(string, ...any)
	// guard refuses connections from coding agents (nil: none).
	guard *harness.Guard

	pty   pty.PTY
	scr   *screen.Screen
	sb    *scrollback.Store // nil when the data directory is unusable
	det   *detect.Detector
	in    *inputQueue
	link  *daemonLink
	local *localTerm // nil when detached
	// stopLocalSize ends following the local terminal's size (handoff).
	stopLocalSize context.CancelFunc
	// prompt watches the screen for claude's or Codex's permission menu
	// while the daemon asks (holder.prompt_watch).
	prompt *promptWatch

	sbWarned         bool
	lastActivityWake int64 // pump goroutine only

	// mu guards sess, atts, ended, handedOff, hungUp, focus,
	// lastLocalTypeWake and the stream positions. It is held while output
	// goes into the screen and the raw attachment queues, so a snapshot
	// taken under it lines up exactly with the byte stream that follows.
	mu        sync.Mutex
	sess      wire.Session
	atts      map[*attachment]struct{}
	ended     bool
	handedOff bool // passthrough whose local terminal hung up (Options.Handoff)
	// hungUp: a client hung the program up (session.signal hangup), as the
	// app does to end a kept shell, or (Windows) the user closed the
	// console of a passthrough session; the daemon does not take the exit
	// for an abnormal one.
	hungUp bool
	// focus holds the terminals on the host (the local terminal, `stagent
	// attach` connections) whose last focus report was focus in;
	// sess.Focused is whether there is any.
	focus             map[any]struct{}
	lastLocalTypeWake int64
	// pos counts the program output fanned out so far. The pump writes each
	// chunk to the scrollback just before fanning it out, so the scrollback
	// holds every byte before pos.
	pos int64
	// sizePos is pos at the last size change (-1: none). Raw bytes from
	// before it were laid out for another size, so a client cannot resume
	// across it.
	sizePos int64
}

// Run runs a session until its program exits and returns the program's
// exit code. Errors before the program started come with ExitUsage,
// ExitNotFound or ExitFailure.
func Run(ctx context.Context, o Options) (int, error) {
	if len(o.Command) == 0 {
		return ExitUsage, errors.New("no command given")
	}
	if o.Handoff && o.Detached {
		return ExitUsage, errors.New("--handoff applies to passthrough sessions only")
	}
	l := o.Layout
	if l == nil {
		var err error
		if l, err = paths.Resolve(); err != nil {
			return ExitFailure, err
		}
	}
	id := o.ID
	if id == "" {
		id = wire.NewSessionID()
	} else if !validID(id) {
		return ExitUsage, fmt.Errorf("invalid session id %q (want 16 lowercase hex characters)", id)
	}
	h := &Holder{o: o, layout: l, id: id, atts: map[*attachment]struct{}{}, focus: map[any]struct{}{}, sizePos: -1}
	if h.guard = o.Guard; h.guard == nil {
		h.guard = harness.NewGuard(l)
	}

	if !o.Detached {
		lt, err := openLocal()
		if err != nil {
			return ExitUsage, err
		}
		h.local = lt
	}
	dir := o.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return ExitFailure, fmt.Errorf("working directory %s is not a directory", dir)
	}
	cols, rows := o.Cols, o.Rows
	if h.local != nil {
		if c, r, ok := h.local.size(); ok {
			cols, rows = c, r
		}
	}
	cols, rows = screen.ClampSize(cols, rows)

	// Infrastructure problems degrade the session (no attach, no
	// scrollback) rather than keep the program from running.
	infraErr := l.EnsureDirs()
	h.logf = o.Logf
	if h.logf == nil {
		h.logf = defaultLogger(l, o.Detached)
	}
	if infraErr != nil {
		h.logf("stagent run: %v", infraErr)
	}
	cfg := loadConfig(l)

	env := o.Env
	if env == nil {
		env = os.Environ()
	}
	if !o.Detached {
		if n := heldSessions(l); n > 0 {
			fmt.Fprintln(os.Stderr, heldNotice(n))
		}
	}
	penv := buildEnv(env, id, o.Detached)
	// The session keeps the command as given; only the program sees
	// codex's --no-daemon and PowerShell's working-directory hook.
	p, err := pty.Start(powershellCommand(runtime.GOOS, codexCommand(o.Command, dir, penv)), dir, penv, cols, rows)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return ExitNotFound, err
		}
		return ExitFailure, err
	}
	h.pty = p
	now := time.Now().UnixMilli()
	mode := wire.ModePassthrough
	if o.Detached {
		mode = wire.ModeDetached
	}
	presenceFile, _ := lookupEnv(env, wire.EnvClaudePresenceFile)
	h.sess = wire.Session{
		ID: id, Harness: wire.DetectHarness(o.Command), Command: o.Command, Cwd: dir,
		PID: p.Pid(), HolderPID: os.Getpid(), Mode: mode,
		State: wire.StateWorking, StateSource: wire.SourceActivity,
		Cols: cols, Rows: rows, StartedAt: now, LastActivityAt: now,
		PresenceFile: presenceFile,
	}

	if infraErr == nil {
		sb, err := scrollback.Open(l.SessionDataDir(id), cfg.Retention.ScrollbackSessionMiB)
		if err != nil {
			h.logf("stagent run: scrollback disabled: %v", err)
		} else {
			h.sb = sb
		}
	}
	h.in = newInputQueue(p)
	dial, dialExisting := o.DialDaemon, o.DialDaemon
	if dial == nil {
		dial = func() (net.Conn, error) { return daemonclient.DialOrStart(l, daemonDialWait) }
		dialExisting = func() (net.Conn, error) { return daemonclient.Dial(l, daemonDialWait) }
	}
	h.link = newDaemonLink(h, dial, dialExisting)
	var respond func([]byte)
	if o.Detached {
		// No real terminal answers the program's queries (DA, CPR, ...).
		respond = h.in.tryPush
	}
	h.scr = screen.New(cols, rows, respond)
	h.prompt = newPromptWatch(h.scr.Lines, h.link.promptGone)
	h.in.menuUp = h.menuShown
	// vt.NewEmulator allocates a 4 MiB parser buffer and 10,000-line
	// scrollbacks that screen.New immediately shrinks; hand those pages back
	// now instead of carrying them in RSS for the life of the session.
	debug.FreeOSMemory()
	h.det = detect.New(detect.Config{
		IdleAfter: time.Duration(cfg.IdleAfterMs) * time.Millisecond,
		OnState:   h.onState,
		OnNotify:  h.onNotify,
	})

	var ln net.Listener
	if infraErr == nil {
		ln, err = ipc.Listen(l.HolderAddr(id))
		if err != nil {
			if o.Detached {
				// Nobody could ever reach a detached session.
				p.Signal(wire.SignalKill)
				p.Wait()
				p.Close()
				h.det.Stop()
				h.scr.Close()
				if h.sb != nil {
					h.sb.Close()
				}
				return ExitFailure, fmt.Errorf("listen %s: %w", l.HolderAddr(id), err)
			}
			h.logf("stagent run: not attachable: %v", err)
			ln = nil
		}
	}

	if o.Detached && o.Logf == nil {
		h.moveStderrToLog()
	}
	return h.run(ctx, ln)
}

func (h *Holder) run(ctx context.Context, ln net.Listener) (int, error) {
	srvCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()

	if h.local != nil {
		if err := h.local.makeRaw(); err != nil {
			h.logf("stagent run: raw mode: %v", err)
		}
		defer h.local.restore()
		go h.local.writeOutput(h.resyncLocal)
	}

	pumpDone := make(chan struct{})
	go h.pump(pumpDone)
	go h.in.run()
	if ln != nil {
		go h.serve(srvCtx, ln)
		// Tmp cleaners must not take the socket of a long-idle session.
		keep := []string{h.layout.RunDir, h.layout.HolderAddr(h.id)}
		if d := h.layout.HolderSocketDir(); d != "" {
			keep = append(keep, d)
		}
		go paths.KeepFresh(srvCtx, keep...)
	}
	go h.link.run()

	if h.local != nil {
		var lost func()
		if h.o.Handoff {
			lost = h.handoff
		}
		sizeCtx, stopSize := context.WithCancel(srvCtx)
		h.stopLocalSize = stopSize
		go h.local.copyInput(srvCtx, h.in, h.det, func(b []byte) { h.localInput(h.local, b) }, lost)
		go h.local.watchSize(sizeCtx, h.followLocalSize)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, termSignals...)
	defer signal.Stop(sigs)
	waited := make(chan struct{})
	var code int
	go func() {
		code, _ = h.pty.Wait()
		close(waited)
	}()

wait:
	for {
		select {
		case <-waited:
			break wait
		case s := <-sigs:
			h.forwardSignal(s)
		case <-ctx.Done():
			h.pty.Signal(wire.SignalKill)
			ctx = context.Background()
		}
	}

	// Let buffered output drain before closing the terminal (ConPTY only
	// reports EOF after Close; on Unix a grandchild may hold it open).
	settle := time.Second
	if runtime.GOOS == "windows" {
		settle = 250 * time.Millisecond
	}
	select {
	case <-pumpDone:
	case <-time.After(settle):
	}
	h.pty.Close()
	select {
	case <-pumpDone:
	case <-time.After(time.Second):
	}
	if h.local != nil {
		h.local.drain()
	}

	h.finish(code)
	stopServer()
	if ln != nil {
		ln.Close()
	}
	h.in.close()
	h.scr.Close()
	if h.sb != nil {
		h.sb.Close()
	}
	return code, nil
}

// finish reports the exit to attached clients and the daemon and waits
// (bounded) for both.
func (h *Holder) finish(code int) {
	h.det.Exited()
	h.prompt.stop()
	h.mu.Lock()
	h.ended = true
	h.sess.ExitCode = &code
	hungUp := h.hungUp
	atts := make([]*attachment, 0, len(h.atts))
	for a := range h.atts {
		atts = append(atts, a)
	}
	h.mu.Unlock()

	for _, a := range atts {
		a.closeWith(code)
	}
	h.link.end(code, hungUp)
	deadline := time.After(attachDrain)
	for _, a := range atts {
		select {
		case <-a.done:
		case <-deadline:
		}
	}
	select {
	case <-h.link.done:
	case <-time.After(linkDrain):
	}
}

func (h *Holder) pump(done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32<<10)
	for {
		n, err := h.pty.Read(buf)
		if n > 0 {
			h.output(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// output fans one chunk of program output out. It never waits on clients
// or the daemon; only the local terminal (passthrough) may apply
// backpressure, as with any terminal, and only while it takes output.
func (h *Holder) output(b []byte) {
	if h.local != nil && h.local.waitRoom() {
		h.logf("stagent run: session %s: local terminal stopped reading; dropping its output until it reads again", h.id)
	}
	if h.sb != nil {
		if _, err := h.sb.Write(b); err != nil && !h.sbWarned {
			h.sbWarned = true
			h.logf("stagent run: scrollback write: %v", err)
		}
	}
	h.det.Feed(b)

	now := time.Now().UnixMilli()
	h.mu.Lock()
	h.scr.Write(b)
	h.pos += int64(len(b))
	h.sess.LastActivityAt = now
	var shared []byte
	if h.local != nil {
		shared = h.local.push(b)
	}
	for a := range h.atts {
		if a.raw {
			if shared == nil {
				shared = bytes.Clone(b)
			}
			a.pushOutput(shared, h.pos)
		} else {
			a.markDirty()
		}
	}
	titleChanged := false
	if t := h.scr.Title(); t != h.sess.Title {
		h.sess.Title = t
		titleChanged = true
	}
	h.mu.Unlock()
	h.prompt.output()

	// Activity timestamps reach the daemon at most once per second.
	if titleChanged || now-h.lastActivityWake >= 1000 {
		h.lastActivityWake = now
		h.link.wake()
	}
}

// resyncLocal redraws the local terminal once it reads again after its
// output was dropped. The snapshot is taken under h.mu, exactly where the
// output queued after it continues.
func (h *Holder) resyncLocal() {
	h.mu.Lock()
	restarted := h.local.restart(h.scr.Snapshot())
	h.mu.Unlock()
	if restarted {
		h.logf("stagent run: session %s: local terminal reading again; redrawn", h.id)
	}
}

// localInput records a read b from a terminal on the host (src: the local
// terminal or a `stagent attach` connection): a keystroke sets
// last_local_input_at, a focus report the terminal's focus. The time
// reaches the daemon at most once per second; the first keystroke after a
// quiet second and every change of focus go out at once.
func (h *Holder) localInput(src any, b []byte) {
	typing := isTyping(b)
	focused, reported := focusReport(b)
	if !typing && !reported {
		return
	}
	now := time.Now().UnixMilli()
	wake := false
	h.mu.Lock()
	if typing {
		h.sess.LastLocalInputAt = now
		if now-h.lastLocalTypeWake >= 1000 {
			h.lastLocalTypeWake = now
			wake = true
		}
	}
	if reported {
		wake = h.setFocusLocked(src, focused) || wake
	}
	h.mu.Unlock()
	if wake {
		h.link.wake()
	}
}

// setFocusLocked records src's focus and reports whether sess.Focused
// changed.
func (h *Holder) setFocusLocked(src any, focused bool) bool {
	if focused {
		h.focus[src] = struct{}{}
	} else {
		delete(h.focus, src)
	}
	was := h.sess.Focused
	h.sess.Focused = len(h.focus) > 0
	return was != h.sess.Focused
}

// dropFocus forgets the focus of a terminal that went away.
func (h *Holder) dropFocus(src any) {
	h.mu.Lock()
	changed := h.setFocusLocked(src, false)
	h.mu.Unlock()
	if changed {
		h.link.wake()
	}
}

// focusReport returns the focus the last focus report in b (ESC [ I in,
// ESC [ O out) says, and whether b holds one.
func focusReport(b []byte) (focused, reported bool) {
	in := bytes.LastIndex(b, []byte("\x1b[I"))
	out := bytes.LastIndex(b, []byte("\x1b[O"))
	return in > out, in >= 0 || out >= 0
}

func (h *Holder) onState(state, source string) {
	h.mu.Lock()
	h.sess.State, h.sess.StateSource = state, source
	h.mu.Unlock()
	h.link.wake()
}

func (h *Holder) onNotify(n detect.Notification) {
	h.link.notify(wire.HolderNotifyParams{ID: h.id, Title: n.Title, Body: n.Body, Bell: n.Bell})
}

func (h *Holder) session() wire.Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sess
}

func (h *Holder) isEnded() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ended
}

// applySize resizes the PTY and screen and tells attached clients.
func (h *Holder) applySize(cols, rows int) {
	cols, rows = screen.ClampSize(cols, rows)
	h.mu.Lock()
	if h.ended || (cols == h.sess.Cols && rows == h.sess.Rows) {
		h.mu.Unlock()
		return
	}
	if err := h.pty.Resize(cols, rows); err != nil {
		h.logf("stagent run: resize: %v", err)
	}
	h.scr.Resize(cols, rows)
	h.sess.Cols, h.sess.Rows = cols, rows
	h.sizePos = h.pos
	rp := &wire.ResizeParams{ID: h.id, Cols: cols, Rows: rows}
	for a := range h.atts {
		a.pushResize(rp)
	}
	h.mu.Unlock()
	h.prompt.resized()
	h.link.wake()
}

// followLocalSize applies the local terminal's new size (passthrough: the
// local terminal owns the size, even after an app forced another one).
func (h *Holder) followLocalSize(cols, rows int) {
	if h.localOwnsSize() {
		h.applySize(cols, rows)
	}
}

// localOwnsSize reports whether a local terminal decides the session size
// (passthrough, not handed off).
func (h *Holder) localOwnsSize() bool {
	if h.local == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.handedOff
}

// handoff turns a passthrough session whose local terminal hung up into a
// detached one (Options.Handoff): output stops going to the terminal, the
// emulator answers the program's terminal queries in its place, the app
// owns the size and the daemon learns the new mode. The program is not
// signalled; it keeps running as if nothing happened.
func (h *Holder) handoff() {
	h.local.disable()
	h.dropFocus(h.local)
	h.mu.Lock()
	if h.handedOff || h.ended {
		h.mu.Unlock()
		return
	}
	h.handedOff = true
	h.sess.Mode = wire.ModeDetached
	h.mu.Unlock()
	h.stopLocalSize()
	h.scr.SetResponder(h.in.tryPush)
	h.link.wake()
	h.logf("stagent run: session %s: terminal hung up, continuing detached", h.id)
}

// signalConsoleClosed (Windows) is the holder's console going away: its
// window closed, logoff or shutdown, which Go reports as SIGTERM. Windows
// ends the holder seconds later, so there is no handoff.
const signalConsoleClosed = "console_closed"

func (h *Holder) forwardSignal(s os.Signal) {
	switch name := signalName(s); name {
	case pty.SignalHangup:
		if h.o.Handoff {
			h.handoff() // later hangups find it done and are ignored
			return
		}
		// The local terminal is gone: stop writing to it and hang up the
		// program the way a closing terminal would.
		if h.local != nil {
			h.local.disable()
		}
		h.pty.Signal(pty.SignalHangup)
	case signalConsoleClosed:
		if h.local == nil {
			h.pty.Signal(wire.SignalTerminate)
			return
		}
		// The user closed the console the session ran in: hang the program
		// up and, as after a client's hangup, its exit is no abnormal one.
		h.local.disable()
		h.mu.Lock()
		h.hungUp = true
		h.mu.Unlock()
		h.pty.Signal(pty.SignalHangup)
	case wire.SignalInterrupt:
		h.in.tryPush([]byte{0x03})
	case wire.SignalTerminate:
		h.pty.Signal(wire.SignalTerminate)
	}
}

func validID(id string) bool {
	if len(id) != 16 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// buildEnv adds the session id (replacing one inherited from an enclosing
// session) and, for detached sessions that have no terminal of their own,
// terminal type defaults matching the emulator.
func buildEnv(base []string, id string, detached bool) []string {
	env := setEnv(base, wire.EnvSessionID, id)
	if detached && runtime.GOOS != "windows" {
		if v, _ := lookupEnv(env, "TERM"); v == "" || v == "dumb" {
			env = setEnv(env, "TERM", "xterm-256color")
		}
		if _, ok := lookupEnv(env, "COLORTERM"); !ok {
			env = setEnv(env, "COLORTERM", "truecolor")
		}
	}
	return env
}

func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && envKeyEqual(k, key) {
			return v, true
		}
	}
	return "", false
}

func setEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); ok && envKeyEqual(k, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

// loadConfig reads the daemon's config.json for the values a holder needs
// before it can ask the daemon (scrollback cap, idle threshold).
func loadConfig(l *paths.Layout) wire.Config {
	var c wire.Config
	if b, err := os.ReadFile(l.Config); err == nil {
		json.Unmarshal(b, &c)
	}
	return c.WithDefaults()
}

const (
	heldDialTimeout = 200 * time.Millisecond
	heldCallTimeout = 500 * time.Millisecond
)

// heldSessions counts the agent sessions held on this host — detached with
// no client attached, not ended, harness not "other" — for the line a
// terminal session prints before its program starts. A daemon that does
// not answer quickly counts as none; it is not started for this.
func heldSessions(l *paths.Layout) int {
	conn, err := daemonclient.Dial(l, heldDialTimeout)
	if err != nil {
		return 0
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), heldCallTimeout)
	defer cancel()
	var res wire.SessionsListResult
	if c.Call(ctx, wire.MethodSessionsList, nil, &res) != nil {
		return 0
	}
	n := 0
	for _, s := range res.Sessions {
		if s.Harness != wire.HarnessOther && s.Mode == wire.ModeDetached && !s.Attached && s.State != wire.StateExited {
			n++
		}
	}
	return n
}

func heldNotice(n int) string {
	if n == 1 {
		return "stagent: 1 agent session is held on this host; `stagent ls` lists it"
	}
	return fmt.Sprintf("stagent: %d agent sessions are held on this host; `stagent ls` lists them", n)
}

// maxHolderLog bounds the shared passthrough log; it is truncated when
// larger at open.
const maxHolderLog = 1 << 20

func defaultLogger(l *paths.Layout, detached bool) func(string, ...any) {
	var w io.Writer = os.Stderr
	if !detached {
		w = io.Discard
		path := filepath.Join(l.LogDir, "holder.log")
		flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		if st, err := os.Stat(path); err == nil && st.Size() > maxHolderLog {
			flags |= os.O_TRUNC
		}
		if f, err := os.OpenFile(path, flags, 0o600); err == nil {
			w = f // closed by process exit
		}
	}
	lg := log.New(w, "", log.LstdFlags)
	return func(format string, args ...any) { lg.Printf(format, args...) }
}
