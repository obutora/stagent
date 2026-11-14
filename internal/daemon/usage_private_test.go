package daemon

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

const (
	apiClaudeToken = "sk-ant-oat01-SECRET-claude"
	apiCodexToken  = "eyJSECRET-codex"
)

// usageAPI is a fake of both private APIs (/claude and /codex): answer
// gives each call's status, headers and body.
type usageAPI struct {
	srv *httptest.Server

	mu     sync.Mutex
	hits   map[string]int
	answer func(path string) (int, http.Header, string)
}

func startUsageAPI(t *testing.T) *usageAPI {
	t.Helper()
	a := &usageAPI{hits: map[string]int{}}
	a.answer = func(string) (int, http.Header, string) { return 200, nil, `{}` }
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.hits[r.URL.Path]++
		answer := a.answer
		a.mu.Unlock()
		status, h, body := answer(r.URL.Path)
		for k, v := range h {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *usageAPI) set(f func(path string) (int, http.Header, string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.answer = f
}

func (a *usageAPI) answerAll(status int, h http.Header, body string) {
	a.set(func(string) (int, http.Header, string) { return status, h, body })
}

func (a *usageAPI) count(path string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hits[path]
}

// waitHits waits until path was called n times in all.
func (a *usageAPI) waitHits(t *testing.T, path string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for a.count(path) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%s called %d times, want %d", path, a.count(path), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Let the daemon record the answer.
	time.Sleep(30 * time.Millisecond)
}

// stillHits checks that path stays at n calls for a while.
func (a *usageAPI) stillHits(t *testing.T, path string, n int) {
	t.Helper()
	time.Sleep(80 * time.Millisecond)
	if got := a.count(path); got != n {
		t.Fatalf("%s called %d times, want %d", path, got, n)
	}
}

// logs collects the daemon's log lines.
type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logs) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

type apiEnv struct {
	*testEnv
	api                 *usageAPI
	clk                 *clock
	logs                *logs
	files               map[string][]byte // the credentials as written
	claudeDir, codexDir string
}

// startPrivateAPI runs a daemon with cfg whose private API calls go to a
// fake, the host's Claude and Codex logins written in the test home.
func startPrivateAPI(t *testing.T, cfg wire.Config) *apiEnv {
	t.Helper()
	return startPrivateAPIGuarded(t, cfg, nil)
}

func startPrivateAPIGuarded(t *testing.T, cfg wire.Config, guard *harness.Guard) *apiEnv {
	t.Helper()
	api := startUsageAPI(t)
	clk := &clock{t: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	lg := &logs{}
	e := startDaemon(t, cfg, Options{
		Guard:          guard,
		Now:            clk.now,
		Logf:           lg.logf,
		PrivateAPITick: 5 * time.Millisecond,
		PrivateAPI: usage.PrivateAPI{
			ClaudeURL: api.srv.URL + "/claude",
			CodexURL:  api.srv.URL + "/codex",
		},
	})
	ae := &apiEnv{testEnv: e, api: api, clk: clk, logs: lg, files: map[string][]byte{},
		claudeDir: filepath.Join(e.layout.Home, ".claude"), codexDir: filepath.Join(e.layout.Home, ".codex")}
	ae.write(filepath.Join(ae.claudeDir, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"`+apiClaudeToken+`","refreshToken":"SECRET-refresh","expiresAt":1893456000000}}`)
	ae.write(filepath.Join(ae.codexDir, "auth.json"), `{"tokens":{"access_token":"`+apiCodexToken+`","refresh_token":"SECRET-r","account_id":"acct-1"}}`)
	return ae
}

func (e *apiEnv) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.files[path] = []byte(content)
}

// app connects as the app does: watching, in front when foreground.
func (e *apiEnv) app(foreground bool) *rpc.Client {
	e.t.Helper()
	c, _ := e.client()
	call(e.t, c, wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	call(e.t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: foreground}, nil)
	return c
}

// advance moves the clock on by d.
func (e *apiEnv) advance(d time.Duration) { e.clk.set(e.clk.now().Add(d)) }

// windows returns usage.get's windows of account (h, dir) by id.
func windows(t *testing.T, c *rpc.Client, h, dir string) map[string]wire.UsageLimitWindow {
	t.Helper()
	var r wire.UsageGetResult
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	out := map[string]wire.UsageLimitWindow{}
	for _, a := range r.Limits {
		if a.Harness == h && a.ConfigDir == dir {
			for _, w := range a.Windows {
				out[w.Window] = w
			}
		}
	}
	return out
}

// checkSecretsKept fails when a credentials file changed or a log line
// holds a token.
func (e *apiEnv) checkSecretsKept() {
	e.t.Helper()
	for path, want := range e.files {
		if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, want) {
			e.t.Fatalf("%s changed", path)
		}
	}
	if logs := e.logs.text(); strings.Contains(logs, "SECRET") {
		e.t.Fatalf("a log line holds a token:\n%s", logs)
	}
}

const claudeAnswer = `{"five_hour":{"utilization":%v,"resets_at":"2026-11-14T08:00:00Z"},"seven_day":{"utilization":61,"resets_at":"2026-11-18T00:00:00Z"}}`

// With usage.private_api off (the default) no API is called, whether the
// app is in front, the usage screen opens or time passes.
func TestPrivateAPIOffCallsNothing(t *testing.T) {
	e := startPrivateAPI(t, fastConfig())
	c := e.app(true)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.advance(2 * time.Hour)
	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.stillHits(t, "/claude", 0)
	e.api.stillHits(t, "/codex", 0)
}

// While on, the API is called only while the app watches in front, at
// most once per 15 minutes, and when the usage screen opens 5 minutes
// after the last call; what it answers is recorded with source
// private_api under the harness's account.
func TestPrivateAPICallsInFrontEvery15Minutes(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI = wire.UsagePrivateAPI{Claude: true, Codex: true}
	e := startPrivateAPI(t, cfg)
	e.api.set(func(path string) (int, http.Header, string) {
		if path == "/codex" {
			return 200, nil, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_at":1794650000},"secondary_window":{"used_percent":70,"limit_window_seconds":604800,"reset_at":1795000000}}}`
		}
		return 200, nil, fmt.Sprintf(claudeAnswer, 42)
	})

	// Watching but not in front: nothing.
	c := e.app(false)
	e.api.stillHits(t, "/claude", 0)

	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	e.api.waitHits(t, "/claude", 1)
	e.api.waitHits(t, "/codex", 1)
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 42 || w.Source != wire.UsageSourcePrivateAPI || w.MeasuredAt != e.clk.now().UnixMilli() || w.Error != nil {
		t.Fatalf("claude five_hour = %+v", w)
	}
	if w := windows(t, c, wire.HarnessCodex, e.codexDir)[wire.UsageWindowSevenDay]; pct(w) != 70 || w.Source != wire.UsageSourcePrivateAPI || w.ResetsAt != 1_795_000_000_000 {
		t.Fatalf("codex seven_day = %+v", w)
	}

	e.advance(15*time.Minute - time.Second)
	e.api.stillHits(t, "/claude", 1)
	e.advance(time.Second)
	e.api.waitHits(t, "/claude", 2)

	// Not in front: nothing however long.
	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: false}, nil)
	e.advance(time.Hour)
	e.api.stillHits(t, "/claude", 2)

	// The usage screen opens: called, once.
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.waitHits(t, "/claude", 3)
	e.advance(4 * time.Minute)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.stillHits(t, "/claude", 3)
	e.advance(time.Minute)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.waitHits(t, "/claude", 4)
	e.checkSecretsKept()
}

