package notify

import (
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Digester collects the notifications arriving within one window: the
// first notification opens a window, and when it closes the collected
// notifications are flushed together (the caller folds them, see Fold).
type Digester struct {
	flush     func([]wire.NotificationData)
	afterFunc func(time.Duration, func()) func() bool // returns stop

	mu      sync.Mutex
	pending []wire.NotificationData
	stop    func() bool
}

// NewDigester calls flush for every closed window that still holds
// notifications, from a timer goroutine.
func NewDigester(flush func([]wire.NotificationData)) *Digester {
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

// Drop removes the notifications of sessionID from the current window:
// the session settled what they were about before they went out.
func (d *Digester) Drop(sessionID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pending = slices.DeleteFunc(d.pending, func(n wire.NotificationData) bool {
		return n.SessionID == sessionID
	})
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
		d.flush(ns)
	}
}

// digestItems bounds how many notifications a digest body lists.
const digestItems = 5

// Fold combines notifications into one. A single notification is returned
// unchanged; several become reason "digest" with Count set, level warn if
// any was a warning, titled in lang.
func Fold(ns []wire.NotificationData, lang string) wire.NotificationData {
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
		lines = append(lines, Text(lang, PhraseDigestMore, len(ns)-digestItems))
	}
	title := Text(lang, PhraseDigest, count)
	if uniform {
		switch reason {
		case "turn_complete":
			title = Text(lang, PhraseDigestTurnComplete, count)
		case "needs_approval":
			title = Text(lang, PhraseDigestNeedsApproval, count)
		case "waiting_input":
			title = Text(lang, PhraseDigestWaitingInput, count)
		case "exited":
			title = Text(lang, PhraseDigestExited, count)
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
