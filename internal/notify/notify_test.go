package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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

const testHostID = "0123456789abcdef0123456789abcdef"

// newSender returns a Sender keeping its failures in a temporary file.
func newSender(t *testing.T) *Sender {
	t.Helper()
	s := NewSender(filepath.Join(t.TempDir(), "notify-errors.json"), testHostID, t.Logf)
	t.Cleanup(s.Close)
	return s
}

func TestPushNtfy(t *testing.T) {
	srv, got := recorder(t, 200)
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL + "/", Topic: "my-topic", Token: "tk_secret"}, HostLabel: "開発機"}
	n := wire.NotificationData{Title: "claude · プロジェクト", Body: "Needs approval: Bash", Level: wire.LevelInfo, Reason: "needs_approval"}
	if err := newSender(t).Push(context.Background(), cfg, n); err != nil {
		t.Fatal(err)
	}
	c := <-got
	if c.path != "/my-topic" || c.body != "Needs approval: Bash" {
		t.Fatalf("request %s %q", c.path, c.body)
	}
	title, err := new(mime.WordDecoder).DecodeHeader(c.header.Get("Title"))
	if err != nil || title != "開発機 · claude · プロジェクト" {
		t.Fatalf("Title header %q decodes to %q (%v)", c.header.Get("Title"), title, err)
	}
	if c.header.Get("Priority") != "high" || !strings.Contains(c.header.Get("Tags"), "lock") {
		t.Fatalf("priority/tags: %q %q", c.header.Get("Priority"), c.header.Get("Tags"))
	}
	if c.header.Get("Authorization") != "Bearer tk_secret" {
		t.Fatalf("auth header %q", c.header.Get("Authorization"))
	}
}

// A push of one session links to the session and carries its id as the
// sequence ID; a digest links to the host only and has no sequence ID; no
// click_base means no link (the sequence ID stays).
func TestPushNtfyClickAndSequenceID(t *testing.T) {
	srv, got := recorder(t, 200)
	s := newSender(t)
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}, ClickBase: "sshtermx://open"}
	one := wire.NotificationData{Title: "claude · api", Body: "Turn complete", Reason: "turn_complete", SessionID: "00112233aabbccdd"}
	digest := Fold([]wire.NotificationData{one, one}, wire.LangEn)
	for _, c := range []struct {
		name      string
		clickBase string
		n         wire.NotificationData
		click     string
		seq       string
	}{
		{"session", "sshtermx://open", one, "sshtermx://open?h=" + testHostID + "&s=00112233aabbccdd", "00112233aabbccdd"},
		{"digest", "sshtermx://open", digest, "sshtermx://open?h=" + testHostID, ""},
		{"no click_base", "", one, "", "00112233aabbccdd"},
	} {
		cfg.ClickBase = c.clickBase
		if err := s.Push(context.Background(), cfg, c.n); err != nil {
			t.Fatal(err)
		}
		h := (<-got).header
		if h.Get("Click") != c.click || h.Get("X-Sequence-ID") != c.seq {
			t.Errorf("%s: Click %q X-Sequence-ID %q, want %q %q", c.name, h.Get("Click"), h.Get("X-Sequence-ID"), c.click, c.seq)
		}
		if _, ok := h["Click"]; c.click == "" && ok {
			t.Errorf("%s: empty Click header sent", c.name)
		}
	}
}

// Clear dismisses the session's notification on ntfy with the same
// sequence ID and token; webhooks and an id ntfy would refuse are handled.
func TestClearNtfy(t *testing.T) {
	srv, got := recorder(t, 200)
	s := newSender(t)
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t", Token: "tk"}}
	s.Clear(cfg, "00112233aabbccdd")
	c := <-got
	if c.path != "/t/00112233aabbccdd/clear" || c.header.Get("Authorization") != "Bearer tk" {
		t.Fatalf("clear request %s auth %q", c.path, c.header.Get("Authorization"))
	}
	odd := strings.Repeat("x", 65)
	s.Clear(cfg, odd)
	if c := <-got; c.path != "/t/"+SequenceID(odd)+"/clear" {
		t.Fatalf("clear of a long id: %s", c.path)
	}
	if seq := SequenceID(odd); len(seq) != 32 || seq == odd {
		t.Fatalf("SequenceID(%q) = %q", odd, seq)
	}
	if SequenceID("a.b") == "a.b" || SequenceID("A-z_09") != "A-z_09" {
		t.Fatal("sequence ID rules")
	}
	if !s.Clear(wire.NotifyConfig{Webhook: wire.WebhookConfig{Enabled: true, URL: srv.URL}}, "00112233aabbccdd") {
		t.Fatal("clear without ntfy rejected")
	}
	select {
	case c := <-got:
		t.Fatalf("webhook got a clear: %+v", c)
	case <-time.After(100 * time.Millisecond):
	}
}

