package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

func TestParseChat(t *testing.T) {
	for _, c := range []struct {
		url, service string
	}{
		{"https://discord.com/api/webhooks/123456/tok_EN-x", ServiceDiscord},
		{"https://discordapp.com/api/webhooks/123456/tok_EN-x", ServiceDiscord},
		{"discord://123456/tok_EN-x", ServiceDiscord},
		{"https://hooks.slack.com/services/T000/B000/XXXX", ServiceSlack},
		{"slack://T000/B000/XXXX", ServiceSlack},
		{"tgram://123456789:AAbb_cc-DD/123456", ServiceTelegram},
		{"tgram://123456789:AAbb_cc-DD/-1001234567890", ServiceTelegram},
		{"tgram://123456789:AAbb_cc-DD/@my_channel", ServiceTelegram},
	} {
		d, err := ParseChat(c.url)
		if err != nil || d.Service != c.service {
			t.Errorf("ParseChat(%q) = %q, %v; want %q", c.url, d.Service, err, c.service)
		}
	}
	// SECRET stands for each URL's token.
	for _, u := range []string{
		"",
		"http://discord.com/api/webhooks/123456/SECRET",           // not https
		"https://discord.com.evil.example/api/webhooks/1/SECRET",  // not Discord's host
		"https://evil.example/api/webhooks/123456/SECRET",         //
		"https://discord.com/api/webhooks/abc/SECRET",             // id not a number
		"https://discord.com/api/webhooks/123456",                 // no token
		"https://discord.com/api/webhooks/123456/SECRET/extra",    //
		"https://discord.com/api/webhooks/123456/SECRET?thread=1", // a query of its own
		"https://discord.com:8443/api/webhooks/123456/SECRET",     //
		"https://user@discord.com/api/webhooks/123456/SECRET",     //
		"discord://123456",                                         //
		"https://hooks.slack.com/services/T000/B000",               // two parts
		"https://hooks.slack.com/triggers/T000/123/SECRET",         // Workflow Builder
		"https://hooks.slack.com/workflows/T000/A000/123/SECRET",   //
		"https://hooks.slack.example/services/T000/B000/SECRET",    //
		"slack://T000/B000",                                        //
		"slack://T000/B000/SECRET/YY",                              //
		"tgram://123456789:SECRET/",                                // no chat id
		"tgram://123456789:SECRET/chat",                            // neither a number nor @name
		"tgram://SECRET/123",                                       //
		"tgram://123456789:SECRET/123/456",                         //
		"https://api.telegram.org/bot123456789:SECRET/sendMessage", // Telegram takes tgram://
		"https://example.com/SECRET",                               // a generic webhook
	} {
		if d, err := ParseChat(u); err == nil {
			t.Errorf("ParseChat(%q) accepted as %q", u, d.Service)
		} else if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "123456") {
			t.Errorf("ParseChat(%q) error repeats the URL: %v", u, err)
		}
	}
}

// chatRequest is a request a chat service received.
type chatRequest struct {
	host, path, query string
	header            http.Header
	body              map[string]any
}

// fakeChat makes s send what it addresses to discord.com, hooks.slack.com
// and api.telegram.org to a test server answering status and reply, and
// returns the requests it receives. Other hosts are left alone.
func fakeChat(t *testing.T, s *Sender, status int, reply string) <-chan chatRequest {
	t.Helper()
	ch := make(chan chatRequest, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(b, &body); err != nil {
			t.Errorf("chat request body %q: %v", b, err)
		}
		ch <- chatRequest{r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), body}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	redirectChat(s, srv.Listener.Addr().String())
	return ch
}

