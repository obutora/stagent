package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// clock is a settable Options.Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// relayIn records in the test home's manifest that `integrate
// --statusline` put the relay in the settings.json of each of dirs.
func relayIn(t *testing.T, dirs ...string) {
	t.Helper()
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Configs []map[string]any `json:"configs"`
	}
	if b, err := os.ReadFile(l.Manifest); err == nil {
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range dirs {
		m.Configs = append(m.Configs, map[string]any{"id": "statusline", "path": filepath.Join(d, "settings.json")})
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.Manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// report sends usage.report the way `stagent statusline claude` does (a
// notification) from configDir, where the relay is installed, then waits
// for the daemon to have handled it: a connection's messages are handled
// in order.
func report(t *testing.T, c *rpc.Client, configDir, rateLimits string) {
	t.Helper()
	relayIn(t, configDir)
	reportAs(t, c, wire.UsageReportParams{ConfigDir: configDir, SessionID: "s1", RateLimits: json.RawMessage(rateLimits)})
}

// reportAs sends usage.report with p and waits until it was handled.
func reportAs(t *testing.T, c *rpc.Client, p wire.UsageReportParams) {
	t.Helper()
	if err := c.Notify(wire.MethodUsageReport, p); err != nil {
		t.Fatal(err)
	}
	call(t, c, wire.MethodPing, nil, nil)
}

// limitsOf returns usage.get's limit windows of one account, by window id.
func limitsOf(t *testing.T, c *rpc.Client, configDir string) map[string]wire.UsageLimitWindow {
	t.Helper()
	var r wire.UsageGetResult
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	out := map[string]wire.UsageLimitWindow{}
	for _, a := range r.Limits {
		if a.Harness == wire.HarnessClaude && a.ConfigDir == configDir {
			for _, w := range a.Windows {
				out[w.Window] = w
			}
		}
	}
	return out
}

func pct(w wire.UsageLimitWindow) float64 {
	if w.UsedPercent == nil {
		return -1
	}
	return *w.UsedPercent
}

// The daemon keeps the windows statusLine runs report, per configuration
// directory: a lower share within the same window (resets within 60 s of
// each other) and a window older than the one it has are dropped, and a
// report with one window leaves the other as it was.
func TestUsageReportKeepsLimitWindows(t *testing.T) {
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	e := startDaemon(t, fastConfig(), Options{Now: clk.now})
	c, _ := e.client()
	const dir = "/home/u/.claude"
	const r5, r7 = 1_763_010_000, 1_763_500_000

	report(t, c, dir, `{"five_hour":{"used_percentage":42,"resets_at":1763010000},"seven_day":{"used_percentage":61.5,"resets_at":1763500000}}`)
	got := limitsOf(t, c, dir)
	if w := got[wire.UsageWindowFiveHour]; pct(w) != 42 || w.ResetsAt != r5*1000 || w.Source != wire.UsageSourceStatusLine || w.MeasuredAt != clk.now().UnixMilli() || w.Reset {
		t.Fatalf("five_hour = %+v", w)
	}
	if w := got[wire.UsageWindowSevenDay]; pct(w) != 61.5 || w.ResetsAt != r7*1000 {
		t.Fatalf("seven_day = %+v", w)
	}
	if other := limitsOf(t, c, "/elsewhere"); len(other) != 0 {
		t.Fatalf("another config dir got %+v", other)
	}

	// Another session still reporting an earlier, lower share of the same
	// window (its reset rounded 30 s apart), and one reporting the window
	// before this one: both dropped.
	clk.set(clk.now().Add(time.Minute))
	report(t, c, dir, `{"five_hour":{"used_percentage":40,"resets_at":1763010030}}`)
	report(t, c, dir, `{"seven_day":{"used_percentage":90,"resets_at":1762900000}}`)
	got = limitsOf(t, c, dir)
	if w := got[wire.UsageWindowFiveHour]; pct(w) != 42 || w.ResetsAt != r5*1000 {
		t.Fatalf("lower share kept: %+v", w)
	}
	if w := got[wire.UsageWindowSevenDay]; pct(w) != 61.5 || w.ResetsAt != r7*1000 {
		t.Fatalf("older window kept: %+v", w)
	}

	// Only the 5-hour window comes (utilization, resets_at as a string):
	// it is taken, the 7-day window stays.
	clk.set(clk.now().Add(time.Minute))
	report(t, c, dir, `{"five_hour":{"utilization":55,"resets_at":"2025-11-13T05:00:00Z"}}`)
	got = limitsOf(t, c, dir)
	if w := got[wire.UsageWindowFiveHour]; pct(w) != 55 || w.ResetsAt != time.Date(2025, 11, 13, 5, 0, 0, 0, time.UTC).UnixMilli() || w.MeasuredAt != clk.now().UnixMilli() {
		t.Fatalf("five_hour after utilization = %+v", w)
	}
	if w := got[wire.UsageWindowSevenDay]; pct(w) != 61.5 {
		t.Fatalf("seven_day lost: %+v", w)
	}

	// A new window (the reset moved on) may start lower.
	report(t, c, dir, `{"five_hour":{"used_percentage":3,"resets_at":1763030000}}`)
	if w := limitsOf(t, c, dir)[wire.UsageWindowFiveHour]; pct(w) != 3 || w.ResetsAt != 1_763_030_000_000 {
		t.Fatalf("next window = %+v", w)
	}
}

// usage.report is sent from coding agents' process trees, so the daemon
// takes it only from a configuration directory the relay is installed in,
// given as a clean absolute path, within bounds: anything else changes
// nothing.
func TestUsageReportRefusesWhatTheRelayCannotSend(t *testing.T) {
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	e := startDaemon(t, fastConfig(), Options{Now: clk.now})
	c, _ := e.client()
	const dir = "/home/u/.claude"
	relayIn(t, dir)
	good := `{"five_hour":{"used_percentage":42,"resets_at":1763010000}}`
	long := strings.Repeat("x", 5000)
	for _, p := range []wire.UsageReportParams{
		{ConfigDir: "/home/u/.claude-other", RateLimits: json.RawMessage(good)}, // no relay there
		{ConfigDir: "home/u/.claude", RateLimits: json.RawMessage(good)},
		{ConfigDir: "/home/u/.claude/", RateLimits: json.RawMessage(good)},
		{ConfigDir: "/home/u/../u/.claude", RateLimits: json.RawMessage(good)},
		{ConfigDir: "", RateLimits: json.RawMessage(good)},
		{ConfigDir: dir, SessionID: long, RateLimits: json.RawMessage(good)},
		{ConfigDir: dir, TranscriptPath: long, RateLimits: json.RawMessage(good)},
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"five_hour":{"used_percentage":42,"resets_at":1763010000},"pad":"` + strings.Repeat("x", 20<<10) + `"}`)},
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"five_hour":{"used_percentage":101,"resets_at":1763010000}}`)},
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"five_hour":{"used_percentage":-1,"resets_at":1763010000}}`)},
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"five_hour":{"used_percentage":"Inf","resets_at":1763010000}}`)},
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"seven_day":{"used_percentage":1,"resets_at":1763900000}}`)}, // 10 days on
		{ConfigDir: dir, RateLimits: json.RawMessage(`{"seven_day":{"used_percentage":1,"resets_at":1},"five_hour":{"used_percentage":1,"resets_at":1763010000}}`)},
	} {
		reportAs(t, c, p)
	}
	var r wire.UsageGetResult
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	if len(r.Limits) != 0 {
		t.Fatalf("refused reports recorded %+v", r.Limits)
	}
	reportAs(t, c, wire.UsageReportParams{ConfigDir: dir, RateLimits: json.RawMessage(good)})
	if w := limitsOf(t, c, dir)[wire.UsageWindowFiveHour]; pct(w) != 42 {
		t.Fatalf("a valid report after them: five_hour = %+v", w)
	}
}

// A window whose reset time has passed comes back without its share,
// marked reset; the values survive a restart of the daemon.
func TestUsageLimitsResetAndRestart(t *testing.T) {
	clk := &clock{t: time.Unix(1_763_000_000, 0)}
	e := startDaemon(t, fastConfig(), Options{Now: clk.now})
	c, _ := e.client()
	const dir = "/home/u/.claude"
	report(t, c, dir, `{"five_hour":{"used_percentage":42,"resets_at":1763010000},"seven_day":{"used_percentage":61,"resets_at":1763500000}}`)
	e.stop()

	clk.set(time.Unix(1_763_010_001, 0))
	e = startDaemonAt(t, fastConfig(), Options{Now: clk.now})
	c, _ = e.client()
	got := limitsOf(t, c, dir)
	if w := got[wire.UsageWindowFiveHour]; !w.Reset || w.UsedPercent != nil || w.ResetsAt != 1_763_010_000_000 || w.Source != wire.UsageSourceStatusLine {
		t.Fatalf("passed window = %+v", w)
	}
	if w := got[wire.UsageWindowSevenDay]; w.Reset || pct(w) != 61 || w.MeasuredAt != 1_763_000_000_000 {
		t.Fatalf("seven_day after restart = %+v", w)
	}
}

// A report that changes a window brings usage.changed; one that changes
// nothing (no rate_limits, not JSON) does not.
func TestUsageReportSendsUsageChanged(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{UsageChangedEvery: time.Millisecond})
	c, ch := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	// Let the first scan's usage.changed go by.
	time.Sleep(50 * time.Millisecond)
	for len(ch) > 0 {
		<-ch
	}
	report(t, c, "/d", `null`)
	report(t, c, "/d", `{"five_hour":"x"}`)
	select {
	case m := <-ch:
		t.Fatalf("usage.changed without a change: %s", m.Method)
	case <-time.After(50 * time.Millisecond):
	}
	resets := time.Now().Add(time.Hour).Unix()
	report(t, c, "/d", `{"five_hour":{"used_percentage":10,"resets_at":`+itoa(int(resets))+`}}`)
	await(t, ch, "usage.changed", func(m *wire.Msg) bool { return m.Method == wire.NotifyUsageChanged })
}

// codexLimits polls usage.get until the codex account of home has
// windows measured at or after since (unix ms), and returns it.
func codexLimits(t *testing.T, c *rpc.Client, home string, since int64) wire.UsageLimits {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var r wire.UsageGetResult
		call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
		for _, a := range r.Limits {
			if a.Harness == wire.HarnessCodex && a.ConfigDir == home && len(a.Windows) > 0 && a.Windows[0].MeasuredAt >= since {
				return a
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no codex limit windows measured since %d: %+v", since, r.Limits)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The background scan takes the newest rate_limits of the rollouts under
// CODEX_HOME as that account's windows, measured by codex_rollout at the
// time of the event, with the plan; a later event of the next window
// replaces it.
func TestUsageCodexRolloutLimits(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".codex")
	file := filepath.Join(home, "sessions", "2026", "11", "14", "rollout-2026-11-14T10-00-00-x.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(at, primary string) string {
		return `{"timestamp":"` + at + `","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":` + primary +
			`,"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1795100000},"plan_type":"plus"}}}` + "\n"
	}
	if err := os.WriteFile(file, []byte(line("2026-11-14T01:00:00.000Z", `{"used_percent":12.0,"window_minutes":300,"resets_at":1794610000}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_794_600_000, 0)}
	e := startDaemon(t, fastConfig(), Options{Now: clk.now, Roots: transcript.Roots{wire.HarnessCodex: home}, UsageScanInterval: 20 * time.Millisecond})
	c, _ := e.client()

	at := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC).UnixMilli()
	a := codexLimits(t, c, home, at)
	if a.Plan != "plus" || len(a.Windows) != 2 {
		t.Fatalf("codex account = %+v", a)
	}
	if w := a.Windows[0]; w.Window != wire.UsageWindowFiveHour || pct(w) != 12 || w.ResetsAt != 1_794_610_000_000 ||
		w.Source != wire.UsageSourceCodexRollout || w.MeasuredAt != at {
		t.Fatalf("five_hour = %+v", w)
	}
	if w := a.Windows[1]; w.Window != wire.UsageWindowSevenDay || pct(w) != 3 || w.Source != wire.UsageSourceCodexRollout {
		t.Fatalf("seven_day = %+v", w)
	}

	// The next window, written later, is taken.
	appendFile(t, file, line("2026-11-14T01:02:00.000Z", `{"used_percent":1.0,"window_minutes":300,"resets_at":1794630000}`))
	a = codexLimits(t, c, home, at+2*60_000)
	if w := a.Windows[0]; pct(w) != 1 || w.ResetsAt != 1_794_630_000_000 {
		t.Fatalf("next five_hour window = %+v", w)
	}
}