// A server that answers a sequence-ID push without naming the sequence
// (ntfy before 2.16) gets the push as a plain one, logged once.
func TestPushNtfyWithoutReplacementSupport(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply string
		logs  int
	}{
		{"supported", `{"id":"abc","event":"message","sequence_id":"00112233aabbccdd"}`, 0},
		{"old server", `{"id":"abc","event":"message"}`, 1},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, c.reply)
		}))
		var mu sync.Mutex
		var logs []string
		s := NewSender(filepath.Join(t.TempDir(), "e.json"), testHostID, func(f string, a ...any) {
			mu.Lock()
			logs = append(logs, f)
			mu.Unlock()
		})
		cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}}
		n := wire.NotificationData{Title: "a", Body: "b", Reason: "turn_complete", SessionID: "00112233aabbccdd"}
		for range 2 {
			if err := s.Push(context.Background(), cfg, n); err != nil {
				t.Fatal(err)
			}
		}
		s.Close()
		srv.Close()
		if len(logs) != c.logs {
			t.Errorf("%s: logged %q", c.name, logs)
		}
	}
}

func TestWants(t *testing.T) {
	def := wire.Config{}.WithDefaults().Notify
	off, on := false, true
	custom := def
	custom.Reasons = wire.NotifyReasons{NeedsApproval: &off, Terminal: &on, Exited: wire.ExitedAll}
	custom = wire.Config{Notify: custom}.WithDefaults().Notify
	n := func(reason, level string) wire.NotificationData {
		return wire.NotificationData{Reason: reason, Level: level}
	}
	for _, c := range []struct {
		cfg  wire.NotifyConfig
		n    wire.NotificationData
		want bool
	}{
		{def, n("needs_approval", wire.LevelInfo), true},
		{def, n("waiting_input", wire.LevelInfo), true},
		{def, n("turn_complete", wire.LevelInfo), true},
		{def, n("terminal", wire.LevelInfo), false},
		{def, n("exited", wire.LevelInfo), false},
		{def, n("exited", wire.LevelWarn), true},
		{custom, n("needs_approval", wire.LevelInfo), false},
		{custom, n("waiting_input", wire.LevelInfo), true},
		{custom, n("terminal", wire.LevelInfo), true},
		{custom, n("exited", wire.LevelInfo), true},
	} {
		if got := Wants(c.cfg, c.n); got != c.want {
			t.Errorf("Wants(%+v, %s/%s) = %v", c.cfg.Reasons, c.n.Reason, c.n.Level, got)
		}
	}
	off2 := def
	off2.Reasons.Exited = wire.ExitedOff
	if Wants(off2, n("exited", wire.LevelWarn)) {
		t.Error("exited off still pushes an abnormal exit")
	}
}

func TestClickURL(t *testing.T) {
	for _, c := range []struct{ base, host, session, want string }{
		{"sshtermx://open", testHostID, "", "sshtermx://open?h=" + testHostID},
		{"sshtermx://open", testHostID, "00112233aabbccdd", "sshtermx://open?h=" + testHostID + "&s=00112233aabbccdd"},
		// Its own query is kept; only h and s are ours.
		{"https://example.com/o?x=1&s=stale", testHostID, "", "https://example.com/o?h=" + testHostID + "&x=1"},
		{"", testHostID, "00112233aabbccdd", ""},
		{"sshtermx://open", "", "00112233aabbccdd", ""},
		{"open", testHostID, "", ""},
	} {
		if got := ClickURL(c.base, c.host, c.session); got != c.want {
			t.Errorf("ClickURL(%q, %q, %q) = %q, want %q", c.base, c.host, c.session, got, c.want)
		}
	}
}