// redirectChat makes s send its chat requests to addr over plain HTTP.
func redirectChat(s *Sender, addr string) {
	s.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Host {
		case "discord.com", "hooks.slack.com", "api.telegram.org":
			r = r.Clone(r.Context())
			r.URL.Scheme, r.URL.Host = "http", addr
		}
		return http.DefaultTransport.RoundTrip(r)
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// testPageLink is PageURL for the open page, testHostID and approval's
// session.
const testPageLink = "https://sshterm.iru-yo.com/open#h=" + testHostID + "&s=00112233aabbccdd"

// Each service gets its own format, titled with the host and the reason's
// emoji, with a link to the open page when there is one, and pushChat
// returns the message's ids where the service names them.
func TestPushChatFormats(t *testing.T) {
	approval := wire.NotificationData{Title: "box · claude · <api> & co", Body: "Needs approval: Bash @everyone <!channel>", Level: wire.LevelInfo, Reason: "needs_approval", SessionID: "00112233aabbccdd"}
	exited := wire.NotificationData{Title: "box · codex · api", Body: "Exited (code 1)", Level: wire.LevelWarn, Reason: "exited"}
	for _, c := range []struct {
		name      string
		url       string
		n         wire.NotificationData
		link      string
		reply     string
		host      string
		path      string
		query     string
		body      string // JSON
		reference ChatMessage
	}{
		{
			name: "discord link", url: "discord://123456/tok", n: approval, link: testPageLink,
			reply: `{"id": "8"}`,
			host:  "discord.com", path: "/api/webhooks/123456/tok", query: "wait=true&with_components=true",
			body: `{"embeds": [{"title": "🔒 box · claude · <api> & co", "description": "Needs approval: Bash @everyone <!channel>"}], "allowed_mentions": {"parse": []},
			  "components": [{"type": 1, "components": [{"type": 2, "style": 5, "label": "SSH Term で開く", "url": "` + testPageLink + `"}]}]}`,
			reference: ChatMessage{ID: "8"},
		},
		{
			name: "slack link", url: "slack://T000/B000/XXXX", n: approval, link: testPageLink, reply: "ok",
			host: "hooks.slack.com", path: "/services/T000/B000/XXXX",
			body: `{"text": "*🔒 box · claude · &lt;api&gt; &amp; co*\nNeeds approval: Bash @everyone &lt;!channel&gt;\n<https://sshterm.iru-yo.com/open#h=` + testHostID + `&amp;s=00112233aabbccdd|SSH Term で開く>", "unfurl_links": false}`,
		},
		{
			name: "telegram link", url: "tgram://123456789:AAbb_cc/123456", n: approval, link: testPageLink,
			reply: `{"ok": true, "result": {"message_id": 9, "chat": {"id": 123456}}}`,
			host:  "api.telegram.org", path: "/bot123456789:AAbb_cc/sendMessage",
			body: `{"chat_id": "123456", "text": "🔒 box · claude · <api> & co\nNeeds approval: Bash @everyone <!channel>", "link_preview_options": {"is_disabled": true},
			  "reply_markup": {"inline_keyboard": [[{"text": "SSH Term で開く", "url": "` + testPageLink + `"}]]}}`,
			reference: ChatMessage{ChatID: "123456", ID: "9"},
		},
		{
			name: "discord", url: "https://discordapp.com/api/webhooks/123456/tok_EN-x", n: approval,
			reply: `{"id": "1100000000000000001", "channel_id": "99"}`,
			host:  "discord.com", path: "/api/webhooks/123456/tok_EN-x", query: "wait=true",
			body:      `{"embeds": [{"title": "🔒 box · claude · <api> & co", "description": "Needs approval: Bash @everyone <!channel>"}], "allowed_mentions": {"parse": []}}`,
			reference: ChatMessage{ID: "1100000000000000001"},
		},
		{
			name: "discord warn", url: "discord://123456/tok", n: exited,
			reply: `{"id": "7"}`,
			host:  "discord.com", path: "/api/webhooks/123456/tok", query: "wait=true",
			body:      `{"embeds": [{"title": "⚠️ box · codex · api", "description": "Exited (code 1)", "color": 16705372}], "allowed_mentions": {"parse": []}}`,
			reference: ChatMessage{ID: "7"},
		},
		{
			name: "slack", url: "slack://T000/B000/XXXX", n: approval, reply: "ok",
			host: "hooks.slack.com", path: "/services/T000/B000/XXXX",
			body: `{"text": "*🔒 box · claude · &lt;api&gt; &amp; co*\nNeeds approval: Bash @everyone &lt;!channel&gt;", "unfurl_links": false}`,
		},
		{
			name: "telegram", url: "tgram://123456789:AAbb_cc/@my_channel", n: exited,
			reply: `{"ok": true, "result": {"message_id": 42, "chat": {"id": -1001234567890, "type": "channel"}, "text": "…"}}`,
			host:  "api.telegram.org", path: "/bot123456789:AAbb_cc/sendMessage",
			body:      `{"chat_id": "@my_channel", "text": "⚠️ box · codex · api\nExited (code 1)", "link_preview_options": {"is_disabled": true}}`,
			reference: ChatMessage{ChatID: "-1001234567890", ID: "42"},
		},
		{
			name: "telegram test", url: "tgram://123456789:AAbb_cc/123456",
			n:     wire.NotificationData{Title: "box · SSH Term", Body: "Test notification", Level: wire.LevelInfo, Reason: "test"},
			reply: `{"ok": true, "result": {"message_id": 5, "chat": {"id": 123456}}}`,
			host:  "api.telegram.org", path: "/bot123456789:AAbb_cc/sendMessage",
			body:      `{"chat_id": "123456", "text": "box · SSH Term\nTest notification", "link_preview_options": {"is_disabled": true}}`,
			reference: ChatMessage{ChatID: "123456", ID: "5"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSender(t)
			got := fakeChat(t, s, http.StatusOK, c.reply)
			ref, err := pushChat(context.Background(), s.client, c.url, c.n, c.link, "SSH Term で開く")
			if err != nil {
				t.Fatal(err)
			}
			if ref != c.reference {
				t.Errorf("reference %+v, want %+v", ref, c.reference)
			}
			r := <-got
			if r.host != c.host || r.path != c.path || r.query != c.query {
				t.Errorf("request to %s %s ?%s, want %s %s ?%s", r.host, r.path, r.query, c.host, c.path, c.query)
			}
			if ct := r.header.Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type %q", ct)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(c.body), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.body, want) {
				t.Errorf("body %v\nwant %v", r.body, want)
			}
		})
	}
}

