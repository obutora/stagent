package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// The notifications of the limit windows (上限の枠, #1562), reason
// usage_limit: 80% and 100% once per window and reset time, then reset
// once after 100%.

const alertDir = "/home/u/.claude"

// alertQuiet is how long a test waits to see that nothing is pushed.
const alertQuiet = 200 * time.Millisecond

// fiveHour is a statusLine rate_limits with the 5-hour window at pct,
// resetting at reset (unix s).
func fiveHour(pct float64, reset int64) string {
	return fmt.Sprintf(`{"five_hour":{"used_percentage":%v,"resets_at":%d}}`, pct, reset)
}

// clockAt is an HH:mm of the host's zone, as the bodies write a reset.
func clockAt(unix int64) string { return time.Unix(unix, 0).Local().Format("15:04") }

// alertSeq is the ntfy sequence ID of the notifications of the 5-hour
// window of alertDir resetting at reset (unix s).
func alertSeq(reset int64) string {
	return notify.SequenceID(notify.ReplaceKey(wire.NotificationData{
		Reason: "usage_limit", Harness: wire.HarnessClaude, ConfigDir: alertDir, Window: wire.UsageWindowFiveHour, ResetsAt: reset * 1000,
	}))
}

// usageAlertEvent awaits the next usage_limit notification event.
func usageAlertEvent(t *testing.T, events chan *wire.Msg, what string) wire.NotificationData {
	t.Helper()
	_, n := decodeEvent[wire.NotificationData](t, await(t, events, what, notificationWith(wire.ReasonUsageLimit)))
	return n
}

// watching connects a client that watches (gets notification events).
func watching(t *testing.T, e *testEnv) chan *wire.Msg {
	t.Helper()
	c, events := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	return events
}

// Rising 79 → 81 → 85% pushes 80% once (warn), 100% once (error) with the
// same sequence ID; once the window resets, reset once (info); the next
// window pushes 80% again under its own sequence ID. The events carry the
// window, so the app opens the usage screen.
func TestUsageLimitNotifiesEachThresholdOnce(t *testing.T) {
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	e, reqs := pushEnv(t, nil, Options{Now: clk.now, UsageScanInterval: 20 * time.Millisecond})
	c, _ := e.client()
	events := watching(t, e)
	const r1, r2 = 1_763_010_000, 1_763_028_000

	report(t, c, alertDir, fiveHour(79, r1))
	noReq(t, reqs, "79%", alertQuiet)
	report(t, c, alertDir, fiveHour(81, r1))
	r := wantPush(t, reqs, "81%", alertSeq(r1), "80% used · resets at "+clockAt(r1))
	if r.title != "box · Claude 5h" {
		t.Errorf("title %q", r.title)
	}
	if n := usageAlertEvent(t, events, "80% event"); n.Level != wire.LevelWarn || n.Title != "Claude 5h" || n.Harness != wire.HarnessClaude ||
		n.Window != wire.UsageWindowFiveHour || n.ResetsAt != r1*1000 || n.SessionID != "" {
		t.Errorf("80%% event %+v", n)
	}
	report(t, c, alertDir, fiveHour(85, r1))
	noReq(t, reqs, "85%", alertQuiet)

	report(t, c, alertDir, fiveHour(100, r1))
	wantPush(t, reqs, "100%", alertSeq(r1), "100% used · resets at "+clockAt(r1))
	if n := usageAlertEvent(t, events, "100% event"); n.Level != wire.LevelError {
		t.Errorf("100%% event %+v", n)
	}
	report(t, c, alertDir, fiveHour(100, r1+20)) // the reset rounded apart
	noReq(t, reqs, "100% again", alertQuiet)

	clk.set(time.Unix(r1+21, 0))
	wantPush(t, reqs, "reset", alertSeq(r1), "Reset")
	if n := usageAlertEvent(t, events, "reset event"); n.Level != wire.LevelInfo {
		t.Errorf("reset event %+v", n)
	}
	noReq(t, reqs, "reset again", alertQuiet)

	report(t, c, alertDir, fiveHour(40, r2))
	noReq(t, reqs, "the next window at 40%", alertQuiet)
	report(t, c, alertDir, fiveHour(82, r2))
	wantPush(t, reqs, "the next window at 82%", alertSeq(r2), "80% used · resets at "+clockAt(r2))
}