func TestPushWebhookAndErrors(t *testing.T) {
	srv, got := recorder(t, 200)
	s := newSender(t)
	cfg := wire.NotifyConfig{Webhook: wire.WebhookConfig{Enabled: true, URL: srv.URL + "/hook", Headers: map[string]string{"X-Key": "abc"}}, HostLabel: "box"}
	n := wire.NotificationData{Title: "codex · api", Body: "Turn complete", Level: wire.LevelInfo, Reason: "turn_complete", SessionID: "00112233aabbccdd"}
	if err := s.Push(context.Background(), cfg, n); err != nil {
		t.Fatal(err)
	}
	c := <-got
	var body wire.NotificationData
	want := n
	want.Title = "box · codex · api"
	if err := json.Unmarshal([]byte(c.body), &body); err != nil || body != want {
		t.Fatalf("webhook body %q (%v)", c.body, err)
	}
	if c.header.Get("X-Key") != "abc" || c.header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", c.header)
	}

	failing, _ := recorder(t, 500)
	cfg.Webhook.URL = failing.URL
	if err := s.Push(context.Background(), cfg, n); err == nil {
		t.Fatal("a 500 response is not reported")
	}
}

// Every push is titled with the host label, else the host name, else not
// at all; a digest takes a colon.
func TestLabelled(t *testing.T) {
	host := func() (string, error) { return "myhost", nil }
	noHost := func() (string, error) { return "", errors.New("no host name") }
	one := wire.NotificationData{Title: "claude · api", Reason: "turn_complete"}
	digest := wire.NotificationData{Title: "3 agents finished their turn", Reason: "digest", Count: 3}
	for _, c := range []struct {
		label    string
		hostname func() (string, error)
		n        wire.NotificationData
		want     string
	}{
		{"開発機", host, one, "開発機 · claude · api"},
		{"", host, one, "myhost · claude · api"},
		{"  ", host, one, "myhost · claude · api"},
		{"", noHost, one, "claude · api"},
		{"開発機", host, digest, "開発機: 3 agents finished their turn"},
		{"", host, digest, "myhost: 3 agents finished their turn"},
		{"", noHost, digest, "3 agents finished their turn"},
	} {
		got := labelled(c.n, c.label, c.hostname)
		if got.Title != c.want {
			t.Errorf("label %q, %q: title %q, want %q", c.label, c.n.Title, got.Title, c.want)
		}
		if got.Body != c.n.Body || got.Reason != c.n.Reason || got.Count != c.n.Count {
			t.Errorf("labelled changed more than the title: %+v", got)
		}
	}
	// The sender uses the host name when no label is configured.
	srv, got := recorder(t, 200)
	s := newSender(t)
	s.hostname = host
	cfg := wire.NotifyConfig{Ntfy: wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "t"}}
	if err := s.Push(context.Background(), cfg, digest); err != nil {
		t.Fatal(err)
	}
	if title, _ := new(mime.WordDecoder).DecodeHeader((<-got).header.Get("Title")); title != "myhost: 3 agents finished their turn" {
		t.Fatalf("Title %q", title)
	}
}