// A hand-set generic webhook and the chat destination both get every push.
func TestPushWebhookAndChat(t *testing.T) {
	hook, hooked := recorder(t, 200)
	s := newSender(t)
	chat := fakeChat(t, s, http.StatusOK, `{"id": "1"}`)
	cfg := wire.NotifyConfig{
		Webhook:   wire.WebhookConfig{Enabled: true, URL: hook.URL + "/hook"},
		Chat:      wire.ChatConfig{Enabled: true, URL: "discord://123456/tok"},
		HostLabel: "box",
	}
	if !Enabled(wire.NotifyConfig{Chat: cfg.Chat}) {
		t.Fatal("a chat destination alone is not enabled")
	}
	if !s.Send(cfg, wire.NotificationData{Title: "claude · api", Body: "Turn complete", Reason: "turn_complete"}) {
		t.Fatal("send rejected")
	}
	for range 2 {
		select {
		case c := <-hooked:
			if c.path != "/hook" || !strings.Contains(c.body, `"box · claude · api"`) {
				t.Errorf("webhook got %s %s", c.path, c.body)
			}
		case r := <-chat:
			if r.host != "discord.com" {
				t.Errorf("chat request to %s", r.host)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a push did not arrive")
		}
	}
}

// With notify.click_page, the chat destination links a push of one session
// to the open page (a digest, notify.test and a push of no session get no
// button) and the generic webhook adds click_url (the host only without a
// session); without click_page neither links.
func TestPushLinksToOpenPage(t *testing.T) {
	hook, hooked := recorder(t, 200)
	s := newSender(t)
	chat := fakeChat(t, s, http.StatusOK, `{"id": "1"}`)
	const page = "https://sshterm.iru-yo.com/open"
	hostOnly := page + "#h=" + testHostID
	one := wire.NotificationData{Title: "claude · api", Body: "Turn complete", Reason: "turn_complete", SessionID: "00112233aabbccdd"}
	for _, c := range []struct {
		name      string
		clickPage string
		n         wire.NotificationData
		chatLink  string // "" = no button
		clickURL  string // "" = no click_url
	}{
		{"session", page, one, testPageLink, testPageLink},
		{"digest", page, Fold([]wire.NotificationData{one, one}, wire.LangEn), "", hostOnly},
		{"notify.test", page, wire.NotificationData{Title: "SSH Term", Body: "Test notification from stagent", Reason: "test"}, "", hostOnly},
		{"no session", page, wire.NotificationData{Title: "claude", Body: "Needs approval", Reason: "needs_approval"}, "", hostOnly},
		{"no click_page", "", one, "", ""},
	} {
		cfg := wire.NotifyConfig{
			Webhook:   wire.WebhookConfig{Enabled: true, URL: hook.URL},
			Chat:      wire.ChatConfig{Enabled: true, URL: "discord://123456/tok"},
			ClickBase: "sshtermx://open", // ntfy's only
			ClickPage: c.clickPage,
			Lang:      wire.LangEn,
		}
		if err := s.Push(context.Background(), cfg, c.n); err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte((<-hooked).body), &body); err != nil {
			t.Fatal(err)
		}
		if got, _ := body["click_url"].(string); got != c.clickURL || (c.clickURL == "") == (body["click_url"] != nil) {
			t.Errorf("%s: webhook click_url %v, want %q", c.name, body["click_url"], c.clickURL)
		}
		if body["reason"] != c.n.Reason || body["body"] != c.n.Body {
			t.Errorf("%s: webhook body %v", c.name, body)
		}
		r := <-chat
		components, _ := r.body["components"].([]any)
		if c.chatLink == "" {
			if r.query != "wait=true" || components != nil {
				t.Errorf("%s: chat got a button: ?%s %v", c.name, r.query, components)
			}
			continue
		}
		want := []any{map[string]any{"type": 1.0, "components": []any{map[string]any{"type": 2.0, "style": 5.0, "label": "Open in SSH Term", "url": c.chatLink}}}}
		if r.query != "wait=true&with_components=true" || !reflect.DeepEqual(components, want) {
			t.Errorf("%s: chat ?%s components %v, want %v", c.name, r.query, components, want)
		}
	}
}

