package notify

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Digester folds the notifications arriving within one window into a single
// push: the first notification opens a window, and when it closes the
// collected notifications are flushed as one (unchanged if it was alone,
// otherwise a "digest" with Count).
type Digester struct {
	flush     func(wire.NotificationData)
	afterFunc func(time.Duration, func()) func() bool // returns stop

	mu      sync.Mutex
	pending []wire.NotificationData
	stop    func() bool
}

// NewDigester calls flush for every closed window, from a timer goroutine.
func NewDigester(flush func(wire.NotificationData)) *Digester {
	return &Digester{
		flush: flush,
		afterFunc: func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		},
	}
}

// Add queues n into the current window, opening one of the given length
// if none is open.
func (d *Digester) Add(n wire.NotificationData, window time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = append(d.pending, n)
	if d.stop == nil {
		d.stop = d.afterFunc(window, d.fire)
	}
}

// Flush closes the current window now (shutdown).
func (d *Digester) Flush() {
	d.mu.Lock()
	if d.stop != nil {
		d.stop()
	}
	d.mu.Unlock()
	d.fire()
}

func (d *Digester) fire() {
	d.mu.Lock()
	ns := d.pending
	d.pending, d.stop = nil, nil
	d.mu.Unlock()
	if len(ns) > 0 {
		d.flush(Fold(ns))
	}
}

// digestItems bounds how many notifications a digest body lists.
const digestItems = 5

// Fold combines notifications into one. A single notification is returned
// unchanged; several become reason "digest" with Count set, level warn if
// any was a warning.
func Fold(ns []wire.NotificationData) wire.NotificationData {
	if len(ns) == 1 {
		return ns[0]
	}
	count := 0
	level := wire.LevelInfo
	reason := ""
	uniform := true
	var lines []string
	for i, n := range ns {
		c := max(n.Count, 1)
		count += c
		if n.Level == wire.LevelWarn {
			level = wire.LevelWarn
		}
		if i == 0 {
			reason = n.Reason
		} else if n.Reason != reason {
			uniform = false
		}
		if len(lines) < digestItems {
			line := n.Title
			if n.Body != "" {
				line += ": " + n.Body
			}
			lines = append(lines, line)
		}
	}
	if len(ns) > digestItems {
		lines = append(lines, fmt.Sprintf("…and %d more", len(ns)-digestItems))
	}
	title := fmt.Sprintf("%d agent notifications", count)
	if uniform {
		switch reason {
		case "turn_complete":
			title = fmt.Sprintf("%d agents finished their turn", count)
		case "needs_approval":
			title = fmt.Sprintf("%d approvals requested", count)
		case "waiting_input":
			title = fmt.Sprintf("%d agents waiting for input", count)
		case "exited":
			title = fmt.Sprintf("%d sessions exited", count)
		}
	}
	return wire.NotificationData{
		Title:  title,
		Body:   strings.Join(lines, "\n"),
		Level:  level,
		Reason: "digest",
		Count:  count,
	}
}
