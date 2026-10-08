package notify

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// chatReply is one answer of a scripted chat service.
type chatReply struct {
	status     int
	retryAfter string // Retry-After header, "" for none
	body       string
}

// scriptedChat makes s send its chat requests to a test server giving the
// replies in turn (the last one again once they run out) and counts the
// requests.
func scriptedChat(t *testing.T, s *Sender, replies ...chatReply) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		i := int(n.Add(1)) - 1
		c := replies[min(i, len(replies)-1)]
		if c.retryAfter != "" {
			w.Header().Set("Retry-After", c.retryAfter)
		}
		w.WriteHeader(c.status)
		io.WriteString(w, c.body)
	}))
	t.Cleanup(srv.Close)
	redirectChat(s, srv.Listener.Addr().String())
	return &n
}

const (
	kindDiscordURL  = "https://discord.com/api/webhooks/123456/AAdiscord_tok"
	kindSlackURL    = "slack://T000/B000/slacksecret"
	kindTelegramURL = "tgram://123456789:AAtelegram_tok/123456"
)

// A failed send to the chat destination has a kind and an error of the
// status line followed by the service's short reason.
func TestChatFailureKinds(t *testing.T) {
	for _, c := range []struct {
		name, url string
		reply     chatReply
		kind      string
		err       string
	}{
		{"discord unknown webhook", kindDiscordURL, chatReply{status: 404, body: `{"message": "Unknown Webhook", "code": 10015}`},
			wire.FailureRevoked, "404 Not Found: Unknown Webhook (10015)"},
		{"discord invalid token", kindDiscordURL, chatReply{status: 401, body: `{"message": "Invalid Webhook Token", "code": 50027}`},
			wire.FailureRevoked, "401 Unauthorized: Invalid Webhook Token (50027)"},
		{"discord bad form", kindDiscordURL, chatReply{status: 400, body: `{"message": "Invalid Form Body", "code": 50035, "errors": {}}`},
			wire.FailureRejected, "400 Bad Request: Invalid Form Body (50035)"},
		{"discord rate limit", kindDiscordURL, chatReply{status: 429, body: `{"message": "You are being rate limited.", "retry_after": 64.5, "global": false}`},
			wire.FailureRateLimited, "429 Too Many Requests: You are being rate limited."},
		{"discord 502", kindDiscordURL, chatReply{status: 502, body: `<html>bad gateway</html>`},
			wire.FailureServer, "502 Bad Gateway"},
		{"telegram revoked token", kindTelegramURL, chatReply{status: 401, body: `{"ok": false, "error_code": 401, "description": "Unauthorized"}`},
			wire.FailureRevoked, "401 Unauthorized"},
		{"telegram blocked", kindTelegramURL, chatReply{status: 403, body: `{"ok": false, "error_code": 403, "description": "Forbidden: bot was blocked by the user"}`},
			wire.FailureUnreachable, "403 Forbidden: bot was blocked by the user"},
		{"telegram chat not found", kindTelegramURL, chatReply{status: 400, body: `{"ok": false, "error_code": 400, "description": "Bad Request: chat not found"}`},
			wire.FailureUnreachable, "400 Bad Request: chat not found"},
		{"telegram supergroup", kindTelegramURL, chatReply{status: 400, body: `{"ok": false, "error_code": 400, "description": "Bad Request: group chat was upgraded to a supergroup chat", "parameters": {"migrate_to_chat_id": -1001234567890}}`},
			wire.FailureUnreachable, "400 Bad Request: group chat was upgraded to a supergroup chat"},
		{"telegram bad request", kindTelegramURL, chatReply{status: 400, body: `{"ok": false, "error_code": 400, "description": "Bad Request: message text is empty"}`},
			wire.FailureRejected, "400 Bad Request: message text is empty"},
		// The kind follows error_code, not the HTTP status.
		{"telegram error_code", kindTelegramURL, chatReply{status: 400, body: `{"ok": false, "error_code": 403, "description": "Forbidden: bot was kicked from the group chat"}`},
			wire.FailureUnreachable, "400 Bad Request: Forbidden: bot was kicked from the group chat"},
		{"telegram 500", kindTelegramURL, chatReply{status: 500, body: `{"ok": false, "error_code": 500, "description": "Internal Server Error"}`},
			wire.FailureServer, "500 Internal Server Error"},
		{"slack no service", kindSlackURL, chatReply{status: 404, body: "no_service"},
			wire.FailureRevoked, "404 Not Found: no_service"},
		{"slack channel not found", kindSlackURL, chatReply{status: 404, body: "channel_not_found"},
			wire.FailureUnreachable, "404 Not Found: channel_not_found"},
		{"slack archived", kindSlackURL, chatReply{status: 410, body: "channel_is_archived\n"},
			wire.FailureUnreachable, "410 Gone: channel_is_archived"},
		{"slack invalid payload", kindSlackURL, chatReply{status: 400, body: "invalid_payload"},
			wire.FailureRejected, "400 Bad Request: invalid_payload"},
		{"slack rate limit", kindSlackURL, chatReply{status: 429, retryAfter: "30", body: "rate_limited"},
			wire.FailureRateLimited, "429 Too Many Requests: rate_limited"},
		{"slack html", kindSlackURL, chatReply{status: 503, body: "<html>Service Unavailable</html>"},
			wire.FailureServer, "503 Service Unavailable"},
	} {
		s := newSender(t)
		got := scriptedChat(t, s, c.reply)
		err := s.Push(context.Background(), wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: c.url}}, wire.NotificationData{Title: "t"})
		f := s.LastErrors()[wire.ChannelChat]
		if f.Kind != c.kind || f.Error != c.err || f.Status != c.reply.status {
			t.Errorf("%s: last error %+v, want kind %s error %q", c.name, f, c.kind, c.err)
		}
		if err == nil || err.Error() != "chat: "+c.err {
			t.Errorf("%s: push error %v", c.name, err)
		}
		if n := got.Load(); n != 1 {
			t.Errorf("%s: %d requests, want 1 (no resend, no migrate_to_chat_id)", c.name, n)
		}
	}

	// No status line: the network.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	s := newSender(t)
	redirectChat(s, dead)
	s.Push(context.Background(), wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: kindDiscordURL}}, wire.NotificationData{Title: "t"})
	if f := s.LastErrors()[wire.ChannelChat]; f.Kind != wire.FailureNetwork || f.Status != 0 || f.Error == "" {
		t.Errorf("refused connection: last error %+v", f)
	}

	// ntfy and the generic webhook get no kind and no reason.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		io.WriteString(w, `{"message": "Unknown Webhook", "code": 10015}`)
	}))
	defer srv.Close()
	s.Push(context.Background(), wire.NotifyConfig{Webhook: wire.WebhookConfig{Enabled: true, URL: srv.URL}}, wire.NotificationData{Title: "t"})
	if f := s.LastErrors()[wire.ChannelWebhook]; f.Kind != "" || f.Error != "404 Not Found" {
		t.Errorf("webhook: last error %+v", f)
	}
}

