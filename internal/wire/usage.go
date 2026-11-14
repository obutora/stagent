package wire

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Usage (使用量): the tokens the coding agents spent on this host, which the
// daemon counts from their transcripts in the background and keeps in
// <root>/state/usage-index.json (see PROTOCOL.md "Usage").

const (
	// MethodUsageGet returns the usage counted so far (UsageGetParams →
	// UsageGetResult). A reader: served to coding agents too.
	MethodUsageGet = "usage.get"

	// NotifyUsageChanged (daemon → app): new usage was counted, or the
	// first scan finished (UsageChanged). Sent at most once per 30 s;
	// the app calls usage.get for the numbers.
	NotifyUsageChanged = "usage.changed"
)

// UsageGetParams selects the days usage.get reports.
type UsageGetParams struct {
	// TZ is the IANA time zone whose calendar days the usage is summed
	// by ("Asia/Tokyo"; empty: UTC).
	TZ string `json:"tz,omitempty"`
	// From and To are the first and last calendar day (YYYY-MM-DD, in TZ)
	// to report, both included; empty leaves that end open.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// RefreshLimits: the usage screen (使用量の画面) was opened, so the
	// private API is called for the limit windows of each harness it is on
	// for (usage.private_api) unless it was called within the last
	// 5 minutes or has to wait (PROTOCOL.md "Private API"). Ignored from a
	// coding agent's process tree.
	RefreshLimits bool `json:"refresh_limits,omitempty"`
}

// UsageTokens are the counts of one row: the tokens of each kind and the
// model requests they came from.
type UsageTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	CacheRead int64 `json:"cache_read"`
	// CacheWrite5m and CacheWrite1h are the tokens written to the prompt
	// cache with a 5-minute and a 1-hour lifetime.
	CacheWrite5m int64 `json:"cache_write_5m"`
	CacheWrite1h int64 `json:"cache_write_1h"`
	Requests     int64 `json:"requests"`
	// AgentCost is the cost in US dollars the agent itself wrote for these
	// requests (omp and pi: each message's cost.total; 0 for the others,
	// which write none).
	AgentCost float64 `json:"agent_cost,omitempty"`
}

// Total is every token of t: input, output, cache reads and cache writes.
func (t UsageTokens) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite5m + t.CacheWrite1h
}

// UsageDay is the usage of one calendar day by one harness, model and
// project (the working directory the agent ran in).
type UsageDay struct {
	Date    string `json:"date"` // YYYY-MM-DD in UsageGetParams.TZ
	Harness string `json:"harness"`
	Model   string `json:"model"`
	Project string `json:"project"`
	UsageTokens
	// LongContext: the part of UsageTokens whose requests had a prompt
	// over each threshold of UsageLongContextThresholds, smallest first;
	// thresholds no request was over are left out.
	LongContext []UsageLongContext `json:"long_context,omitempty"`
}

// UsageLongContextThresholds are the prompt sizes (tokens) usage is also
// counted above, so long-context prices can be applied: the app keeps the
// price table and picks the threshold of each model (Claude Haiku 5.5:
// 100K; Claude's earlier long-context prices: 200K; OpenAI: 272K). A
// request's prompt is its input, cache reads and cache writes.
var UsageLongContextThresholds = [...]int64{100_000, 200_000, 272_000}

// UsageLongContext is the usage of the requests whose prompt was over Over
// tokens (a request over a larger threshold is in every smaller one too).
type UsageLongContext struct {
	Over int64 `json:"over"`
	UsageTokens
}

// UsageSession is the usage of one conversation within the days asked for.
type UsageSession struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	// Project is the working directory most of its tokens were spent in.
	Project string `json:"project"`
	// First and Last are the times of its first and last counted request
	// (unix ms).
	First  int64    `json:"first"`
	Last   int64    `json:"last"`
	Models []string `json:"models"`
	UsageTokens
	Total int64 `json:"total"` // UsageTokens.Total
}

