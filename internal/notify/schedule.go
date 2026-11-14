package notify

import (
	"net/url"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// The notifications of a schedule's runs (reason "schedule") replace one
// another per schedule, not per session: ntfy's sequence ID and the chat
// message a newer one replaces are keyed by ReplaceKey. Their links carry
// sc=<schedule id> and r=<run id> instead of s=. Those of a limit window
// replace one another per window and reset time (usage.go).

// scheduleKeyPrefix starts the ReplaceKey of a schedule's notifications;
// a session id (16 hex characters) never does.
const scheduleKeyPrefix = "schedule_"

// ReplaceKey is what n replaces and is replaced by: its schedule's for a
// notification of a schedule's run, its window's for a limit window's
// (usageReplaceKey), else its session ("": none).
func ReplaceKey(n wire.NotificationData) string {
	switch {
	case n.ScheduleID != "":
		return scheduleKeyPrefix + n.ScheduleID
	case isUsage(n):
		return usageReplaceKey(n)
	}
	return n.SessionID
}

// IsSessionKey reports whether key (a ReplaceKey, or a session of
// ChatSessions) is a session's: a schedule's or a limit window's is
// settled by no session.
func IsSessionKey(key string) bool {
	return !strings.HasPrefix(key, scheduleKeyPrefix) && !strings.HasPrefix(key, usageKeyPrefix)
}

// linkValues are the values a link to n carries besides h: s=<session>,
// or sc=<schedule> and r=<run> (when known), or an empty sc= for a digest
// folding some schedule's notification (the host's schedules), or
// u=<harness> for a limit window's (an empty u= for a digest folding one:
// the host's usage screen either way).
func linkValues(n wire.NotificationData) url.Values {
	v := url.Values{}
	switch {
	case n.ScheduleID != "":
		v.Set("sc", n.ScheduleID)
		if n.RunID != "" {
			v.Set("r", n.RunID)
		}
	case isUsage(n):
		v.Set("u", n.Harness)
	case n.Schedules:
		v.Set("sc", "")
	case n.Usage:
		v.Set("u", "")
	case n.SessionID != "":
		v.Set("s", n.SessionID)
	}
	return v
}

// ClickURLOf is the link an ntfy push of n opens (ClickURL), with sc= and
// r= for a schedule's run, u= for a limit window.
func ClickURLOf(clickBase, hostID string, n wire.NotificationData) string {
	if !sessionless(n) {
		return ClickURL(clickBase, hostID, n.SessionID)
	}
	base := ClickURL(clickBase, hostID, "")
	if base == "" {
		return ""
	}
	u, _ := url.Parse(base)
	q := u.Query()
	for k, vs := range linkValues(n) {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// PageURLOf is the link to the open page for a push of n (PageURL), with
// sc= and r= for a schedule's run, u= for a limit window.
func PageURLOf(clickPage, hostID string, n wire.NotificationData) string {
	if !sessionless(n) {
		return PageURL(clickPage, hostID, n.SessionID)
	}
	base := PageURL(clickPage, hostID, "")
	if base == "" {
		return ""
	}
	return base + "&" + linkValues(n).Encode()
}

// sessionless reports whether n links elsewhere than to its session: a
// schedule's or a limit window's notification, or a digest folding one.
func sessionless(n wire.NotificationData) bool {
	return n.ScheduleID != "" || n.Schedules || isUsage(n) || n.Usage
}