// A reason holding the URL, a token or the URL's path is dropped whole;
// control characters are dropped and a reason is 200 characters at most.
func TestChatReasonHidesSecrets(t *testing.T) {
	for _, c := range []struct {
		name, url string
		reply     chatReply
		err       string
	}{
		{"discord token", kindDiscordURL, chatReply{status: 404, body: `{"message": "Unknown Webhook AAdiscord_tok", "code": 10015}`}, "404 Not Found"},
		{"discord url", kindDiscordURL, chatReply{status: 400, body: `{"message": "bad ` + kindDiscordURL + `", "code": 1}`}, "400 Bad Request"},
		{"discord path", "discord://123456/AAdiscord_tok", chatReply{status: 400, body: `{"message": "no route /api/webhooks/123456/", "code": 1}`}, "400 Bad Request: no route /api/webhooks/123456/ (1)"},
		{"discord full path", "discord://123456/AAdiscord_tok", chatReply{status: 400, body: `{"message": "no route /api/webhooks/123456/AAdiscord_tok", "code": 1}`}, "400 Bad Request"},
		{"telegram token", kindTelegramURL, chatReply{status: 401, body: `{"ok": false, "error_code": 401, "description": "Unauthorized: 123456789:AAtelegram_tok"}`}, "401 Unauthorized"},
		{"telegram secret", kindTelegramURL, chatReply{status: 400, body: `{"ok": false, "error_code": 400, "description": "Bad Request: AAtelegram_tok"}`}, "400 Bad Request"},
		{"telegram path", kindTelegramURL, chatReply{status: 404, body: `{"ok": false, "error_code": 404, "description": "Not Found: /bot123456789:AAtelegram_tok/sendMessage"}`}, "404 Not Found"},
		{"slack webhook token", kindSlackURL, chatReply{status: 403, body: "slacksecret"}, "403 Forbidden"},
		{"control characters", kindDiscordURL, chatReply{status: 400, body: `{"message": "bad\u001b[31m\nform\u0000", "code": 50035}`}, "400 Bad Request: bad[31mform (50035)"},
		{"long", kindDiscordURL, chatReply{status: 400, body: `{"message": "` + strings.Repeat("é", 300) + `", "code": 50035}`}, "400 Bad Request: " + strings.Repeat("é", 200)},
	} {
		s := newSender(t)
		scriptedChat(t, s, c.reply)
		err := s.Push(context.Background(), wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: c.url}}, wire.NotificationData{Title: "t"})
		if f := s.LastErrors()[wire.ChannelChat]; f.Error != c.err {
			t.Errorf("%s: last error %q, want %q", c.name, f.Error, c.err)
		}
		if err == nil || err.Error() != "chat: "+c.err {
			t.Errorf("%s: push error %v", c.name, err)
		}
	}
}

