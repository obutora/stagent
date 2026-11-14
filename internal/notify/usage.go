package notify

import (
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// The texts of a limit window's notifications (reason usage_limit): the
// title names the agent and the window ("Claude 5h"; a push puts the host
// before it), the body is a fixed phrase with the reset time.

// UsageLimitTitle is the title of a notification about window (a
// wire.UsageWindow* id or "<minutes>m") of harness in lang.
func UsageLimitTitle(lang, harness, window string) string {
	return usageOwner(harness) + " " + windowLabel(lang, window)
}

// UsageLimitBody is the body of a notification in lang: the window passed
// threshold (80 or 100) and resets at resetsAt, or (threshold 0) it reset.
// The reset time is the host's wall clock: HH:mm within a day of now, else
// with the month and day.
func UsageLimitBody(lang string, threshold int, resetsAt, now time.Time) string {
	at := resetsAt.Local()
	clock := at.Format("15:04")
	if at.Sub(now) >= 24*time.Hour {
		clock = at.Format("1/2 15:04")
	}
	if threshold == 0 {
		return Text(lang, PhraseLimitReset)
	}
	return Text(lang, PhraseLimitUsed, threshold, clock)
}

// usageOwner is the name of the account's owner in a title: the service
// (Claude, not Claude Code).
func usageOwner(harness string) string {
	switch harness {
	case wire.HarnessClaude:
		return "Claude"
	case wire.HarnessCodex:
		return "Codex"
	}
	return harness
}

// windowLabel names a window in lang: 5h, Week, the week and the model for
// the weekly window of one model ("Week (Opus)"), or its length.
func windowLabel(lang, window string) string {
	switch window {
	case wire.UsageWindowFiveHour:
		return Text(lang, PhraseWindowFiveHour)
	case wire.UsageWindowSevenDay:
		return Text(lang, PhraseWindowSevenDay)
	}
	if model, ok := wire.UsageWindowModel(window); ok {
		return Text(lang, PhraseWindowSevenDayModel, strings.ToUpper(model[:1])+model[1:])
	}
	m, ok := wire.UsageWindowLength(window)
	switch {
	case !ok:
		return window
	case m >= 2*24*60 && m%(24*60) == 0:
		return Text(lang, PhraseWindowDays, m/(24*60))
	case m%60 == 0:
		return Text(lang, PhraseWindowHours, m/60)
	}
	return Text(lang, PhraseWindowMinutes, m)
}

// A limit window's notifications replace one another per window and reset
// time (80%, 100% and the reset of one window); their links carry
// u=<harness>, the host's usage screen (linkValues).

// usageKeyPrefix starts the ReplaceKey of a limit window's notifications.
const usageKeyPrefix = "usage_"

// isUsage reports whether n is about a limit window.
func isUsage(n wire.NotificationData) bool {
	return n.Reason == wire.ReasonUsageLimit && n.Window != ""
}

// usageReplaceKey is the ReplaceKey of a limit window's notification n:
// its harness, account (a hash of the configuration directory), window and
// reset time, a valid ntfy sequence ID as it is.
func usageReplaceKey(n wire.NotificationData) string {
	h := fnv.New32a()
	h.Write([]byte(n.ConfigDir))
	return fmt.Sprintf("%s%s_%08x_%s_%d", usageKeyPrefix, n.Harness, h.Sum32(), n.Window, n.ResetsAt/1000)
}
