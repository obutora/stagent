package usage

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

const (
	claudeToken = "sk-ant-oat01-SECRET-claude-token"
	codexToken  = "eyJSECRET.codex.token"
)

var apiNow = time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)

// fakeAPI is a usage API answering status and body, recording the
// requests' headers.
type fakeAPI struct {
	*httptest.Server
	status  int
	header  http.Header
	body    string
	got     []http.Header
	paths   []string
	answers int
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{status: 200, header: http.Header{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.got = append(f.got, r.Header.Clone())
		f.paths = append(f.paths, r.URL.Path)
		for k, v := range f.header {
			w.Header()[k] = v
		}
		w.WriteHeader(f.status)
		w.Write([]byte(f.body))
	}))
	t.Cleanup(f.Close)
	return f
}

// claudeDir is a Claude configuration directory with credentials holding
// claudeToken (expiring at expiresAt, unix ms).
func claudeDir(t *testing.T, expiresAt int64) string {
	t.Helper()
	dir := t.TempDir()
	creds := `{"claudeAiOauth":{"accessToken":"` + claudeToken + `","refreshToken":"sk-ant-ort01-SECRET-refresh","expiresAt":` + itoa(expiresAt) + `,"scopes":["user:inference","user:profile"]}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func codexDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	auth := `{"OPENAI_API_KEY":null,"tokens":{"id_token":"x","access_token":"` + codexToken + `","refresh_token":"SECRET-refresh","account_id":"acct-123"},"last_refresh":"2026-11-13T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(auth), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func windowsByID(ws []Window) map[string]Window {
	out := map[string]Window{}
	for _, w := range ws {
		out[w.ID] = w
	}
	return out
}

var later = apiNow.Add(time.Hour).UnixMilli()

// Claude's /api/oauth/usage is called with the login token and the beta
// header, and its windows are read whatever names and forms it uses:
// utilization or used_percentage, a number or a string, resets_at in
// seconds, milliseconds or RFC 3339; the weekly window of a model and the
// enabled extra usage come too, a null or incomplete window does not.
func TestClaudeUsageAPIReadsWindows(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]Window
	}{
		{
			name: "utilization numbers, RFC 3339",
			body: `{"five_hour":{"utilization":42.0,"resets_at":"2026-11-14T08:00:00.123456+00:00"},"seven_day":{"utilization":61,"resets_at":"2026-11-18T00:00:00Z"},"seven_day_oauth_apps":null,"seven_day_opus":{"utilization":12,"resets_at":"2026-11-18T00:00:00Z"},"seven_day_sonnet":null,"extra_usage":{"is_enabled":true,"monthly_limit":5000,"used_credits":1234,"utilization":24.68}}`,
			want: map[string]Window{
				"five_hour":      {ID: "five_hour", UsedPercent: 42, ResetsAt: time.Date(2026, 11, 14, 8, 0, 0, 123456000, time.UTC).UnixMilli()},
				"seven_day":      {ID: "seven_day", UsedPercent: 61, ResetsAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC).UnixMilli()},
				"seven_day_opus": {ID: "seven_day_opus", UsedPercent: 12, ResetsAt: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC).UnixMilli()},
				"extra_usage":    {ID: "extra_usage", UsedPercent: 24.68},
			},
		},
		{
			name: "used_percentage strings, seconds and milliseconds",
			body: `{"five_hour":{"used_percentage":"7.5","resets_at":1763110800},"seven_day":{"used_percentage":90,"resets_at":"1763500000000"},"extra_usage":{"is_enabled":false,"utilization":3}}`,
			want: map[string]Window{
				"five_hour": {ID: "five_hour", UsedPercent: 7.5, ResetsAt: 1_763_110_800_000},
				"seven_day": {ID: "seven_day", UsedPercent: 90, ResetsAt: 1_763_500_000_000},
			},
		},
		{
			name: "null and incomplete windows",
			body: `{"five_hour":null,"seven_day":{"utilization":5},"seven_day_sonnet":{"resets_at":1763500000}}`,
			want: map[string]Window{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.body = c.body
			p := &PrivateAPI{ClaudeURL: api.URL + "/api/oauth/usage"}
			m, aerr := p.Measure(context.Background(), Account{Harness: wire.HarnessClaude, Dir: claudeDir(t, later)}, apiNow)
			if aerr != nil {
				t.Fatalf("error %+v", aerr)
			}
			if m.Source != wire.UsageSourcePrivateAPI || !m.At.Equal(apiNow) {
				t.Fatalf("measurement = %+v", m)
			}
			got := windowsByID(m.Windows)
			if len(got) != len(c.want) {
				t.Fatalf("windows = %+v, want %+v", got, c.want)
			}
			for id, w := range c.want {
				if got[id] != w {
					t.Errorf("%s = %+v, want %+v", id, got[id], w)
				}
			}
			h := api.got[0]
			if h.Get("Authorization") != "Bearer "+claudeToken || h.Get("anthropic-beta") != "oauth-2025-04-20" {
				t.Fatalf("headers = %v", h)
			}
		})
	}
}