// UsageGetResult is the usage counted so far.
type UsageGetResult struct {
	// Scanning: the first scan since the daemon started has not finished,
	// so the numbers can still grow.
	Scanning bool `json:"scanning"`
	// ScannedAt is when the last scan finished (unix ms; 0: never).
	ScannedAt int64 `json:"scanned_at,omitempty"`
	// Counted are the harnesses whose usage is counted; the others' are
	// not counted yet.
	Counted []string `json:"counted"`
	// Days: one row per day, harness, model and project with any usage,
	// oldest day first.
	Days []UsageDay `json:"days"`
	// Sessions: the conversations that spent the most tokens in the days
	// asked for, at most UsageTopSessions, most first.
	Sessions []UsageSession `json:"sessions"`
	// Limits are the limit windows measured so far, one item per account
	// (harness order of the registry, then ConfigDir).
	Limits []UsageLimits `json:"limits"`
}

// UsageTopSessions is how many sessions usage.get returns.
const UsageTopSessions = 20

// UsageChanged is the usage.changed notification.
type UsageChanged struct {
	Scanning  bool  `json:"scanning"`
	ScannedAt int64 `json:"scanned_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Limit windows (上限の枠): the periods a subscription's usage is counted
// in (Claude's and Codex's 5 hours and 7 days), each with the share of it
// used and its reset time, per account, kept in
// <root>/state/usage-limits.json and returned by usage.get (UsageLimits).

// MethodUsageReport (statusline → daemon, a notification: no reply) brings
// the limit windows `stagent statusline claude` read from Claude Code's
// statusLine input. Served to coding agents: it runs under claude (ADR
// 0004), so the daemon takes it only for a configuration directory the
// relay is installed in and within bounds (PROTOCOL.md "Limit windows").
const MethodUsageReport = "usage.report"

// ReasonUsageLimit is the NotificationData.Reason of a limit window's
// notifications (80%, 100%, reset).
const ReasonUsageLimit = "usage_limit"

// Limit window ids (UsageLimitWindow.Window). A window of another length
// is "<minutes>m" (Codex: "1440m").
const (
	UsageWindowFiveHour = "five_hour"
	UsageWindowSevenDay = "seven_day"
)

// Ways a limit window was measured (UsageLimitWindow.Source).
const (
	// UsageSourceStatusLine: Claude Code's statusLine input (rate_limits),
	// relayed by `stagent statusline claude`.
	UsageSourceStatusLine = "statusline"
	// UsageSourceCodexRollout: the rate_limits of Codex's token_count
	// events, read from its rollouts by the usage scan.
	UsageSourceCodexRollout = "codex_rollout"
	// UsageSourcePrivateAPI: the harness's undocumented usage API
	// (Claude /api/oauth/usage, Codex wham/usage), called with the login
	// token the harness keeps when usage.private_api is on for it.
	UsageSourcePrivateAPI = "private_api"
)

// More limit window ids the private API brings: Claude's weekly window of
// one model ("seven_day_opus", "seven_day_sonnet": UsageWindowSevenDay +
// "_" + the model, see UsageWindowModel) and its extra usage (超過の課金, a
// monthly spend limit whose reset time is not told: ResetsAt 0).
const (
	UsageWindowExtraUsage = "extra_usage"
)

// usageWindowLengths are the windows named by their length (minutes), in
// the order usage.get lists them.
var usageWindowLengths = [...]struct {
	id      string
	minutes int64
}{{UsageWindowFiveHour, 5 * 60}, {UsageWindowSevenDay, 7 * 24 * 60}}

// UsageWindowOfLength is the id of a window lasting minutes: five_hour for
// 300 (±1), seven_day for 10080 (±1), any other length "<minutes>m".
func UsageWindowOfLength(minutes float64) string {
	m := int64(math.Round(minutes))
	for _, w := range usageWindowLengths {
		if m >= w.minutes-1 && m <= w.minutes+1 {
			return w.id
		}
	}
	return strconv.FormatInt(m, 10) + "m"
}

// UsageWindowLength is how long window id lasts, in minutes: 300 for
// five_hour, 10080 for seven_day, N for "<N>m"; ok is false for any other
// id.
func UsageWindowLength(id string) (minutes int64, ok bool) {
	for _, w := range usageWindowLengths {
		if w.id == id {
			return w.minutes, true
		}
	}
	digits, found := strings.CutSuffix(id, "m")
	m, err := strconv.ParseInt(digits, 10, 64)
	if !found || err != nil || m <= 0 {
		return 0, false
	}
	return m, true
}

// UsageWindowModel is the model of Claude's weekly window of one model
// ("opus" of "seven_day_opus": lowercase letters and digits); ok is false
// for any other id.
func UsageWindowModel(id string) (model string, ok bool) {
	model, found := strings.CutPrefix(id, UsageWindowSevenDay+"_")
	if !found || model == "" {
		return "", false
	}
	for _, c := range model {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	return model, true
}

// UsageWindowRank orders window ids the way usage.get lists them:
// five_hour, seven_day, the weekly windows of one model, the others (by
// id), then extra_usage.
func UsageWindowRank(id string) int {
	for i, w := range usageWindowLengths {
		if w.id == id {
			return i
		}
	}
	switch _, model := UsageWindowModel(id); {
	case model:
		return len(usageWindowLengths)
	case id == UsageWindowExtraUsage:
		return len(usageWindowLengths) + 2
	}
	return len(usageWindowLengths) + 1
}

// Why the private API could not measure (UsageLimitError.Code).
const (
	// UsageErrRateLimited: the API answered 429; it is not called again
	// before RetryAt (its Retry-After).
	UsageErrRateLimited = "rate_limited"
	// UsageErrUnauthorized: the API refused the token (401) or the token
	// has expired; starting the harness renews the login. Not called again
	// for an hour.
	UsageErrUnauthorized = "unauthorized"
	// UsageErrForbidden: the API answered 403 (the token lacks the scope):
	// it cannot be measured this way. Not called again for an hour.
	UsageErrForbidden = "forbidden"
	// UsageErrNoCredentials: the harness keeps no login token to call the
	// API with. Not called again for an hour.
	UsageErrNoCredentials = "no_credentials"
	// UsageErrServer: the API answered another status (5xx).
	UsageErrServer = "server"
	// UsageErrNetwork: the API could not be reached in 10 s.
	UsageErrNetwork = "network"
	// UsageErrBadResponse: the API's answer was not the JSON expected.
	UsageErrBadResponse = "bad_response"
)

// UsageLimitError is the last failed private API call for a limit window.
type UsageLimitError struct {
	Code    string `json:"code"`    // UsageErr*
	Message string `json:"message"` // English, for logs; never holds a token
	At      int64  `json:"at"`      // when it failed (unix ms)
	// RetryAt is when the API is called again at the earliest (unix ms).
	RetryAt int64 `json:"retry_at,omitempty"`
}

// UsageReportParams is usage.report: what one run of Claude Code's
// statusLine gave, as it gave it (harness claude, source statusline).
type UsageReportParams struct {
	// ConfigDir is the account the windows belong to: Claude Code's
	// configuration directory ($CLAUDE_CONFIG_DIR, else ~/.claude) as a
	// clean absolute path.
	ConfigDir      string `json:"config_dir"`
	SessionID      string `json:"session_id,omitempty"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	// RateLimits is the statusLine input's rate_limits object, verbatim.
	RateLimits json.RawMessage `json:"rate_limits"`
}