// A queued push answered 429 with a wait of Timeout at most is sent once
// more after that wait, and only a failed resend is recorded; a longer
// wait, a 5xx and notify.test (Push) are not resent.
func TestChatRateLimitResend(t *testing.T) {
	ok := chatReply{status: 200, body: `{"id": "1", "ok": true, "result": {"message_id": 7, "chat": {"id": 123456}}}`}
	for _, c := range []struct {
		name, url string
		replies   []chatReply
		requests  int32
		slept     []time.Duration
		kind      string // of the last error, "" for none
	}{
		{"discord json", kindDiscordURL, []chatReply{{status: 429, body: `{"message": "You are being rate limited.", "retry_after": 1.5, "global": false}`}, ok},
			2, []time.Duration{1500 * time.Millisecond}, ""},
		{"discord header", kindDiscordURL, []chatReply{{status: 429, retryAfter: "2", body: `<html></html>`}, ok},
			2, []time.Duration{2 * time.Second}, ""},
		{"telegram", kindTelegramURL, []chatReply{{status: 429, body: `{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 5", "parameters": {"retry_after": 5}}`}, ok},
			2, []time.Duration{5 * time.Second}, ""},
		{"slack fails again", kindSlackURL, []chatReply{{status: 429, retryAfter: "3", body: "rate_limited"}},
			2, []time.Duration{3 * time.Second}, wire.FailureRateLimited},
		{"discord too long", kindDiscordURL, []chatReply{{status: 429, body: `{"message": "You are being rate limited.", "retry_after": 5.01}`}, ok},
			1, nil, wire.FailureRateLimited},
		{"telegram too long", kindTelegramURL, []chatReply{{status: 429, body: `{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 6", "parameters": {"retry_after": 6}}`}, ok},
			1, nil, wire.FailureRateLimited},
		{"no wait named", kindSlackURL, []chatReply{{status: 429, body: "rate_limited"}, ok},
			1, nil, wire.FailureRateLimited},
		{"server", kindDiscordURL, []chatReply{{status: 503, retryAfter: "1"}, ok},
			1, nil, wire.FailureServer},
	} {
		s := newSender(t)
		var slept []time.Duration
		s.sleep = func(d time.Duration) { slept = append(slept, d) }
		got := scriptedChat(t, s, c.replies...)
		if !s.Send(wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: c.url}}, wire.NotificationData{Title: "t"}) {
			t.Fatal("send rejected")
		}
		s.Close()
		if n := got.Load(); n != c.requests {
			t.Errorf("%s: %d requests, want %d", c.name, n, c.requests)
		}
		if fmt.Sprint(slept) != fmt.Sprint(c.slept) {
			t.Errorf("%s: waited %v, want %v", c.name, slept, c.slept)
		}
		f, failed := s.LastErrors()[wire.ChannelChat]
		if failed != (c.kind != "") || f.Kind != c.kind {
			t.Errorf("%s: last error %+v, want kind %q", c.name, f, c.kind)
		}
	}

	// notify.test answers right away.
	s := newSender(t)
	s.sleep = func(d time.Duration) { t.Errorf("notify.test waited %v", d) }
	got := scriptedChat(t, s, chatReply{status: 429, body: `{"message": "You are being rate limited.", "retry_after": 0.5}`}, ok)
	err := s.Push(context.Background(), wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: kindDiscordURL}}, wire.NotificationData{Title: "t"})
	if err == nil || got.Load() != 1 || s.LastErrors()[wire.ChannelChat].Kind != wire.FailureRateLimited {
		t.Fatalf("notify.test: %v after %d requests, last error %+v", err, got.Load(), s.LastErrors())
	}
}