// A failed call becomes the last error of each window of the account —
// told apart: 403, 5xx, 429, 401 — and the last value stays; a 429 is not
// called again before its Retry-After, a 401 or 403 for an hour. Turning
// the API off drops the errors. No token reaches a log line, the
// credentials stay as they were.
func TestPrivateAPIErrorsKeepLastValue(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Claude = true
	e := startPrivateAPI(t, cfg)
	errOf := func(c *rpc.Client, id string) *wire.UsageLimitError {
		t.Helper()
		return windows(t, c, wire.HarnessClaude, e.claudeDir)[id].Error
	}

	// The first call is refused for its scope: the windows carry only the
	// error.
	e.api.answerAll(403, nil, `{"error":{"message":"OAuth token does not meet scope requirement"}}`)
	c := e.app(true)
	e.api.waitHits(t, "/claude", 1)
	start := e.clk.now()
	ws := windows(t, c, wire.HarnessClaude, e.claudeDir)
	if w := ws[wire.UsageWindowFiveHour]; w.UsedPercent != nil || w.Reset || w.Error == nil || w.Error.Code != wire.UsageErrForbidden || w.Error.RetryAt != start.Add(time.Hour).UnixMilli() {
		t.Fatalf("five_hour after 403 = %+v (error %+v)", w, w.Error)
	}
	if ws[wire.UsageWindowSevenDay].Error == nil {
		t.Fatal("seven_day has no error")
	}
	e.advance(59 * time.Minute)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.stillHits(t, "/claude", 1)

	// An hour later it answers: the value, no error.
	e.api.answerAll(200, nil, fmt.Sprintf(claudeAnswer, 42))
	e.advance(time.Minute)
	e.api.waitHits(t, "/claude", 2)
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 42 || w.Error != nil {
		t.Fatalf("five_hour after 200 = %+v", w)
	}

	// 5xx: the value stays, retried 30 s later.
	e.api.answerAll(503, nil, ``)
	e.advance(15 * time.Minute)
	e.api.waitHits(t, "/claude", 3)
	at := e.clk.now()
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 42 || w.Error == nil || w.Error.Code != wire.UsageErrServer || w.Error.At != at.UnixMilli() || w.Error.RetryAt != at.Add(30*time.Second).UnixMilli() {
		t.Fatalf("five_hour after 503 = %+v (error %+v)", w, w.Error)
	}

	// 429 with Retry-After: nothing before it ends.
	e.api.answerAll(429, http.Header{"Retry-After": {"600"}}, ``)
	e.advance(30 * time.Second)
	e.api.waitHits(t, "/claude", 4)
	at = e.clk.now()
	if er := errOf(c, wire.UsageWindowFiveHour); er == nil || er.Code != wire.UsageErrRateLimited || er.RetryAt != at.Add(10*time.Minute).UnixMilli() {
		t.Fatalf("error after 429 = %+v", er)
	}
	e.advance(10*time.Minute - time.Second)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.stillHits(t, "/claude", 4)

	// 401: told apart, an hour's wait, the value still there.
	e.api.answerAll(401, nil, `{"error":{"type":"authentication_error"}}`)
	e.advance(time.Second)
	e.api.waitHits(t, "/claude", 5)
	at = e.clk.now()
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 42 || w.Error == nil || w.Error.Code != wire.UsageErrUnauthorized || w.Error.RetryAt != at.Add(time.Hour).UnixMilli() {
		t.Fatalf("five_hour after 401 = %+v (error %+v)", w, w.Error)
	}

	// Broken JSON and no answer.
	e.api.answerAll(200, nil, `{"five_hour":`)
	e.advance(time.Hour)
	e.api.waitHits(t, "/claude", 6)
	if er := errOf(c, wire.UsageWindowFiveHour); er == nil || er.Code != wire.UsageErrBadResponse {
		t.Fatalf("error after broken JSON = %+v", er)
	}

	// Turned off: the errors go, the value stays.
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: []byte(`{"usage":{"private_api":{"claude":false}}}`)}, nil)
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 42 || w.Error != nil {
		t.Fatalf("five_hour after turning off = %+v", w)
	}
	e.advance(time.Hour)
	e.api.stillHits(t, "/claude", 6)
	e.checkSecretsKept()
}

