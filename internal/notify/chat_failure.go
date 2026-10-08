package notify

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/obutora/stagent/internal/wire"
)

// maxChatReason bounds the service's reason in a chat error (characters).
const maxChatReason = 200

// chatError is a failed send to the chat destination: its kind
// (wire.Failure*) and, for a 429, the wait the service asks for
// (retryAfter; negative when it names none). err is the status error,
// with the service's reason, or the transport error.
type chatError struct {
	kind       string
	retryAfter time.Duration
	err        error
}

func (e *chatError) Error() string { return e.err.Error() }
func (e *chatError) Unwrap() error { return e.err }

// resendable reports whether a new push failing with err is sent once
// more after err's wait: a 429 naming a wait of Timeout at most.
func resendable(err error) (time.Duration, bool) {
	var ce *chatError
	if !errors.As(err, &ce) || ce.kind != wire.FailureRateLimited || ce.retryAfter < 0 || ce.retryAfter > Timeout {
		return 0, false
	}
	return ce.retryAfter, true
}

var (
	// slackReasonRe is a Slack error body worth showing (no_service,
	// channel_not_found, …).
	slackReasonRe = regexp.MustCompile(`^[a-z_]+$`)
	// telegramSupergroupRe is Telegram's 400 of a group that became a
	// supergroup, when it comes without migrate_to_chat_id.
	telegramSupergroupRe = regexp.MustCompile(`(?i)upgraded to a supergroup`)
)

// chatFailure classifies err, a failed send to the chat destination
// rawURL: a *chatError whose text is the status line followed by the
// service's short reason (Discord's message and code, Telegram's
// description, Slack's body when it is [a-z_]+), or the transport error
// (kind network). Telegram is classified by error_code, and its
// migrate_to_chat_id is never followed. An err that is no send (an
// unreadable URL) stays as it is.
func chatFailure(rawURL string, err error) error {
	if err == nil {
		return nil
	}
	d, perr := ParseChat(rawURL)
	if perr != nil {
		return err
	}
	var se *statusError
	if !errors.As(err, &se) {
		return &chatError{kind: wire.FailureNetwork, retryAfter: -1, err: err}
	}
	code, reason, kind := se.code, "", ""
	retryAfter := waitSeconds(se.retryAfter)
	switch d.Service {
	case ServiceDiscord:
		var r struct {
			Message    string   `json:"message"`
			Code       int      `json:"code"`
			RetryAfter *float64 `json:"retry_after"`
		}
		json.Unmarshal(se.body, &r)
		if reason = r.Message; reason != "" && r.Code != 0 {
			reason += fmt.Sprintf(" (%d)", r.Code)
		}
		if r.Code == 10015 || r.Code == 50027 { // Unknown Webhook, Invalid Webhook Token
			kind = wire.FailureRevoked
		}
		if r.RetryAfter != nil {
			retryAfter = seconds(*r.RetryAfter)
		}
	case ServiceTelegram:
		var r struct {
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
			Parameters  struct {
				RetryAfter      *float64 `json:"retry_after"`
				MigrateToChatID int64    `json:"migrate_to_chat_id"`
			} `json:"parameters"`
		}
		json.Unmarshal(se.body, &r)
		reason = r.Description
		if r.ErrorCode != 0 {
			code = r.ErrorCode
		}
		switch {
		case code == 401:
			kind = wire.FailureRevoked
		case code == 403, code == 400 && (r.Parameters.MigrateToChatID != 0 ||
			strings.Contains(strings.ToLower(r.Description), "chat not found") || telegramSupergroupRe.MatchString(r.Description)):
			kind = wire.FailureUnreachable
		}
		if r.Parameters.RetryAfter != nil {
			retryAfter = seconds(*r.Parameters.RetryAfter)
		}
	default: // ServiceSlack
		if body := strings.TrimSpace(string(se.body)); slackReasonRe.MatchString(body) {
			reason = body
		}
		switch reason {
		case "no_service", "no_active_hooks", "invalid_token", "team_disabled", "no_team":
			kind = wire.FailureRevoked
		case "channel_not_found", "channel_is_archived", "action_prohibited":
			kind = wire.FailureUnreachable
		}
	}
	if kind == "" {
		switch {
		case code == 429:
			kind = wire.FailureRateLimited
		case code >= 500:
			kind = wire.FailureServer
		default:
			kind = wire.FailureRejected
		}
	}
	e := *se
	e.reason = chatReason(se.status, reason, chatSecrets(rawURL, d))
	return &chatError{kind: kind, retryAfter: retryAfter, err: &e}
}

// chatReason cleans the service's reason: control characters dropped, the
// status line's own text not repeated (Telegram's "Forbidden: …"), 200
// characters at most. It is "" — the status line alone — when it holds
// any of secrets.
func chatReason(status, reason string, secrets []string) string {
	reason = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, reason))
	// Before cutting it short: a cut must not leave part of a secret.
	for _, s := range secrets {
		if s != "" && strings.Contains(reason, s) {
			return ""
		}
	}
	if _, text, ok := strings.Cut(status, " "); ok {
		if strings.EqualFold(reason, text) {
			return ""
		}
		if len(reason) > len(text) && strings.EqualFold(reason[:len(text)+1], text+":") {
			reason = strings.TrimSpace(reason[len(text)+1:])
		}
	}
	if r := []rune(reason); len(r) > maxChatReason {
		reason = string(r[:maxChatReason])
	}
	return reason
}

// chatSecrets are the parts of a chat URL no error may hold: the URL as
// written, the URL stagent posts to, its path, and the token (Discord's
// and Slack's webhook token, Telegram's bot token and its secret half).
func chatSecrets(rawURL string, d ChatDest) []string {
	secrets := []string{rawURL, d.endpoint}
	if _, rest, ok := strings.Cut(rawURL, "://"); ok {
		secrets = append(secrets, rest)
	}
	if u, err := url.Parse(d.endpoint); err == nil {
		secrets = append(secrets, u.Path)
		switch d.Service {
		case ServiceTelegram:
			token := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/bot"), "/sendMessage")
			_, secret, _ := strings.Cut(token, ":")
			secrets = append(secrets, token, secret)
		default:
			secrets = append(secrets, u.Path[strings.LastIndex(u.Path, "/")+1:])
		}
	}
	return secrets
}

// waitSeconds reads a Retry-After header of seconds; negative when there
// is none (an HTTP date included).
func waitSeconds(h string) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(h), 64)
	if err != nil {
		return -1
	}
	return seconds(f)
}

// seconds is f seconds; negative when f is not a wait.
func seconds(f float64) time.Duration {
	if f < 0 || math.IsNaN(f) || f > math.MaxInt64/float64(time.Second) {
		return -1
	}
	return time.Duration(f * float64(time.Second))
}
