package detect

import (
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// fakeClock fires timers only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	c      *fakeClock
	at     time.Time
	f      func()
	active bool
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.at, t.active = t.c.now.Add(d), true
	return was
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, at: c.now.Add(d), f: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves time forward, firing due timers (including ones re-armed
// by a firing timer) in deadline order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, t := range c.timers {
			if t.active && !t.at.After(target) && (next == nil || t.at.Before(next.at)) {
				next = t
			}
		}
		if next == nil {
			c.now = target
			c.mu.Unlock()
			return
		}
		c.now = next.at
		next.active = false
		c.mu.Unlock()
		next.f()
	}
}

func (c *fakeClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

type recorder struct {
	states []string
	notes  []Notification
}

func newDetector(t *testing.T) (*Detector, *fakeClock, *recorder) {
	t.Helper()
	clk := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	r := &recorder{}
	d := New(Config{
		IdleAfter: 3 * time.Second,
		Clock:     clk,
		OnState:   func(state, source string) { r.states = append(r.states, state+"/"+source) },
		OnNotify:  func(n Notification) { r.notes = append(r.notes, n) },
	})
	t.Cleanup(d.Stop)
	return d, clk, r
}

func TestQuietPeriodMakesIdleAndOutputResumesWork(t *testing.T) {
	d, clk, r := newDetector(t)

	clk.Advance(2 * time.Second)
	d.Feed([]byte("spinner"))
	clk.Advance(2 * time.Second) // only 2s quiet
	if len(r.states) != 0 {
		t.Fatalf("transitions before the quiet period elapsed: %v", r.states)
	}
	clk.Advance(time.Second)
	if want := []string{"idle/activity"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
	if n := clk.armed(); n != 0 {
		t.Fatalf("%d timers armed while idle, want none", n)
	}

	d.Feed([]byte("more"))
	clk.Advance(3 * time.Second)
	want := []string{"idle/activity", "working/activity", "idle/activity"}
	if !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
}

func TestSilentProgramBecomesIdle(t *testing.T) {
	_, clk, r := newDetector(t)
	clk.Advance(3 * time.Second)
	if want := []string{"idle/activity"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
}

// Mode sets and resets alone draw nothing: a program that re-sends them
// while it waits still becomes idle, and stays so.
func TestModeChangesAloneAreNotWork(t *testing.T) {
	d, clk, r := newDetector(t)
	for range 4 {
		clk.Advance(time.Second)
		d.Feed([]byte("\x1b[?2004h"))
	}
	d.Feed([]byte("\x1b[?1004h\x1b[?25;2004l"))
	if want := []string{"idle/activity"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
	d.Feed([]byte("\x1b[?2004hx"))
	if want := []string{"idle/activity", "working/activity"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states after drawn output = %v, want %v", r.states, want)
	}
}

func TestOSC9SplitAcrossChunksIsStickyUntilInput(t *testing.T) {
	d, clk, r := newDetector(t)
	for _, chunk := range []string{"work\x1b", "]9;Build", " done", "\x07prompt redraw"} {
		d.Feed([]byte(chunk))
	}
	if want := []Notification{{Body: "Build done"}}; !reflect.DeepEqual(r.notes, want) {
		t.Fatalf("notes = %+v, want %+v", r.notes, want)
	}
	if want := []string{"waiting_input/terminal"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
	// Redraws and silence do not end waiting_input.
	d.Feed([]byte("cursor blink"))
	clk.Advance(10 * time.Second)
	if st, _ := d.State(); st != wire.StateWaitingInput {
		t.Fatalf("state = %s after redraw/silence, want waiting_input", st)
	}
	// Input then output: working again.
	d.Input()
	d.Feed([]byte("echo"))
	if st, src := d.State(); st != wire.StateWorking || src != wire.SourceActivity {
		t.Fatalf("state = %s/%s after input+output, want working/activity", st, src)
	}
}

func TestNotificationFormats(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   []Notification
	}{
		{"osc777 with ST split at ESC", []string{"\x1b]777;notify;Codex;Turn complete\x1b", "\\"},
			[]Notification{{Title: "Codex", Body: "Turn complete"}}},
		{"osc99 chunked title and body", []string{"\x1b]99;i=7:d=0;Hel", "lo\x1b\\x\x1b]99;i=7:p=body;World\x1b\\"},
			[]Notification{{Title: "Hello", Body: "World"}}},
		{"osc99 base64", []string{"\x1b]99;e=1;SGk=\x07"},
			[]Notification{{Title: "Hi"}}},
		{"conemu progress is not a notification", []string{"\x1b]9;4;1;50\x07"}, nil},
		{"title OSC terminated by BEL is not a bell", []string{"\x1b]0;my title\x07", "\x1b]2;t\x07"}, nil},
		{"BEL inside DCS is not a bell", []string{"\x1bPq\x07data\x1b\\"}, nil},
		{"C1 bytes in UTF-8 text are not OSC", []string{"\xe2\x9d\x9d 9;x\x07"}, []Notification{{Bell: true}}},
		{"control characters are stripped", []string{"\x1b]9;a\x01b\x1b\\"}, []Notification{{Body: "ab"}}},
		{"oversized OSC is dropped", []string{"\x1b]9;", string(make([]byte, maxOSC)), "\x07"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _, r := newDetector(t)
			for _, c := range tc.chunks {
				d.Feed([]byte(c))
			}
			if !reflect.DeepEqual(r.notes, tc.want) {
				t.Fatalf("notes = %+v, want %+v", r.notes, tc.want)
			}
		})
	}
}

func TestBellNotificationsAreRateLimited(t *testing.T) {
	d, clk, r := newDetector(t)
	d.Feed([]byte("\x07"))
	d.Feed([]byte("\x07\x07"))
	if len(r.notes) != 1 {
		t.Fatalf("%d bell notifications within a second, want 1", len(r.notes))
	}
	clk.Advance(time.Second)
	d.Feed([]byte("\x07"))
	if len(r.notes) != 2 || !r.notes[1].Bell {
		t.Fatalf("notes = %+v, want a second bell after 1s", r.notes)
	}
	if want := []string{"waiting_input/terminal"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
}

func TestExitedIsFinal(t *testing.T) {
	d, clk, r := newDetector(t)
	d.Exited()
	d.Feed([]byte("late output\x07"))
	clk.Advance(time.Minute)
	if want := []string{"exited/process"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
	if len(r.notes) != 0 || clk.armed() != 0 {
		t.Fatalf("notes %v / armed timers %d after exit", r.notes, clk.armed())
	}
}

func TestIdleAfterFromDaemonApplies(t *testing.T) {
	d, clk, r := newDetector(t)
	d.SetIdleAfter(10 * time.Second)
	d.Feed([]byte("x"))
	clk.Advance(5 * time.Second)
	if len(r.states) != 0 {
		t.Fatalf("idle after 5s with idle_after 10s: %v", r.states)
	}
	clk.Advance(5 * time.Second)
	if want := []string{"idle/activity"}; !reflect.DeepEqual(r.states, want) {
		t.Fatalf("states = %v, want %v", r.states, want)
	}
}
