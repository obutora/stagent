package usage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// Private API (非公開 API): the undocumented usage endpoints Claude Code
// and Codex show their limits from, called with the login token the
// harness keeps (read only: never refreshed, never written) when
// usage.private_api is on. A help that may break at any time; the
// statusLine relay and the rollouts stay the main sources. No token ever
// goes into an error, a log line or a file of ours.

// The endpoints.
const (
	ClaudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	CodexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
)

const (
	// apiTimeout bounds one call, connection included.
	apiTimeout = 10 * time.Second
	// maxAPIAnswer bounds the answer read.
	maxAPIAnswer = 1 << 20
	// claudeKeychainService is the macOS Keychain service Claude Code
	// keeps its login in (KeychainService).
	claudeKeychainService = "Claude Code-credentials"
)

// PrivateAPI calls the private APIs. The zero value calls the real ones
// with http.DefaultClient and reads no Keychain.
type PrivateAPI struct {
	Client *http.Client
	// ClaudeURL and CodexURL replace the endpoints (tests).
	ClaudeURL string
	CodexURL  string
	// Keychain reads the password of a macOS Keychain generic password by
	// its service (MacKeychain on macOS; nil: none, the credentials file
	// only).
	Keychain func(ctx context.Context, service string) ([]byte, error)
}

// Account is whose limits are measured: a harness and its configuration
// directory (a clean path, the key its windows are kept under). ConfigEnv
// is the harness's variable as set (CLAUDE_CONFIG_DIR; "" when unset),
// which names Claude Code's Keychain item.
type Account struct {
	Harness   string
	Dir       string
	ConfigEnv string
}

// APIError is why a call measured nothing. Message never holds a token.
type APIError struct {
	Code    string // wire.UsageErr*
	Message string
	// RetryAt is the end of a 429's Retry-After (zero: none given).
	RetryAt time.Time
}

func (e *APIError) Error() string { return e.Message }

func apiErr(code, format string, args ...any) *APIError {
	return &APIError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Measure calls the private API of a.Harness (claude or codex) at now and
// returns the windows it answered, with Source private_api.
func (p *PrivateAPI) Measure(ctx context.Context, a Account, now time.Time) (Measurement, *APIError) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	switch a.Harness {
	case wire.HarnessClaude:
		return p.measureClaude(ctx, a, now)
	case wire.HarnessCodex:
		return p.measureCodex(ctx, a, now)
	}
	return Measurement{}, apiErr(wire.UsageErrNoCredentials, "%s has no private API", a.Harness)
}

func (p *PrivateAPI) measureClaude(ctx context.Context, a Account, now time.Time) (Measurement, *APIError) {
	token, aerr := p.claudeToken(ctx, a, now)
	if aerr != nil {
		return Measurement{}, aerr
	}
	// The endpoint serves Claude Code's own login, so the call names
	// itself as Claude Code (claude-code/<version>) in case the API turns
	// away other clients; whether the real API needs it is to be checked
	// (#1564). The version is fixed: nothing read depends on it.
	body, aerr := p.get(ctx, cmp.Or(p.ClaudeURL, ClaudeUsageURL), map[string]string{
		"Authorization":  "Bearer " + token,
		"anthropic-beta": "oauth-2025-04-20",
		"User-Agent":     "claude-code/2.1.0",
	}, now)
	if aerr != nil {
		return Measurement{}, aerr
	}
	ws, ok := claudeUsageWindows(body, now)
	if !ok {
		return Measurement{}, apiErr(wire.UsageErrBadResponse, "claude usage API: the answer is not the JSON expected")
	}
	return Measurement{Source: wire.UsageSourcePrivateAPI, Windows: ws, At: now}, nil
}

func (p *PrivateAPI) measureCodex(ctx context.Context, a Account, now time.Time) (Measurement, *APIError) {
	b, err := os.ReadFile(filepath.Join(a.Dir, "auth.json"))
	if err != nil {
		return Measurement{}, apiErr(wire.UsageErrNoCredentials, "codex: no auth.json in %s", a.Dir)
	}
	var auth struct {
		Tokens *struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
		} `json:"tokens"`
	}
	if json.Unmarshal(b, &auth) != nil || auth.Tokens == nil || auth.Tokens.AccessToken == "" {
		return Measurement{}, apiErr(wire.UsageErrNoCredentials, "codex: %s has no ChatGPT login token", filepath.Join(a.Dir, "auth.json"))
	}
	h := map[string]string{
		"Authorization": "Bearer " + auth.Tokens.AccessToken,
		"User-Agent":    "stagent/" + version.Version,
	}
	if auth.Tokens.AccountID != "" {
		h["ChatGPT-Account-Id"] = auth.Tokens.AccountID
	}
	body, aerr := p.get(ctx, cmp.Or(p.CodexURL, CodexUsageURL), h, now)
	if aerr != nil {
		return Measurement{}, aerr
	}
	m, ok := codexUsageWindows(body, now)
	if !ok {
		return Measurement{}, apiErr(wire.UsageErrBadResponse, "codex usage API: the answer is not the JSON expected")
	}
	return m, nil
}

