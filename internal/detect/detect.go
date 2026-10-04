// Package detect derives a session state from its terminal output alone:
// desktop-notification escapes (OSC 9, OSC 777;notify, OSC 99) and BEL mean
// the program wants the user, output means it is working, and silence for
// IdleAfter means it is idle. It reports transitions only and runs no timer
// while nothing can change.
package detect

import (
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Notification is one notification the program emitted.
type Notification struct {
	Title string
	Body  string
	Bell  bool // a bare BEL rather than an OSC notification
}

// Timer is the subset of *time.Timer the detector uses.
type Timer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

// Clock abstracts time for tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// bellInterval rate-limits bell notifications (shells ring on every failed
// completion); the state still changes on each bell.
const bellInterval = time.Second

// Config configures a Detector. Callbacks run synchronously, one at a time,
// with the detector locked: they must not call back into the detector.
type Config struct {
	IdleAfter time.Duration // default 3s
	// OnState receives each state change (wire.State*) and its source.
	OnState func(state, source string)
	// OnNotify receives every notification the program emitted.
	OnNotify func(Notification)
	Clock    Clock // default: real time
}

// Detector is the output-derived state machine of one session. States:
// working, idle, waiting_input, exited.
//
//   - Any output: idle → working. waiting_input is sticky until input is
//     written to the program (Input): a program that asked for the user
//     usually redraws its prompt afterwards, which is not work.
//   - No output for IdleAfter while working → idle (one timer, re-armed
//     lazily: it fires at most once per IdleAfter while output flows).
//   - OSC 9 / 777;notify / 99, or BEL outside a string sequence →
//     notification + waiting_input.
//   - Exited → exited, final.
type Detector struct {
	mu         sync.Mutex
	cfg        Config
	idleAfter  time.Duration
	state      string
	source     string
	awaitInput bool
	lastOutput time.Time
	timer      Timer
	armed      bool
	lastBell   time.Time
	p          parser
}

// New returns a detector in state working (the program just started) with
// the idle timer armed.
func New(cfg Config) *Detector {
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.IdleAfter <= 0 {
		cfg.IdleAfter = 3 * time.Second
	}
	d := &Detector{cfg: cfg, idleAfter: cfg.IdleAfter, state: wire.StateWorking, source: wire.SourceActivity}
	d.mu.Lock()
	d.lastOutput = cfg.Clock.Now()
	d.arm()
	d.mu.Unlock()
	return d
}

// State returns the current state and its source.
func (d *Detector) State() (state, source string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state, d.source
}

// SetIdleAfter changes the quiet period (the daemon's idle_after_ms).
func (d *Detector) SetIdleAfter(v time.Duration) {
	if v <= 0 {
		return
	}
	d.mu.Lock()
	d.idleAfter = v
	d.mu.Unlock()
}

// Feed processes a chunk of program output. Escape sequences may be split
// across chunks. A chunk of DEC private mode sets and resets only (`CSI ?
// Pm h/l`) changes nothing on screen and is not work: Oh My Pi on Windows
// re-enables bracketed paste every second while it waits.
func (d *Detector) Feed(b []byte) {
	if len(b) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == wire.StateExited {
		return
	}
	d.p.feed(b, d.notified)
	if onlyModeChanges(b) {
		return
	}
	d.lastOutput = d.cfg.Clock.Now()
	switch {
	case d.state == wire.StateIdle,
		d.state == wire.StateWaitingInput && !d.awaitInput:
		d.set(wire.StateWorking, wire.SourceActivity)
	}
	if d.state == wire.StateWorking {
		d.arm()
	}
}

// onlyModeChanges reports whether b consists of whole `CSI ? Pm h` / `CSI
// ? Pm l` sequences and nothing else.
func onlyModeChanges(b []byte) bool {
	for len(b) > 0 {
		if len(b) < 4 || b[0] != 0x1b || b[1] != '[' || b[2] != '?' {
			return false
		}
		i := 3
		for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ';') {
			i++
		}
		if i == 3 || i >= len(b) || (b[i] != 'h' && b[i] != 'l') {
			return false
		}
		b = b[i+1:]
	}
	return true
}

// Input records that input was written to the program, which ends a sticky
// waiting_input: the next output counts as work again.
func (d *Detector) Input() {
	d.mu.Lock()
	d.awaitInput = false
	d.mu.Unlock()
}

// Exited moves to the final state exited.
func (d *Detector) Exited() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disarm()
	d.set(wire.StateExited, wire.SourceProcess)
}

// Stop releases the timer without a transition.
func (d *Detector) Stop() {
	d.mu.Lock()
	d.disarm()
	d.mu.Unlock()
}

func (d *Detector) notified(n Notification) {
	d.awaitInput = true
	d.set(wire.StateWaitingInput, wire.SourceTerminal)
	if n.Bell {
		now := d.cfg.Clock.Now()
		if !d.lastBell.IsZero() && now.Sub(d.lastBell) < bellInterval {
			return
		}
		d.lastBell = now
	}
	if d.cfg.OnNotify != nil {
		d.cfg.OnNotify(n)
	}
}

func (d *Detector) set(state, source string) {
	if d.state == state {
		return
	}
	d.state, d.source = state, source
	if state != wire.StateWorking {
		d.disarm()
	}
	if d.cfg.OnState != nil {
		d.cfg.OnState(state, source)
	}
}

func (d *Detector) arm() {
	if d.armed {
		return
	}
	d.armed = true
	if d.timer == nil {
		d.timer = d.cfg.Clock.AfterFunc(d.idleAfter, d.fire)
		return
	}
	d.timer.Reset(d.idleAfter)
}

func (d *Detector) disarm() {
	if d.armed && d.timer != nil {
		d.timer.Stop()
	}
	d.armed = false
}

func (d *Detector) fire() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.armed {
		return // stopped after it had already fired
	}
	d.armed = false
	if d.state != wire.StateWorking {
		return
	}
	if quiet := d.cfg.Clock.Now().Sub(d.lastOutput); quiet < d.idleAfter {
		d.armed = true
		d.timer.Reset(d.idleAfter - quiet)
		return
	}
	d.set(wire.StateIdle, wire.SourceActivity)
}