// UsageLimits are the limit windows of one account: a harness and its
// configuration directory.
type UsageLimits struct {
	Harness   string `json:"harness"`
	ConfigDir string `json:"config_dir"`
	// Plan is the account's plan when the harness tells it (Codex
	// plan_type: "plus", "pro").
	Plan    string             `json:"plan,omitempty"`
	Windows []UsageLimitWindow `json:"windows"`
}

// UsageLimitWindow is the last measurement of one limit window.
type UsageLimitWindow struct {
	Window string `json:"window"` // UsageWindow*
	// UsedPercent is the share of the window's limit used (0–100); absent
	// when Reset.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// ResetsAt is when the window starts over (unix ms).
	ResetsAt int64 `json:"resets_at"`
	// Source is how it was measured (UsageSource*).
	Source string `json:"source"`
	// MeasuredAt is when the value was measured (unix ms): when the daemon
	// received it (statusline), or the time of the event (codex_rollout).
	MeasuredAt int64 `json:"measured_at"`
	// Reset: ResetsAt has passed, so the value measured before it no longer
	// holds and is not returned.
	Reset bool `json:"reset,omitempty"`
	// Error is the last failure of the private API for this window's
	// account, while usage.private_api is on for it and no call succeeded
	// since; the value above (if any) is the last one measured. A window
	// with only an error has no UsedPercent, ResetsAt 0, MeasuredAt 0.
	Error *UsageLimitError `json:"error,omitempty"`
}
