package notify

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// chatCall is a request Discord's or Telegram's API received.
type chatCall struct {
	method, path, query string
	body                map[string]any
}

func (c chatCall) String() string { return c.method + " " + c.path + "?" + c.query }

// chatAPI makes s send what it addresses to discord.com and
// api.telegram.org to a test server standing in for both: it numbers the
// messages it takes from first (Telegram's chat is 555) and answers
// anything else with success, unless fail (may be nil) names a status for
// a call. Every call is returned, in order.
func chatAPI(t *testing.T, s *Sender, first int, fail func(chatCall) int) <-chan chatCall {
	t.Helper()
	ch := make(chan chatCall, 32)
	var mu sync.Mutex
	sent := first - 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c := chatCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		if len(b) > 0 {
			if err := json.Unmarshal(b, &c.body); err != nil {
				t.Errorf("%s body %q: %v", c, b, err)
			}
		}
		ch <- c
		if fail != nil {
			if code := fail(c); code != 0 {
				w.WriteHeader(code)
				io.WriteString(w, `{"ok": false}`)
				return
			}
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(c.path, "/sendMessage"):
			sent++
			fmt.Fprintf(w, `{"ok": true, "result": {"message_id": %d, "chat": {"id": 555}}}`, sent)
		case c.method == http.MethodPost && strings.HasPrefix(c.path, "/api/webhooks/") && !strings.Contains(c.path, "/messages/"):
			sent++
			fmt.Fprintf(w, `{"id": "%d"}`, sent)
		case c.method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			io.WriteString(w, `{"ok": true}`)
		}
	}))
	t.Cleanup(srv.Close)
	redirectChat(s, srv.Listener.Addr().String())
	return ch
}

// chatSender is a Sender keeping its chat messages in the file it returns
// and collecting its log lines.
func chatSender(t *testing.T, dir string) (*Sender, string, func() []string) {
	t.Helper()
	path := filepath.Join(dir, "chat-messages.json")
	var mu sync.Mutex
	var logged []string
	s := NewSender(filepath.Join(dir, "notify-errors.json"), path, testHostID, func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf(format, args...))
	})
	t.Cleanup(s.Close)
	return s, path, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(logged)
	}
}

// drain closes s and returns the calls the chat API received.
func drain(s *Sender, ch <-chan chatCall) []chatCall {
	s.Close()
	var calls []chatCall
	for {
		select {
		case c := <-ch:
			calls = append(calls, c)
		default:
			return calls
		}
	}
}

// storedRefs reads the chat messages file.
func storedRefs(t *testing.T, path string) []chatRef {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var refs []chatRef
	if err := json.Unmarshal(b, &refs); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return refs
}

const (
	testDiscordURL  = "discord://123456/tok_EN-x"
	testDiscordPath = "/api/webhooks/123456/tok_EN-x"
	testTelegramURL = "tgram://123456789:AAbb_cc/555"
	testTelegramBot = "/bot123456789:AAbb_cc/"
	testSession     = "00112233aabbccdd"
)

func chatConfig(url string) wire.NotifyConfig {
	return wire.NotifyConfig{
		Chat:      wire.ChatConfig{Enabled: true, URL: url},
		HostLabel: "box",
		ClickPage: "https://sshterm.iru-yo.com/open",
		Lang:      wire.LangJa,
	}
}

func approvalOf(sessionID string) wire.NotificationData {
	return wire.NotificationData{Title: "claude · api", Body: "承認待ち: Bash", Level: wire.LevelInfo, Reason: "needs_approval", SessionID: sessionID}
}