// A failed push is remembered per channel with the HTTP status and a
// message free of the topic and URL, kept across a restart (the file), and
// forgotten once a push to that channel succeeds.
func TestLastErrors(t *testing.T) {
	status := 429
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	// A port nobody listens on: a transport error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String() + "/hook-secret"
	ln.Close()

	path := filepath.Join(t.TempDir(), "notify-errors.json")
	s := NewSender(path, "", t.Logf)
	defer s.Close()
	cfg := wire.NotifyConfig{
		Ntfy:    wire.NtfyConfig{Enabled: true, Server: srv.URL, Topic: "topic-secret"},
		Webhook: wire.WebhookConfig{Enabled: true, URL: dead},
	}
	n := wire.NotificationData{Title: "t", Body: "b"}
	before := time.Now().UnixMilli()
	err = s.Push(context.Background(), cfg, n)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("push error %v", err)
	}
	last := s.LastErrors()
	ntfy, webhook := last[wire.ChannelNtfy], last[wire.ChannelWebhook]
	if len(last) != 2 || ntfy.Status != 429 || ntfy.Error != "429 Too Many Requests" || ntfy.At < before {
		t.Fatalf("ntfy failure %+v (all %+v)", ntfy, last)
	}
	if webhook.Status != 0 || webhook.Error == "" || strings.Contains(webhook.Error, "secret") || webhook.At < before {
		t.Fatalf("webhook failure %+v", webhook)
	}
	st, err := os.Stat(path)
	// Windows has no permission bits: Go reports 0666 for any writable file.
	if err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("failures file %v %v", st, err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "secret") {
		t.Fatalf("failures file %s", b)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc[wire.ChannelWebhook]["status"]; ok {
		t.Fatalf("a transport error has a status: %s", b)
	}

	// A restarted sender loads them.
	s2 := NewSender(path, "", t.Logf)
	defer s2.Close()
	if got := s2.LastErrors(); !reflect.DeepEqual(got, last) {
		t.Fatalf("reloaded %+v, want %+v", got, last)
	}

	// Success clears that channel only; the queued path records too.
	mu.Lock()
	status = 200
	mu.Unlock()
	if !s2.Send(cfg, n) {
		t.Fatal("send rejected")
	}
	s2.Close()
	if got := s2.LastErrors(); len(got) != 1 || got[wire.ChannelWebhook].Error == "" || got[wire.ChannelWebhook].At < webhook.At {
		t.Fatalf("after ntfy success %+v", got)
	}
	reloaded, err := LoadFailures(path)
	if err != nil || len(reloaded) != 1 || reloaded[wire.ChannelWebhook].Error == "" {
		t.Fatalf("file after ntfy success %+v %v", reloaded, err)
	}
	cfg.Webhook.Enabled = false
	s3 := NewSender(path, "", t.Logf)
	defer s3.Close()
	if err := s3.Push(context.Background(), cfg, n); err != nil {
		t.Fatal(err)
	}
	if got := s3.LastErrors(); len(got) != 1 {
		t.Fatalf("a disabled channel's failure was cleared: %+v", got)
	}
	cfg.Webhook = wire.WebhookConfig{Enabled: true, URL: srv.URL}
	if err := s3.Push(context.Background(), cfg, n); err != nil {
		t.Fatal(err)
	}
	if got := s3.LastErrors(); got == nil || len(got) != 0 {
		t.Fatalf("after webhook success %+v", got)
	}
	if reloaded, _ := LoadFailures(path); reloaded == nil || len(reloaded) != 0 {
		t.Fatalf("file after all succeeded %+v", reloaded)
	}
}

func TestSenderDeliversAsyncAndSkipsDisabled(t *testing.T) {
	srv, got := recorder(t, 200)
	s := newSender(t)
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
	if got := Fold([]wire.NotificationData{one}, wire.LangEn); got != one {
		t.Fatalf("single notification changed: %+v", got)
	}
	three := Fold([]wire.NotificationData{one, one, one}, wire.LangEn)
	if three.Reason != "digest" || three.Count != 3 || three.Title != "3 agents finished their turn" || three.Level != wire.LevelInfo {
		t.Fatalf("uniform digest %+v", three)
	}
	if ja := Fold([]wire.NotificationData{one, one, one}, wire.LangJa); ja.Title != "3 件の agent がターンを終えました" || ja.Reason != "digest" {
		t.Fatalf("ja digest %+v", ja)
	}
	warn := wire.NotificationData{Title: "b", Body: "Exited with code 1", Level: wire.LevelWarn, Reason: "exited"}
	mixed := Fold([]wire.NotificationData{one, warn}, "xx")
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
	d := NewDigester(func(ns []wire.NotificationData) { flushed = append(flushed, Fold(ns, wire.LangEn)) })
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

	// A notification alone in the next window goes out unchanged; one a
	// session settled meanwhile (Drop) does not go out at all.
	settled := n
	settled.SessionID = "00112233aabbccdd"
	d.Add(n, 5*time.Second)
	d.Add(settled, 5*time.Second)
	d.Drop(settled.SessionID)
	if len(ft.fires) != 2 {
		t.Fatal("no new window after the first closed")
	}
	ft.fire(1)
	if len(flushed) != 2 || flushed[1] != n {
		t.Fatalf("single flushed %+v", flushed[1])
	}
	d.Add(settled, 5*time.Second)
	d.Drop(settled.SessionID)
	ft.fire(2)
	if len(flushed) != 2 {
		t.Fatalf("an empty window flushed %+v", flushed[2:])
	}
}