// get calls url and returns the body of a 200 answer; any other answer or
// failure is an APIError by its kind.
func (p *PrivateAPI) get(ctx context.Context, url string, header map[string]string, now time.Time) ([]byte, *APIError) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apiErr(wire.UsageErrNetwork, "usage API: bad URL")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	c := p.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, apiErr(wire.UsageErrNetwork, "usage API %s: %s", req.URL.Host, netReason(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIAnswer))
	switch {
	case resp.StatusCode == http.StatusOK && err != nil:
		return nil, apiErr(wire.UsageErrNetwork, "usage API %s: the answer was cut off", req.URL.Host)
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode == http.StatusTooManyRequests:
		e := apiErr(wire.UsageErrRateLimited, "usage API %s: 429 Too Many Requests", req.URL.Host)
		e.RetryAt = retryAfter(resp.Header.Get("Retry-After"), now)
		return nil, e
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, apiErr(wire.UsageErrUnauthorized, "usage API %s: 401 Unauthorized (the login token was refused)", req.URL.Host)
	case resp.StatusCode == http.StatusForbidden:
		return nil, apiErr(wire.UsageErrForbidden, "usage API %s: 403 Forbidden (the login token lacks the scope)", req.URL.Host)
	}
	return nil, apiErr(wire.UsageErrServer, "usage API %s: HTTP %d", req.URL.Host, resp.StatusCode)
}

// netReason says why a call failed without repeating the request (whose
// URL holds no token, but keep error texts short and fixed).
func netReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer within 10 s"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}

// retryAfter reads a Retry-After header (seconds or an HTTP date) as a
// time after now; zero when absent or unreadable.
func retryAfter(v string, now time.Time) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s >= 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	if t, err := http.ParseTime(v); err == nil {
		return t
	}
	return time.Time{}
}

// KeychainService is the macOS Keychain service Claude Code keeps its
// login in: "Claude Code-credentials", with "-" and the first 8 hex digits
// of the SHA-256 of configEnv (CLAUDE_CONFIG_DIR as set) when it is set.
func KeychainService(configEnv string) string {
	if configEnv == "" {
		return claudeKeychainService
	}
	sum := sha256.Sum256([]byte(configEnv))
	return claudeKeychainService + "-" + hex.EncodeToString(sum[:])[:8]
}

// MacKeychain reads a generic password with /usr/bin/security.
func MacKeychain(ctx context.Context, service string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", service, "-w").Output()
	if err != nil {
		return nil, fmt.Errorf("keychain item %q not readable", service)
	}
	return out, nil
}

// claudeToken reads Claude Code's OAuth access token for a: the Keychain
// item first (macOS), else <dir>/.credentials.json.
func (p *PrivateAPI) claudeToken(ctx context.Context, a Account, now time.Time) (string, *APIError) {
	var expired bool
	if p.Keychain != nil {
		if b, err := p.Keychain(ctx, KeychainService(a.ConfigEnv)); err == nil {
			tok, exp, ok := claudeOAuth(b, now)
			if ok && !exp {
				return tok, nil
			}
			expired = expired || exp
		}
	}
	if b, err := os.ReadFile(filepath.Join(a.Dir, ".credentials.json")); err == nil {
		tok, exp, ok := claudeOAuth(b, now)
		if ok && !exp {
			return tok, nil
		}
		expired = expired || exp
	}
	if expired {
		return "", apiErr(wire.UsageErrUnauthorized, "claude: the login token has expired")
	}
	return "", apiErr(wire.UsageErrNoCredentials, "claude: no login token for %s", a.Dir)
}

// claudeOAuth reads claudeAiOauth.accessToken of Claude Code's credentials
// JSON and whether its expiresAt (unix ms) has passed.
func claudeOAuth(b []byte, now time.Time) (token string, expired, ok bool) {
	var c struct {
		OAuth *struct {
			AccessToken string          `json:"accessToken"`
			ExpiresAt   json.RawMessage `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(b, &c) != nil || c.OAuth == nil || c.OAuth.AccessToken == "" {
		return "", false, false
	}
	if at, ok := number(c.OAuth.ExpiresAt); ok && at > 0 && int64(at) <= now.UnixMilli() {
		return "", true, true
	}
	return c.OAuth.AccessToken, false, true
}

// claudeUsageWindows reads /api/oauth/usage answered at now: five_hour,
// seven_day and the weekly windows of one model (readWindow), and
// extra_usage when it is enabled (its utilization; its reset time is not
// told). A null or incomplete window is left out. ok is false when body is
// not a JSON object.
func claudeUsageWindows(body []byte, now time.Time) ([]Window, bool) {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil || top == nil {
		return nil, false
	}
	var models []string
	for k := range top {
		if _, ok := wire.UsageWindowModel(k); ok {
			models = append(models, k)
		}
	}
	slices.Sort(models)
	out := readWindows(top, append([]string{wire.UsageWindowFiveHour, wire.UsageWindowSevenDay}, models...), now)
	var extra struct {
		IsEnabled   bool            `json:"is_enabled"`
		Utilization json.RawMessage `json:"utilization"`
		ResetsAt    json.RawMessage `json:"resets_at"`
	}
	if json.Unmarshal(top[wire.UsageWindowExtraUsage], &extra) == nil && extra.IsEnabled {
		if pct, ok := number(extra.Utilization); ok && pct >= 0 {
			at, _ := resetTime(extra.ResetsAt)
			out = append(out, Window{ID: wire.UsageWindowExtraUsage, UsedPercent: pct, ResetsAt: at})
		}
	}
	return out, true
}

// codexUsageWindows reads wham/usage answered at now: plan_type and
// rate_limit's primary_window and secondary_window, read like the
// rollouts' windows (readWindowPair).
func codexUsageWindows(body []byte, now time.Time) (Measurement, bool) {
	var r struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   json.RawMessage `json:"primary_window"`
			Secondary json.RawMessage `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if json.Unmarshal(body, &r) != nil {
		return Measurement{}, false
	}
	m := Measurement{Source: wire.UsageSourcePrivateAPI, Plan: r.PlanType, At: now}
	if r.RateLimit != nil {
		m.Windows = readWindowPair(r.RateLimit.Primary, r.RateLimit.Secondary, now)
	}
	return m, true
}