// A failure the API's host cannot be reached for is a network error.
func TestPrivateAPINetworkError(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Codex = true
	e := startPrivateAPI(t, cfg)
	e.api.srv.Close()
	c := e.app(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		w := windows(t, c, wire.HarnessCodex, e.codexDir)[wire.UsageWindowFiveHour]
		if w.Error != nil {
			if w.Error.Code != wire.UsageErrNetwork || w.UsedPercent != nil {
				t.Fatalf("five_hour = %+v (error %+v)", w, w.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no network error")
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.checkSecretsKept()
}

// A coding agent's process tree can neither turn the API on (config.set
// is refused, ADR 0004) nor make it called by asking usage.get to refresh.
func TestPrivateAPINotForAgents(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Claude = true
	e := startPrivateAPIGuarded(t, cfg, guardAs("claude"))
	c, _ := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	e.api.stillHits(t, "/claude", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := c.Call(ctx, wire.MethodConfigSet, wire.ConfigSetParams{Config: []byte(`{"usage":{"private_api":{"codex":true}}}`)}, nil)
	if errCode(err) != wire.ErrAgentRefused {
		t.Fatalf("config.set from an agent: %v", err)
	}
}

// noUsageAlert fails when a usage_limit notification event comes within
// alertQuiet.
func noUsageAlert(t *testing.T, events chan *wire.Msg, what string) {
	t.Helper()
	deadline := time.After(alertQuiet)
	isAlert := notificationWith(wire.ReasonUsageLimit)
	for {
		select {
		case m := <-events:
			if isAlert(m) {
				_, n := decodeEvent[wire.NotificationData](t, m)
				t.Fatalf("%s: usage_limit notification %+v", what, n)
			}
		case <-deadline:
			return
		}
	}
}

// The private API's windows go through the limit window notifications
// (#1562) like the statusLine's: the error-only windows a first failed
// call makes (no value, no reset time) notify nothing — neither a
// threshold nor a reset — and a value it answers later notifies 80%; a
// failure after it keeps the value and notifies no reset.
func TestPrivateAPIValuesNotifyErrorsDoNot(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Claude = true
	e := startPrivateAPI(t, cfg)
	events := watching(t, e.testEnv)

	e.api.answerAll(403, nil, `{}`)
	c := e.app(true)
	e.api.waitHits(t, "/claude", 1)
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; w.UsedPercent != nil || w.Error == nil {
		t.Fatalf("five_hour after 403 = %+v", w)
	}
	noUsageAlert(t, events, "error-only windows")

	e.api.answerAll(200, nil, fmt.Sprintf(claudeAnswer, 85))
	e.advance(time.Hour)
	e.api.waitHits(t, "/claude", 2)
	n := usageAlertEvent(t, events, "80% from the private API")
	reset := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC).UnixMilli()
	if n.Level != wire.LevelWarn || n.Harness != wire.HarnessClaude || n.Window != wire.UsageWindowFiveHour || n.ResetsAt != reset {
		t.Fatalf("80%% event %+v", n)
	}
	noUsageAlert(t, events, "seven_day at 61%")

	e.api.answerAll(503, nil, ``)
	e.advance(15 * time.Minute)
	e.api.waitHits(t, "/claude", 3)
	if w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowFiveHour]; pct(w) != 85 || w.Error == nil || w.Reset {
		t.Fatalf("five_hour after 503 = %+v", w)
	}
	noUsageAlert(t, events, "a failure after a value")
	e.checkSecretsKept()
}

// Turning the API off while a call is in flight drops what that call
// brings: its failure does not come back as an error of the windows.
func TestPrivateAPITurnedOffDuringCall(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Claude = true
	e := startPrivateAPI(t, cfg)
	release := make(chan struct{})
	e.api.set(func(string) (int, http.Header, string) {
		<-release
		return 503, nil, ``
	})
	c := e.app(true)
	deadline := time.Now().Add(5 * time.Second)
	for e.api.count("/claude") < 1 {
		if time.Now().After(deadline) {
			t.Fatal("the API was not called")
		}
		time.Sleep(5 * time.Millisecond)
	}
	call(t, c, wire.MethodConfigSet, wire.ConfigSetParams{Config: []byte(`{"usage":{"private_api":{"claude":false}}}`)}, nil)
	close(release)
	time.Sleep(80 * time.Millisecond)
	if ws := windows(t, c, wire.HarnessClaude, e.claudeDir); len(ws) != 0 {
		t.Fatalf("windows after turning off during a call: %+v", ws)
	}
}

// A CLAUDE_CONFIG_DIR written with a trailing slash is the same account
// as the statusLine relay reports under: its cleaned path.
func TestPrivateAPIClaudeConfigDirCleaned(t *testing.T) {
	cfg := fastConfig()
	cfg.Usage.PrivateAPI.Claude = true
	e := startPrivateAPI(t, cfg)
	dir := filepath.Join(e.layout.Home, "claude-work")
	e.write(filepath.Join(dir, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"`+apiClaudeToken+`","expiresAt":1893456000000}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir+string(filepath.Separator))
	e.api.answerAll(200, nil, fmt.Sprintf(claudeAnswer, 42))
	c := e.app(true)
	e.api.waitHits(t, "/claude", 1)
	var r wire.UsageGetResult
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	if len(r.Limits) != 1 || r.Limits[0].ConfigDir != dir {
		t.Fatalf("accounts = %+v, want one of %s", r.Limits, dir)
	}
}
