package daemon

import (
	"os"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/wire"
)

// Push notifications go to the phone only when nobody is looking: every
// notification becomes an in-app event, but a push also needs a session
// started through stagent, a reason notify.reasons selects, no Claude app
// notifying the same session (skip_when_claude_app_notifies), no app in the
// foreground watching this host (presence.set) and nobody present at the
// session. A passthrough session's pushes wait pushGrace first, and every
// push of a session is dropped, or cleared on the phone, once the session
// settles what it was about.
const (
	// presentTyping: a keystroke at a terminal on the host within this
	// long means someone is present at the session.
	presentTyping = 60 * time.Second
	// presentFocused: the same while that terminal has the focus.
	presentFocused = 10 * time.Minute
	// defaultPushGrace is how long a passthrough session's push waits: the
	// user at its terminal often answers within it.
	defaultPushGrace = 15 * time.Second
)

// notifyLocked emits a notification event about s (nil: a hook of a
// program not started through stagent) and pushes it when it qualifies
// (see above). push, when non-nil, replaces the pushed copy. Both carry
// the session id.
func (d *Daemon) notifyLocked(s *session, n wire.NotificationData, push *wire.NotificationData) {
	if s == nil {
		d.emitLocked("", wire.EventNotification, n)
		return
	}
	n.SessionID = s.s.ID
	d.emitLocked(s.s.ID, wire.EventNotification, n)
	if push == nil {
		push = &n
	}
	p := *push
	p.SessionID = s.s.ID
	cfg := d.eff.Notify
	if !notify.Enabled(cfg) || !notify.Wants(cfg, p) || cfg.SkipWhenClaudeAppNotifies && s.remoteControl {
		return
	}
	// What settles a hook-derived notification is the next hook; output
	// alone (a redraw) must not clear it.
	s.pushFromHook = p.Reason != reasonTerminal && p.Reason != reasonExited && s.s.StateSource == wire.SourceHook
	if s.s.Mode != wire.ModePassthrough {
		d.pushLocked(s, p)
		return
	}
	gen := s.pushGen
	var t *time.Timer
	t = time.AfterFunc(d.opts.PushGrace, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		s.graceTimers = slices.DeleteFunc(s.graceTimers, func(x *time.Timer) bool { return x == t })
		if d.closed || s.pushGen != gen {
			return
		}
		d.pushLocked(s, p)
	})
	s.graceTimers = append(s.graceTimers, t)
}

// pushLocked queues p of s into the digest unless the app is in the
// foreground watching this host or someone is present at the session.
func (d *Daemon) pushLocked(s *session, p wire.NotificationData) {
	cfg := d.eff.Notify
	if !notify.Enabled(cfg) || d.appForegroundLocked() || d.presentLocked(s, time.Now()) {
		return
	}
	s.pushed = true
	d.digest.Add(p, time.Duration(cfg.DigestWindowMs)*time.Millisecond)
}

// settlePushLocked: s settled what its notifications were about (its
// approval closed, it went back to working, it ended). Pushes still held
// back are dropped, and the one on the phone is cleared (ntfy) or marked
// resolved (chat destination).
func (d *Daemon) settlePushLocked(s *session) {
	s.pushGen++
	for _, t := range s.graceTimers {
		t.Stop()
	}
	s.graceTimers = nil
	d.digest.Drop(s.s.ID)
	if s.pushed {
		s.pushed = false
		d.sender.Clear(d.eff.Notify, s.s.ID)
	}
}

// appForegroundLocked reports whether an app watching this host is in the
// foreground (presence.set); it then shows the events itself.
func (d *Daemon) appForegroundLocked() bool {
	for cs := range d.watchers {
		if cs.foreground {
			return true
		}
	}
	return false
}

// presentLocked reports whether someone is present at s (在席): a
// keystroke at a terminal on the host within presentTyping, or within
// presentFocused while that terminal has the focus, or the holder's
// $CLAUDE_CLIENT_PRESENCE_FILE exists. A handed-off session without such
// input counts as absent unless that file says otherwise.
func (d *Daemon) presentLocked(s *session, now time.Time) bool {
	if last := s.s.LastLocalInputAt; last > 0 {
		quiet := now.Sub(time.UnixMilli(last))
		if quiet < presentTyping || s.s.Focused && quiet < presentFocused {
			return true
		}
	}
	if f := s.s.PresenceFile; f != "" {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}
