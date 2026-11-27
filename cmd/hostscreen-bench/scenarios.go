package main

import (
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// env is what a scenario drives during its window.
type env struct {
	t   Transport
	rec *Recorder
	k   kind
}

// send sends an input the way the app does (no wait for its answer).
func (e *env) send(in wire.HostScreenInput) {
	e.rec.Sent(time.Now())
	e.t.Input(in)
}

// timed sends an input and times it to the first frame that looks
// different.
func (e *env) timed(in wire.HostScreenInput) {
	e.rec.Input(time.Now())
	e.t.Input(in)
}

// scenario is one thing a person does with the host screen tab.
type scenario struct {
	name string
	path string // the page shown
	// setup runs before the window (after the first frame).
	setup func(e *env)
	// run drives the target until the window ends.
	run func(e *env, until time.Time)
}

// Scroll gestures as host_screen_cursor.dart sends them while two fingers
// move: one scroll every 80 ms (scrollEvery) with the distance gathered
// since, in frame fractions. The pause lets the screen settle so that the
// next burst's first scroll can be timed: an Android emulator runs each
// scroll as a 150 ms swipe, one after another, then flings; an iOS
// simulator lifts each 160 ms swipe without a pause and the page glides on
// for seconds.
const (
	scrollEvery  = 80 * time.Millisecond
	scrollPerMsg = -0.06 // fingers move up 6% of the frame: the page goes down
	scrollBurst  = 13    // ≈ 1 s of moving fingers
	scrollPause  = 5 * time.Second
	// typeEvery leaves room for what a letter brings along (Android's
	// clipboard overlay after the paste) before the next is timed.
	typeEvery    = 2 * time.Second
	typedLetters = "hostscreenbandwidth"
)

var scenarios = []scenario{
	{
		name: "still",
		path: "/still",
		run:  func(e *env, until time.Time) {},
	},
	{
		// Bursts of about 1 s of scrolling, then a pause in which the
		// screen settles; the first scroll of each burst is timed.
		name: "scroll",
		path: "/scroll",
		run: func(e *env, until time.Time) {
			for time.Now().Before(until) {
				for i := 0; i < scrollBurst && time.Now().Before(until); i++ {
					in := wire.HostScreenInput{Type: wire.InputScroll, X: 0.5, Y: 0.6, DY: scrollPerMsg}
					if i == 0 {
						e.timed(in)
					} else {
						e.send(in)
					}
					time.Sleep(scrollEvery)
				}
				time.Sleep(scrollPause)
			}
		},
	},
	{
		// One letter every typeEvery into a focused textarea (32 px text, caret
		// hidden so that its blinking is neither traffic nor a change).
		name: "text",
		path: "/text",
		setup: func(e *env) {
			x, y := e.k.textAt()
			e.send(wire.HostScreenInput{Type: wire.InputTap, X: x, Y: y})
		},
		run: func(e *env, until time.Time) {
			for i := 0; time.Now().Before(until); i++ {
				e.timed(wire.HostScreenInput{Type: wire.InputText, Text: string(typedLetters[i%len(typedLetters)])})
				time.Sleep(typeEvery)
			}
		},
	},
	{
		// A looping muted 1280×720 30 fps clip (videoArgs) at page width.
		name: "video",
		path: "/video",
		run:  func(e *env, until time.Time) {},
	},
}
