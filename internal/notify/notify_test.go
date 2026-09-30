package notify

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

type captured struct {
	path   string
	header http.Header
	body   string
}

func recorder(t *testing.T, status int) (*httptest.Server, <-chan captured) {
	t.Helper()
	ch := make(chan captured, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- captured{r.URL.Path, r.Header.Clone(), string(b)}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func TestPushNtfy(t *testing.T) {
	srv, got := recorder(t, 200)
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL + "/", Topic: "my-topic", Token: "tk_secret"}}
	n := wire.NotificationData{Title: "claude · プロジェクト", Body: "Needs approval: Bash", Level: wire.LevelInfo, Reason: "needs_approval"}
	if err := Push(context.Background(), http.DefaultClient, cfg, n); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c.path != "/my-topic" || c.body != "Needs approval: Bash" {
		t.Fatalf("request %s %q", c.path, c.body)
	}
	title, err := new(mime.WordDecoder).DecodeHeader(c.header.Get("Title"))
	if err != nil || title != n.Title {
		t.Fatalf("Title header %q decodes to %q (%v)", c.header.Get("Title"), title, err)
	}
	if c.header.Get("Priority") != "high" || !strings.Contains(c.header.Get("Tags"), "lock") {
		t.Fatalf("priority/tags: %q %q", c.header.Get("Priority"), c.header.Get("Tags"))
	}
	if c.header.Get("Authorization") != "Bearer tk_secret" {
		t.Fatalf("auth header %q", c.header.Get("Authorization"))
	}
}

func TestPushWebhookAndErrors(t *testing.T) {
	srv, got := recorder(t, 200)
	cfg := wire.NotifyConfig{Webhook: wire.WebhookConfig{Enabled: true, URL: srv.URL + "/hook", Headers: map[string]string{"X-Key": "abc"}}}
	n := wire.NotificationData{Title: "codex · api", Body: "Turn complete", Level: wire.LevelInfo, Reason: "turn_complete"}
	if err := Push(context.Background(), http.DefaultClient, cfg, n); err != nil {
		t.Fatal(err)
	}
	c := <-got
	var body wire.NotificationData
	if err := json.Unmarshal([]byte(c.body), &body); err != nil || body != n {
		t.Fatalf("webhook body %q (%v)", c.body, err)
	}
	if c.header.Get("X-Key") != "abc" || c.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", c.header)
	}

	failing, _ := recorder(t, 500)
	cfg.Webhook.URL = failing.URL
	if err := Push(context.Background(), http.DefaultClient, cfg, n); err == nil {
		t.Fatal("a 500 response is not reported")
	}
}

func TestSenderDeliversAsyncAndSkipsDisabled(t *testing.T) {
	srv, got := recorder(t, 200)
	s := NewSender(t.Logf)
	if !s.Send(wire.NotifyConfig{}, wire.NotificationData{Title: "nothing enabled"}) {
		t.Fatal("disabled config should be a no-op success")
	}
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}}
	if !s.Send(cfg, wire.NotificationData{Title: "hello", Body: "b"}) {
		t.Fatal("send rejected")
	}
	select {
	case c := <-got:
		if c.body != "b" {
			t.Fatalf("body %q", c.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("push not delivered")
	}
	s.Close()
	if s.Send(cfg, wire.NotificationData{Title: "late"}) {
		t.Fatal("send after close accepted")
	}
	select {
	case c := <-got:
		t.Fatalf("unexpected push %+v", c)
	default:
	}
}

func TestFold(t *testing.T) {
	one := wire.NotificationData{Title: "a", Body: "Turn complete", Level: wire.LevelInfo, Reason: "turn_complete"}
	if got := Fold([]wire.NotificationData{one}); got != one {
		t.Fatalf("single notification changed: %+v", got)
	}
	three := Fold([]wire.NotificationData{one, one, one})
	if three.Reason != "digest" || three.Count != 3 || three.Title != "3 agents finished their turn" || three.Level != wire.LevelInfo {
		t.Fatalf("uniform digest %+v", three)
	}
	warn := wire.NotificationData{Title: "b", Body: "Exited with code 1", Level: wire.LevelWarn, Reason: "exited"}
	mixed := Fold([]wire.NotificationData{one, warn})
	if mixed.Count != 2 || mixed.Level != wire.LevelWarn || mixed.Title != "2 agent notifications" ||
		mixed.Body != "a: Turn complete\nb: Exited with code 1" {
		t.Fatalf("mixed digest %+v", mixed)
	}
}

// fakeTimer lets a test close digest windows by hand.
type fakeTimer struct {
	mu    sync.Mutex
	fires []func()
	d     []time.Duration
}

func (f *fakeTimer) after(d time.Duration, fn func()) func() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fires = append(f.fires, fn)
	f.d = append(f.d, d)
	return func() bool { return true }
}

func (f *fakeTimer) fire(i int) { f.fires[i]() }

func TestDigesterFoldsWithinWindow(t *testing.T) {
	var flushed []wire.NotificationData
	d := NewDigester(func(n wire.NotificationData) { flushed = append(flushed, n) })
	ft := &fakeTimer{}
	d.afterFunc = ft.after

	n := wire.NotificationData{Title: "s", Body: "Turn complete", Level: wire.LevelInfo, Reason: "turn_complete"}
	d.Add(n, 5*time.Second)
	d.Add(n, 5*time.Second)
	d.Add(n, 5*time.Second)
	if len(ft.fires) != 1 || ft.d[0] != 5*time.Second {
		t.Fatalf("windows opened: %d %v", len(ft.fires), ft.d)
	}
	if len(flushed) != 0 {
		t.Fatal("flushed before the window closed")
	}
	ft.fire(0)
	if len(flushed) != 1 || flushed[0].Count != 3 || flushed[0].Reason != "digest" {
		t.Fatalf("flushed %+v", flushed)
	}

	// A notification alone in the next window goes out unchanged.
	d.Add(n, 5*time.Second)
	if len(ft.fires) != 2 {
		t.Fatal("no new window after the first closed")
	}
	ft.fire(1)
	if len(flushed) != 2 || flushed[1] != n {
		t.Fatalf("single flushed %+v", flushed[1])
	}
}