// Codex's wham/usage is called with auth.json's token and account id; its
// windows are told apart by limit_window_seconds, reset by reset_at or
// reset_after_seconds.
func TestCodexUsageAPIReadsWindows(t *testing.T) {
	api := newFakeAPI(t)
	api.body = `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":12,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":1763110800},"secondary_window":{"used_percent":"70.5","limit_window_seconds":604800,"reset_after_seconds":7200}},"credits":{"has_credits":false}}`
	p := &PrivateAPI{CodexURL: api.URL + "/backend-api/wham/usage"}
	m, aerr := p.Measure(context.Background(), Account{Harness: wire.HarnessCodex, Dir: codexDir(t)}, apiNow)
	if aerr != nil {
		t.Fatalf("error %+v", aerr)
	}
	got := windowsByID(m.Windows)
	if w := got["five_hour"]; w.UsedPercent != 12 || w.ResetsAt != 1_763_110_800_000 {
		t.Fatalf("five_hour = %+v", w)
	}
	if w := got["seven_day"]; w.UsedPercent != 70.5 || w.ResetsAt != apiNow.Add(2*time.Hour).UnixMilli() {
		t.Fatalf("seven_day = %+v", w)
	}
	if m.Plan != "plus" || m.Source != wire.UsageSourcePrivateAPI {
		t.Fatalf("measurement = %+v", m)
	}
	h := api.got[0]
	if h.Get("Authorization") != "Bearer "+codexToken || h.Get("ChatGPT-Account-Id") != "acct-123" {
		t.Fatalf("headers = %v", h)
	}
}

// Each way a call fails gives its own error: 429 (with the end of its
// Retry-After, in seconds or as a date), 401, 403, 5xx, no answer, an
// answer that is not JSON, no token, an expired token. No error text
// holds the token.
func TestUsageAPIErrors(t *testing.T) {
	retryDate := apiNow.Add(90 * time.Second)
	cases := []struct {
		name    string
		status  int
		header  http.Header
		body    string
		down    bool
		dir     func(t *testing.T) string
		code    string
		retryAt time.Time
		calls   int
	}{
		{name: "429 seconds", status: 429, header: http.Header{"Retry-After": {"120"}}, code: wire.UsageErrRateLimited, retryAt: apiNow.Add(2 * time.Minute), calls: 1},
		{name: "429 date", status: 429, header: http.Header{"Retry-After": {retryDate.Format(http.TimeFormat)}}, code: wire.UsageErrRateLimited, retryAt: retryDate, calls: 1},
		{name: "429 without Retry-After", status: 429, code: wire.UsageErrRateLimited, calls: 1},
		{name: "401", status: 401, body: `{"error":{"type":"authentication_error"}}`, code: wire.UsageErrUnauthorized, calls: 1},
		{name: "403", status: 403, body: `{"error":{"message":"OAuth token does not meet scope requirement user:profile"}}`, code: wire.UsageErrForbidden, calls: 1},
		{name: "500", status: 500, code: wire.UsageErrServer, calls: 1},
		{name: "503", status: 503, code: wire.UsageErrServer, calls: 1},
		{name: "broken JSON", status: 200, body: `{"five_hour":`, code: wire.UsageErrBadResponse, calls: 1},
		{name: "not an object", status: 200, body: `[1,2]`, code: wire.UsageErrBadResponse, calls: 1},
		{name: "no answer", down: true, code: wire.UsageErrNetwork},
		{name: "no token", dir: func(t *testing.T) string { return t.TempDir() }, code: wire.UsageErrNoCredentials},
		{name: "expired token", dir: func(t *testing.T) string { return claudeDir(t, apiNow.Add(-time.Minute).UnixMilli()) }, code: wire.UsageErrUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.status, api.body = c.status, c.body
			if c.header != nil {
				api.header = c.header
			}
			url := api.URL
			if c.down {
				api.Close()
			}
			dir := claudeDir(t, later)
			if c.dir != nil {
				dir = c.dir(t)
			}
			p := &PrivateAPI{ClaudeURL: url}
			_, aerr := p.Measure(context.Background(), Account{Harness: wire.HarnessClaude, Dir: dir}, apiNow)
			if aerr == nil || aerr.Code != c.code {
				t.Fatalf("error = %+v, want code %s", aerr, c.code)
			}
			if !aerr.RetryAt.Equal(c.retryAt) {
				t.Fatalf("retry at %v, want %v", aerr.RetryAt, c.retryAt)
			}
			if len(api.got) != c.calls {
				t.Fatalf("%d calls, want %d", len(api.got), c.calls)
			}
			if strings.Contains(aerr.Message, "SECRET") || strings.Contains(aerr.Error(), claudeToken) {
				t.Fatalf("error text holds the token: %q", aerr.Message)
			}
		})
	}
}