// A restarted daemon remembers what it notified (state/): the same window
// pushes no 80% again, but still its 100%.
func TestUsageLimitNotifiedSurvivesRestart(t *testing.T) {
	reqs := make(chan ntfyReq, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		title, _ := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		seq := r.Header.Get("X-Sequence-ID")
		reqs <- ntfyReq{r.Method, r.URL.Path, seq, title, string(b), time.Now()}
		json.NewEncoder(w).Encode(map[string]string{"id": "m1", "sequence_id": seq})
	}))
	t.Cleanup(srv.Close)
	cfg := fastConfig()
	cfg.Notify.Ntfy = wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	opts := Options{Now: clk.now, UsageScanInterval: 20 * time.Millisecond}
	const r1 = 1_763_010_000

	e := startDaemon(t, cfg, opts)
	c, _ := e.client()
	report(t, c, alertDir, fiveHour(81, r1))
	wantPush(t, reqs, "81%", alertSeq(r1), "80% used · resets at "+clockAt(r1))
	c.Close()
	e.stop()

	e = startDaemonAt(t, cfg, opts)
	c, _ = e.client()
	report(t, c, alertDir, fiveHour(85, r1))
	noReq(t, reqs, "85% after the restart", alertQuiet)
	report(t, c, alertDir, fiveHour(100, r1))
	wantPush(t, reqs, "100% after the restart", alertSeq(r1), "100% used · resets at "+clockAt(r1))
}

// A value measured more than an hour ago (Codex's rollout written before)
// is not compared; one measured within the hour is.
func TestUsageLimitIgnoresOldValues(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	file := filepath.Join(home, "sessions", "2026", "11", "14", "rollout-2026-11-14T10-00-00-x.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	const reset = 1_794_630_000 // 04:20 UTC
	line := func(at string, pct float64) string {
		return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":%d}}}}`+"\n", at, pct, reset)
	}
	// Measured 01:00; the daemon starts at 02:01.
	if err := os.WriteFile(file, []byte(line("2026-11-14T01:00:00.000Z", 90)), 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 14, 2, 1, 0, 0, time.UTC)
	clk := &clock{t: at}
	e, reqs := pushEnv(t, nil, Options{Now: clk.now, Roots: transcript.Roots{wire.HarnessCodex: home}, UsageScanInterval: 20 * time.Millisecond})
	c, _ := e.client()
	codexLimits(t, c, home, at.Add(-61*time.Minute).UnixMilli())
	noReq(t, reqs, "a value of 61 minutes ago", alertQuiet)

	appendFile(t, file, line("2026-11-14T02:00:30.000Z", 91))
	r := wantPush(t, reqs, "a value of 30 s ago", notify.SequenceID(notify.ReplaceKey(wire.NotificationData{
		Reason: "usage_limit", Harness: wire.HarnessCodex, ConfigDir: home, Window: wire.UsageWindowFiveHour, ResetsAt: reset * 1000,
	})), "80% used · resets at "+clockAt(reset))
	if r.title != "box · Codex 5h" {
		t.Errorf("title %q", r.title)
	}
}

// notify.reasons.usage_limit false pushes none; the app still gets the
// event (it shows it as its own settings say).
func TestUsageLimitReasonOff(t *testing.T) {
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	e, reqs := pushEnv(t, func(c *wire.Config) { c.Notify.Reasons.UsageLimit = new(false) }, Options{Now: clk.now})
	c, _ := e.client()
	events := watching(t, e)
	report(t, c, alertDir, fiveHour(100, 1_763_010_000))
	usageAlertEvent(t, events, "100% event")
	noReq(t, reqs, "usage_limit off", alertQuiet)
}