// The next message of a session is sent first, then the older one is
// deleted; only the newer stays in the chat messages file.
func TestChatReplaceSendsThenDeletes(t *testing.T) {
	for _, c := range []struct {
		name, url string
		send      string // path of a send
		check     func(t *testing.T, del chatCall)
	}{
		{"discord", testDiscordURL, testDiscordPath, func(t *testing.T, del chatCall) {
			if del.method != http.MethodDelete || del.path != testDiscordPath+"/messages/1" {
				t.Errorf("delete %s, want DELETE %s/messages/1", del, testDiscordPath)
			}
		}},
		{"telegram", testTelegramURL, testTelegramBot + "sendMessage", func(t *testing.T, del chatCall) {
			want := map[string]any{"chat_id": "555", "message_id": 1.0}
			if del.method != http.MethodPost || del.path != testTelegramBot+"deleteMessage" || !reflect.DeepEqual(del.body, want) {
				t.Errorf("delete %s %v, want POST %sdeleteMessage %v", del, del.body, testTelegramBot, want)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path, logged := chatSender(t, t.TempDir())
			api := chatAPI(t, s, 1, nil)
			cfg := chatConfig(c.url)
			s.Send(cfg, approvalOf(testSession))
			s.Send(cfg, approvalOf(testSession))
			calls := drain(s, api)
			if len(calls) != 3 || calls[0].path != c.send || calls[1].path != c.send {
				t.Fatalf("calls %v, want two sends then a delete", calls)
			}
			c.check(t, calls[2])
			if refs := storedRefs(t, path); len(refs) != 1 || refs[0].MessageID != "2" || !slices.Equal(refs[0].Sessions, []string{testSession}) {
				t.Fatalf("stored %+v, want message 2 only", refs)
			}
			if l := logged(); len(l) != 0 {
				t.Fatalf("logged %q", l)
			}
		})
	}
}

// A settled session's message is edited to resolved, not deleted: ☑️ in
// place of the reason's emoji, 「（解決済み）」 after the title, the body
// as it was and the same link button (Discord: a grey embed; Telegram:
// reply_markup again). It is then forgotten.
func TestChatResolvedKeepsButton(t *testing.T) {
	const title = "☑️ box · claude · api（解決済み）"
	for _, c := range []struct {
		name, url string
		check     func(t *testing.T, sent, edit chatCall)
	}{
		{"discord", testDiscordURL, func(t *testing.T, sent, edit chatCall) {
			if edit.method != http.MethodPatch || edit.path != testDiscordPath+"/messages/1" || edit.query != "with_components=true" {
				t.Fatalf("edit %s, want PATCH %s/messages/1?with_components=true", edit, testDiscordPath)
			}
			embeds := []any{map[string]any{"title": title, "description": "承認待ち: Bash", "color": float64(discordResolvedColor)}}
			if !reflect.DeepEqual(edit.body["embeds"], embeds) {
				t.Errorf("embeds %v, want %v", edit.body["embeds"], embeds)
			}
			if sent.body["components"] == nil || !reflect.DeepEqual(edit.body["components"], sent.body["components"]) {
				t.Errorf("components %v, want those sent %v", edit.body["components"], sent.body["components"])
			}
		}},
		{"telegram", testTelegramURL, func(t *testing.T, sent, edit chatCall) {
			if edit.method != http.MethodPost || edit.path != testTelegramBot+"editMessageText" {
				t.Fatalf("edit %s, want POST %seditMessageText", edit, testTelegramBot)
			}
			b := edit.body
			if b["chat_id"] != "555" || b["message_id"] != 1.0 || b["text"] != title+"\n承認待ち: Bash" {
				t.Errorf("edit body %v", b)
			}
			if sent.body["reply_markup"] == nil || !reflect.DeepEqual(b["reply_markup"], sent.body["reply_markup"]) {
				t.Errorf("reply_markup %v, want the one sent %v", b["reply_markup"], sent.body["reply_markup"])
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path, _ := chatSender(t, t.TempDir())
			api := chatAPI(t, s, 1, nil)
			cfg := chatConfig(c.url)
			s.Send(cfg, approvalOf(testSession))
			s.Clear(cfg, testSession)
			calls := drain(s, api)
			if len(calls) != 2 {
				t.Fatalf("calls %v, want a send and an edit", calls)
			}
			c.check(t, calls[0], calls[1])
			if refs := storedRefs(t, path); len(refs) != 0 {
				t.Fatalf("stored %+v after resolving", refs)
			}
		})
	}
}

// A digest's message is edited to resolved once all its sessions have
// settled; a later message of one of them does not delete it.
func TestChatDigestResolvedWhenAllSettle(t *testing.T) {
	s, path, _ := chatSender(t, t.TempDir())
	api := chatAPI(t, s, 1, nil)
	cfg := chatConfig(testDiscordURL)
	cfg.Lang = wire.LangEn
	const a, b = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	s.SendDigest(cfg, []wire.NotificationData{approvalOf(a), approvalOf(b)})
	s.Send(cfg, approvalOf(a)) // message 2: a's own
	s.Clear(cfg, a)
	s.Close()
	if refs := storedRefs(t, path); len(refs) != 1 || refs[0].MessageID != "1" || !refs[0].Digest || !slices.Equal(refs[0].Sessions, []string{b}) {
		t.Fatalf("stored %+v, want the digest, waiting for b", refs)
	}
	calls := drain(s, api)
	if len(calls) != 3 || calls[2].method != http.MethodPatch || calls[2].path != testDiscordPath+"/messages/2" {
		t.Fatalf("calls %v, want two sends and the edit of a's message", calls)
	}

	s2, _, _ := chatSender(t, filepath.Dir(path))
	api = chatAPI(t, s2, 1, nil)
	s2.Clear(cfg, a) // settled again: the digest still waits for b
	s2.Clear(cfg, b)
	calls = drain(s2, api)
	if len(calls) != 1 || calls[0].method != http.MethodPatch || calls[0].path != testDiscordPath+"/messages/1" || calls[0].query != "" {
		t.Fatalf("calls %v, want the edit of the digest", calls)
	}
	want := []any{map[string]any{"title": "☑️ box: 2 approvals requested (resolved)", "description": "claude · api: 承認待ち: Bash\nclaude · api: 承認待ち: Bash", "color": float64(discordResolvedColor)}}
	if !reflect.DeepEqual(calls[0].body["embeds"], want) || calls[0].body["components"] != nil {
		t.Fatalf("digest edit %v, want embeds %v and no button", calls[0].body, want)
	}
	if refs := storedRefs(t, path); len(refs) != 0 {
		t.Fatalf("stored %+v after resolving", refs)
	}
}

// After a restart, the messages are back: a session that is still there
// gets its message resolved when it settles (Clear), a gone one at once
// (ResolveChat).
func TestChatRestoredAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s, path, _ := chatSender(t, dir)
	api := chatAPI(t, s, 1, nil)
	cfg := chatConfig(testTelegramURL)
	const live, gone = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb"
	s.Send(cfg, approvalOf(live))
	s.Send(cfg, approvalOf(gone))
	if calls := drain(s, api); len(calls) != 2 {
		t.Fatalf("calls %v", calls)
	}

	s2, _, _ := chatSender(t, dir)
	api = chatAPI(t, s2, 3, nil)
	if got := s2.ChatSessions(cfg); !slices.Equal(got, []string{live, gone}) {
		t.Fatalf("restored sessions %v", got)
	}
	if got := s2.ChatSessions(chatConfig(testDiscordURL)); len(got) != 0 {
		t.Fatalf("sessions of another chat URL %v", got)
	}
	s2.ResolveChat(cfg, gone)
	s2.Send(cfg, approvalOf(live)) // replaces message 1
	s2.Clear(cfg, live)
	calls := drain(s2, api)
	var got []string
	for _, c := range calls {
		got = append(got, fmt.Sprintf("%s %v", strings.TrimPrefix(c.path, testTelegramBot), c.body["message_id"]))
	}
	want := []string{"editMessageText 2", "sendMessage <nil>", "deleteMessage 1", "editMessageText 3"}
	if !slices.Equal(got, want) {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if refs := storedRefs(t, path); len(refs) != 0 {
		t.Fatalf("stored %+v", refs)
	}
}

// When notify.chat.url changes or chat is removed, the messages sent
// through the old URL are forgotten, never edited through it.
func TestChatForgottenWhenURLChanges(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  wire.NotifyConfig
	}{
		{"changed", chatConfig("discord://654321/other")},
		{"chat null", wire.NotifyConfig{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			s, path, _ := chatSender(t, dir)
			api := chatAPI(t, s, 1, nil)
			old := chatConfig(testDiscordURL)
			s.Send(old, approvalOf(testSession))
			s.Reconfigure(c.cfg)
			s.Close()
			if refs := storedRefs(t, path); len(refs) != 0 {
				t.Fatalf("stored %+v after the URL changed", refs)
			}
			if calls := drain(s, api); len(calls) != 1 {
				t.Fatalf("calls %v, want the send only", calls)
			}

			// Restarted with the new config: settling goes nowhere near
			// the old URL, and Reconfigure forgets them.
			s2, _, _ := chatSender(t, dir)
			api = chatAPI(t, s2, 1, nil)
			s2.Send(old, approvalOf(testSession))
			drain(s2, api)
			s3, _, _ := chatSender(t, dir)
			api3 := chatAPI(t, s3, 1, nil)
			s3.Clear(c.cfg, testSession)
			s3.Reconfigure(c.cfg)
			if calls := drain(s3, api3); len(calls) != 0 {
				t.Fatalf("calls %v through the old URL", calls)
			}
			if refs := storedRefs(t, path); len(refs) != 0 {
				t.Fatalf("stored %+v after the URL changed", refs)
			}
		})
	}
}

// state/chat-messages.json holds ids, never the chat URL or its token,
// and only its owner reads it.
func TestChatMessagesFileHoldsNoSecret(t *testing.T) {
	for _, u := range []string{"tgram://123456789:AAsecret_token/555", "discord://123456/secret_token"} {
		s, path, _ := chatSender(t, t.TempDir())
		api := chatAPI(t, s, 1, nil)
		s.Send(chatConfig(u), approvalOf(testSession))
		drain(s, api)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "secret") || strings.Contains(string(b), "123456789:") || strings.Contains(string(b), "123456/") || !strings.Contains(string(b), `"message_id":"1"`) {
			t.Fatalf("%s: chat messages file %s", u, b)
		}
		if refs := storedRefs(t, path); len(refs) != 1 || refs[0].URLSHA256 != chatFingerprint(u) || len(refs[0].URLSHA256) != 64 {
			t.Fatalf("stored %+v", refs)
		}
		if fi, err := os.Stat(path); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
			t.Fatalf("chat messages file %v %v, want 0600", fi.Mode(), err)
		}
	}
}