// The Keychain item named for the configuration directory is read before
// the credentials file: Claude Code-credentials, and with
// CLAUDE_CONFIG_DIR the first 8 hex digits of the directory's SHA-256
// appended.
func TestClaudeTokenFromKeychain(t *testing.T) {
	api := newFakeAPI(t)
	api.body = `{}`
	var asked []string
	p := &PrivateAPI{ClaudeURL: api.URL, Keychain: func(_ context.Context, service string) ([]byte, error) {
		asked = append(asked, service)
		return []byte(`{"claudeAiOauth":{"accessToken":"keychain-token","expiresAt":` + itoa(later) + `}}` + "\n"), nil
	}}
	dir := claudeDir(t, later)
	if _, aerr := p.Measure(context.Background(), Account{Harness: wire.HarnessClaude, Dir: dir, ConfigEnv: dir + "/"}, apiNow); aerr != nil {
		t.Fatal(aerr)
	}
	if len(asked) != 1 || asked[0] != KeychainService(dir+"/") {
		t.Fatalf("keychain asked %q", asked)
	}
	if got := api.got[0].Get("Authorization"); got != "Bearer keychain-token" {
		t.Fatalf("Authorization = %q", got)
	}

	// No Keychain item: the file's token.
	p.Keychain = func(context.Context, string) ([]byte, error) { return nil, errors.New("not found") }
	if _, aerr := p.Measure(context.Background(), Account{Harness: wire.HarnessClaude, Dir: dir}, apiNow); aerr != nil {
		t.Fatal(aerr)
	}
	if got := api.got[1].Get("Authorization"); got != "Bearer "+claudeToken {
		t.Fatalf("Authorization = %q", got)
	}
}

// The Keychain service names (the hashes computed apart with
// `printf %s <dir> | shasum -a 256`): CLAUDE_CONFIG_DIR as set.
func TestKeychainService(t *testing.T) {
	cases := []struct{ env, want string }{
		{"", "Claude Code-credentials"},
		{"/Users/u/.claude-work", "Claude Code-credentials-87169a1d"},
		{"/tmp/claude config", "Claude Code-credentials-d63d722d"},
	}
	for _, c := range cases {
		if got := KeychainService(c.env); got != c.want {
			t.Errorf("KeychainService(%q) = %q, want %q", c.env, got, c.want)
		}
	}
}

// Calling reads the credentials and auth.json without changing a byte,
// whatever the API answers.
func TestUsageAPILeavesCredentialsAlone(t *testing.T) {
	cdir, xdir := claudeDir(t, later), codexDir(t)
	files := []string{filepath.Join(cdir, ".credentials.json"), filepath.Join(xdir, "auth.json")}
	before := map[string][]byte{}
	stat := map[string]time.Time{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		before[f] = b
		fi, _ := os.Stat(f)
		stat[f] = fi.ModTime()
	}
	for _, status := range []int{200, 401, 403, 429, 500} {
		api := newFakeAPI(t)
		api.status, api.body = status, `{}`
		p := &PrivateAPI{ClaudeURL: api.URL, CodexURL: api.URL}
		p.Measure(context.Background(), Account{Harness: wire.HarnessClaude, Dir: cdir}, apiNow)
		p.Measure(context.Background(), Account{Harness: wire.HarnessCodex, Dir: xdir}, apiNow)
	}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		fi, _ := os.Stat(f)
		if !bytes.Equal(b, before[f]) || !fi.ModTime().Equal(stat[f]) {
			t.Fatalf("%s changed", f)
		}
	}
	for _, dir := range []string{cdir, xdir} {
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("%s now holds %d files", dir, len(entries))
		}
	}
}