// The chat URL (Telegram: the bot token) never shows in a push error, the
// last errors (what config.get and stagent doctor show) or the log.
func TestChatSecretStaysOut(t *testing.T) {
	const secretURL = "tgram://123456789:AAsecret_token/123456"
	var mu sync.Mutex
	var logged strings.Builder
	logf := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(&logged, format+"\n", args...)
	}
	path := filepath.Join(t.TempDir(), "notify-errors.json")
	s := NewSender(path, "", "", logf)
	defer s.Close()
	cfg := wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: secretURL}}
	n := wire.NotificationData{Title: "t", Body: "b"}
	checkFree := func(what, text string) {
		t.Helper()
		if strings.Contains(text, "secret") || strings.Contains(text, "123456789") {
			t.Fatalf("%s holds the chat URL: %s", what, text)
		}
	}

	got := fakeChat(t, s, http.StatusTooManyRequests, `{"ok": false, "error_code": 429, "description": "Too Many Requests: retry after 5"}`)
	err := s.Push(context.Background(), cfg, n)
	<-got
	if err == nil || !strings.Contains(err.Error(), "429 Too Many Requests") {
		t.Fatalf("push error %v", err)
	}
	checkFree("push error", err.Error())
	if f := s.LastErrors()[wire.ChannelChat]; f.Status != 429 || f.Kind != wire.FailureRateLimited || f.Error != "429 Too Many Requests: retry after 5" {
		t.Fatalf("chat failure %+v", f)
	}

	// A transport error: net/http's message names the request URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	redirectChat(s, dead)
	err = s.Push(context.Background(), cfg, n)
	if err == nil {
		t.Fatal("a refused connection is not reported")
	}
	checkFree("push error", err.Error())
	if !s.Send(cfg, n) {
		t.Fatal("send rejected")
	}
	s.Close()
	f := s.LastErrors()[wire.ChannelChat]
	if f.Error == "" || f.Status != 0 {
		t.Fatalf("chat failure %+v", f)
	}
	checkFree("last error", f.Error)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	checkFree("failures file", string(b))
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(logged.String(), "chat:") {
		t.Fatalf("the queued failure was not logged: %q", logged.String())
	}
	checkFree("log", logged.String())

	// An unreadable URL (a hand-edited config.json) fails the same way.
	s2 := NewSender(path, "", "", logf)
	defer s2.Close()
	err = s2.Push(context.Background(), wire.NotifyConfig{Chat: wire.ChatConfig{Enabled: true, URL: "tgram://123456789:AAsecret_token/not-a-chat"}}, n)
	if err == nil {
		t.Fatal("an unreadable chat URL was sent")
	}
	checkFree("push error", err.Error())
}

func TestOnly(t *testing.T) {
	all := wire.NotifyConfig{
		Ntfy:    wire.NtfyConfig{Enabled: true, Topic: "t"},
		Webhook: wire.WebhookConfig{Enabled: true, URL: "https://hook.example"},
		Chat:    wire.ChatConfig{URL: "discord://1/t"}, // disabled
	}
	if got, err := Only(all, nil); err != nil || !reflect.DeepEqual(got, all) {
		t.Fatalf("no channels named: %+v %v", got, err)
	}
	got, err := Only(all, []string{wire.ChannelWebhook, wire.ChannelChat})
	if err != nil || got.Ntfy.Enabled || !got.Webhook.Enabled || got.Chat.Enabled {
		t.Fatalf("webhook and chat: %+v %v", got, err)
	}
	if got, _ := Only(all, []string{wire.ChannelChat}); Enabled(got) {
		t.Fatalf("a disabled channel named alone is enabled: %+v", got)
	}
	if _, err := Only(all, []string{"ntfy", "email"}); err == nil {
		t.Fatal("an unknown channel was accepted")
	}
}