// Queued pushes keep going to a destination the service disabled; while
// the same kind of failure repeats, only the first is logged.
func TestChatFailureLoggedOnce(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	s := NewSender(filepath.Join(t.TempDir(), "notify-errors.json"), "", "", logf)
	revoked := chatReply{status: 404, body: `{"message": "Unknown Webhook", "code": 10015}`}
	server := chatReply{status: 500}
	ok := chatReply{status: 200, body: `{"id": "1"}`}
	got := scriptedChat(t, s, revoked, revoked, revoked, server, revoked, ok, revoked)
	cfg := wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: kindDiscordURL}}
	for range 7 {
		if !s.Send(cfg, wire.NotificationData{Title: "t"}) {
			t.Fatal("send rejected")
		}
	}
	s.Close()
	if n := got.Load(); n != 7 {
		t.Fatalf("%d of 7 pushes sent", n)
	}
	want := []string{
		"notify: chat: 404 Not Found: Unknown Webhook (10015)",
		"notify: chat: 500 Internal Server Error",
		"notify: chat: 404 Not Found: Unknown Webhook (10015)",
		"notify: chat: 404 Not Found: Unknown Webhook (10015)",
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(logs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("logged %q, want %q", logs, want)
	}
}

// Taking a new chat URL forgets the old URL's failure, also one a push
// queued before the change only records afterwards. The first config
// (the daemon starting) and disabling the chat keep it.
func TestChatURLChangeForgetsFailure(t *testing.T) {
	s := newSender(t)
	release := make(chan struct{})
	var gated atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if gated.Load() {
			<-release
		}
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message": "Unknown Webhook", "code": 10015}`)
	}))
	t.Cleanup(srv.Close)
	redirectChat(s, srv.Listener.Addr().String())
	old := wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: kindDiscordURL}}
	n := wire.NotificationData{Title: "t"}
	revoked := func() bool { return s.LastErrors()[wire.ChannelChat].Kind == wire.FailureRevoked }

	if s.Push(context.Background(), old, n) == nil || !revoked() {
		t.Fatalf("no revoked failure: %+v", s.LastErrors())
	}
	s.Reconfigure(old)
	if !revoked() {
		t.Fatal("the first config forgot the failure")
	}
	disabled := old
	disabled.Chat.Enabled = false
	s.Reconfigure(disabled)
	if !revoked() {
		t.Fatal("disabling the chat forgot the failure")
	}

	gated.Store(true)
	if !s.Send(old, n) {
		t.Fatal("send rejected")
	}
	s.Reconfigure(wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: kindTelegramURL}})
	if _, failed := s.LastErrors()[wire.ChannelChat]; failed {
		t.Fatal("a new URL kept the old URL's failure")
	}
	close(release)
	s.Close()
	if f, failed := s.LastErrors()[wire.ChannelChat]; failed {
		t.Fatalf("a push queued for the old URL recorded %+v after the change", f)
	}
}