// A message that cannot be deleted is edited to resolved instead (e.g.
// Telegram's after 48 hours); one that cannot be edited either, a 429 or
// a connection error is forgotten with one log line, a 404 silently. None
// is retried or becomes a failed push (last_error).
func TestChatReplaceAndResolveFailures(t *testing.T) {
	isDelete := func(c chatCall) bool {
		return c.method == http.MethodDelete || strings.HasSuffix(c.path, "/deleteMessage")
	}
	isEdit := func(c chatCall) bool {
		return c.method == http.MethodPatch || strings.HasSuffix(c.path, "/editMessageText")
	}
	for _, c := range []struct {
		name, url string
		fail      func(chatCall) int
		resolve   bool     // Clear after one send, instead of a second send
		calls     []string // after the sends: deletes and edits
		logs      int
	}{
		{name: "telegram past 48 hours", url: testTelegramURL,
			fail:  func(c chatCall) int { return status(isDelete(c), http.StatusBadRequest) },
			calls: []string{"delete", "edit"}},
		{name: "neither deleted nor edited", url: testTelegramURL,
			fail:  func(c chatCall) int { return status(isDelete(c) || isEdit(c), http.StatusBadRequest) },
			calls: []string{"delete", "edit"}, logs: 1},
		{name: "deleted already", url: testDiscordURL,
			fail:  func(c chatCall) int { return status(isDelete(c), http.StatusNotFound) },
			calls: []string{"delete"}},
		{name: "delete rate limited", url: testDiscordURL,
			fail:  func(c chatCall) int { return status(isDelete(c), http.StatusTooManyRequests) },
			calls: []string{"delete"}, logs: 1},
		{name: "edit rate limited", url: testTelegramURL, resolve: true,
			fail:  func(c chatCall) int { return status(isEdit(c), http.StatusTooManyRequests) },
			calls: []string{"edit"}, logs: 1},
		{name: "edit of a gone message", url: testDiscordURL, resolve: true,
			fail:  func(c chatCall) int { return status(isEdit(c), http.StatusNotFound) },
			calls: []string{"edit"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, path, logged := chatSender(t, t.TempDir())
			api := chatAPI(t, s, 1, c.fail)
			cfg := chatConfig(c.url)
			s.Send(cfg, approvalOf(testSession))
			sends, kept := 2, 1
			if c.resolve {
				s.Clear(cfg, testSession)
				sends, kept = 1, 0
			} else {
				s.Send(cfg, approvalOf(testSession))
			}
			calls := drain(s, api)
			var got []string
			for _, call := range calls[min(sends, len(calls)):] {
				switch {
				case isDelete(call):
					got = append(got, "delete")
				case isEdit(call):
					got = append(got, "edit")
				default:
					got = append(got, call.String())
				}
			}
			if !slices.Equal(got, c.calls) {
				t.Fatalf("calls after the sends %q, want %q", got, c.calls)
			}
			if l := logged(); len(l) != c.logs || strings.Contains(strings.Join(l, ""), "AAbb_cc") || strings.Contains(strings.Join(l, ""), "tok_EN") {
				t.Fatalf("logged %q, want %d line(s) without the URL", l, c.logs)
			}
			if refs := storedRefs(t, path); len(refs) != kept {
				t.Fatalf("stored %+v, want %d", refs, kept)
			}
			if f, ok := s.LastErrors()[wire.ChannelChat]; ok {
				t.Fatalf("last_error %+v", f)
			}
		})
	}
}

func status(match bool, code int) int {
	if match {
		return code
	}
	return 0
}

// Slack names no message: it only gets pushes, never a delete or edit, and
// nothing is kept.
func TestChatSlackSendsOnly(t *testing.T) {
	s, path, _ := chatSender(t, t.TempDir())
	got := fakeChat(t, s, http.StatusOK, "ok")
	cfg := chatConfig("slack://T000/B000/XXXX")
	s.Send(cfg, approvalOf(testSession))
	s.Send(cfg, approvalOf(testSession))
	s.Clear(cfg, testSession)
	s.Close()
	if n := len(got); n != 2 {
		t.Fatalf("%d requests, want the two pushes", n)
	}
	if _, err := os.Stat(path); err == nil {
		if refs := storedRefs(t, path); len(refs) != 0 {
			t.Fatalf("stored %+v", refs)
		}
	}
}