// The schedule: after a success the next call is 15 minutes on (5 when
// the usage screen opens); failures wait 30 s, doubling up to 15 minutes;
// a 429 waits for its Retry-After; a refused, unscoped or missing token an
// hour. Nothing is called while the app is not in front unless the screen
// opens.
func TestAPISchedule(t *testing.T) {
	var s APISchedule
	now := apiNow
	if !s.Due(now, true, false) || s.Due(now, false, false) {
		t.Fatal("a first call is due only in front")
	}
	s.Done(now, nil)
	for _, d := range []time.Duration{time.Minute, 14*time.Minute + 59*time.Second} {
		if s.Due(now.Add(d), true, false) {
			t.Fatalf("due %v after a success", d)
		}
	}
	if s.Due(now.Add(4*time.Minute), true, true) || !s.Due(now.Add(5*time.Minute), false, true) {
		t.Fatal("opening the screen calls 5 minutes after the last call")
	}
	if !s.Due(now.Add(15*time.Minute), true, false) || s.Due(now.Add(time.Hour), false, false) {
		t.Fatal("due after 15 minutes in front only")
	}

	now = now.Add(15 * time.Minute)
	server := &APIError{Code: wire.UsageErrServer}
	var waits []time.Duration
	for range 7 {
		next := s.Done(now, server)
		waits = append(waits, next.Sub(now))
		if s.Due(next.Add(-time.Second), true, true) || !s.Due(next, true, false) {
			t.Fatalf("not due exactly at %v", next.Sub(now))
		}
		now = next
	}
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("waits = %v, want %v", waits, want)
		}
	}

	s.Done(now, nil)
	now = now.Add(15 * time.Minute)
	if next := s.Done(now, &APIError{Code: wire.UsageErrRateLimited, RetryAt: now.Add(10 * time.Minute)}); next != now.Add(10*time.Minute) {
		t.Fatalf("429 waits until %v", next.Sub(now))
	}
	if s.Due(now.Add(9*time.Minute), true, true) {
		t.Fatal("called before Retry-After")
	}
	for _, code := range []string{wire.UsageErrUnauthorized, wire.UsageErrForbidden, wire.UsageErrNoCredentials} {
		if next := s.Done(now, &APIError{Code: code}); next != now.Add(time.Hour) {
			t.Fatalf("%s waits %v", code, next.Sub(now))
		}
	}
}

// What the private API brings sits with the other sources' windows: the
// weekly windows of a model after the week, extra usage (no reset time)
// last and never marked reset; another source's later measurement keeps
// the private API's error, the next success of the API drops it.
func TestLimitsKeepPrivateAPIWindowsAndErrors(t *testing.T) {
	l := OpenLimits(filepath.Join(t.TempDir(), "usage-limits.json"))
	const dir = "/home/u/.claude"
	reset := apiNow.Add(3 * time.Hour).UnixMilli()
	if _, err := l.Record(wire.HarnessClaude, dir, Measurement{Source: wire.UsageSourcePrivateAPI, At: apiNow, Windows: []Window{
		{ID: wire.UsageWindowExtraUsage, UsedPercent: 24},
		{ID: "seven_day_opus", UsedPercent: 12, ResetsAt: reset},
		{ID: wire.UsageWindowFiveHour, UsedPercent: 40, ResetsAt: reset},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordError(wire.HarnessClaude, dir, wire.UsageLimitError{Code: wire.UsageErrServer, At: apiNow.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	l.Record(wire.HarnessClaude, dir, Measurement{Source: wire.UsageSourceStatusLine, At: apiNow, Windows: []Window{{ID: wire.UsageWindowFiveHour, UsedPercent: 45, ResetsAt: reset}}})
	report := func() []wire.UsageLimitWindow { return l.Report(apiNow.Add(time.Hour))[0].Windows }
	ws := report()
	var ids []string
	for _, w := range ws {
		ids = append(ids, w.Window)
		if w.Reset || w.Error == nil || w.Error.Code != wire.UsageErrServer {
			t.Fatalf("%s = %+v", w.Window, w)
		}
	}
	if strings.Join(ids, ",") != "five_hour,seven_day_opus,extra_usage" || *ws[0].UsedPercent != 45 || ws[0].Source != wire.UsageSourceStatusLine {
		t.Fatalf("windows %v, five_hour %+v", ids, ws[0])
	}
	l.Record(wire.HarnessClaude, dir, Measurement{Source: wire.UsageSourcePrivateAPI, At: apiNow, Windows: []Window{{ID: wire.UsageWindowExtraUsage, UsedPercent: 10}}})
	for _, w := range report() {
		if w.Error != nil {
			t.Fatalf("%s kept its error after a success", w.Window)
		}
		if w.Window == wire.UsageWindowExtraUsage && *w.UsedPercent != 10 {
			t.Fatalf("extra usage = %v (a lower share without reset time is taken)", *w.UsedPercent)
		}
	}
}
