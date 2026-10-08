// Package notify sends notifications to offline push channels: an ntfy
// topic, a generic webhook and/or a chat destination (Discord, Slack,
// Telegram). Sending is asynchronous through a small
// bounded queue; nothing here ever carries terminal output (callers only
// pass wire.NotificationData, whose body is a fixed phrase). Every push is
// titled with the host it comes from, and the last failure of each channel
// is kept (on disk, across restarts) until a push to it succeeds. On ntfy,
// the notifications of one session share a sequence ID: a newer one
// replaces the older, and Clear dismisses it. On Discord and Telegram, a
// newer message of a session replaces the older (sent, then the older
// deleted), and Clear marks it resolved (see chatref.go).
package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Timeout bounds one HTTP request.
const Timeout = 5 * time.Second

// queueSize bounds pending pushes; when full, new pushes are dropped.
const queueSize = 16

// Sender delivers notifications to the enabled channels of a config.
type Sender struct {
	client   *http.Client
	logf     func(format string, args ...any)
	hostname func() (string, error)
	hostID   string // h= of the Click link; "" means no link
	path     string // last failures file
	chatPath string // unresolved chat messages file ("": kept in memory only)

	mu       sync.Mutex
	failures map[string]wire.NotifyFailure // by channel, never nil
	// chatRefs: the unresolved chat messages. Only the worker changes
	// them (and writes chatPath); others read them under mu.
	chatRefs []chatRef
	// chatURL: the fingerprint of the configured chat URL, once
	// Reconfigure has named one (chatURLSet). A chat outcome of another
	// URL (a push queued before the change) is not recorded.
	chatURL    string
	chatURLSet bool
	// noReplace: an ntfy server ignored a sequence ID (logged once).
	noReplace atomic.Bool
	// sleep waits out a 429 before the resend (time.Sleep).
	sleep func(time.Duration)
	// chatLogged is the kind of the chat failure the worker logged last,
	// "" after a success: the same kind again is not logged.
	chatLogged string

	once  sync.Once
	queue chan job
	done  chan struct{}
	wg    sync.WaitGroup
}

// job is a push of n (sessions: a digest's) or, by op, what follows a
// session settling or the config changing.
type job struct {
	cfg      wire.NotifyConfig
	n        wire.NotificationData
	sessions []string // a digest's sessions (n has none); nil otherwise
	op       jobOp
}

type jobOp int

const (
	opPush jobOp = iota
	// opClear: session n.SessionID settled: its ntfy notification is
	// cleared and its chat messages resolved.
	opClear
	// opResolve: its chat messages only (a session gone over a restart).
	opResolve
	// opForget: chat messages sent through another chat URL are forgotten.
	opForget
)

// NewSender returns a Sender keeping the last failure per channel in the
// file failures and the unresolved chat messages in the file
// chatMessages (both loaded now, rewritten when they change; chatMessages
// may be "": in memory only) and linking pushes to hostID (see ClickURL,
// PageURL). logf (may be nil) receives delivery errors.
func NewSender(failures, chatMessages, hostID string, logf func(format string, args ...any)) *Sender {
	if logf == nil {
		logf = log.Printf
	}
	f, err := LoadFailures(failures)
	if err != nil {
		logf("notify: %v", err)
	}
	refs, err := loadChatRefs(chatMessages)
	if err != nil {
		logf("notify: %v", err)
	}
	return &Sender{
		client:   &http.Client{Timeout: Timeout},
		logf:     logf,
		hostname: os.Hostname,
		hostID:   hostID,
		path:     failures,
		chatPath: chatMessages,
		sleep:    time.Sleep,
		failures: f,
		chatRefs: refs,
		queue:    make(chan job, queueSize),
		done:     make(chan struct{}),
	}
}

// Enabled reports whether cfg has any channel to send to.
func Enabled(cfg wire.NotifyConfig) bool {
	return (cfg.Ntfy.Enabled && cfg.Ntfy.Topic != "") || (cfg.Webhook.Enabled && cfg.Webhook.URL != "") ||
		(cfg.Chat.Enabled && cfg.Chat.URL != "")
}

// Only keeps the channels of cfg that channels names (wire.Channel*)
// enabled; none named keeps them all. An unknown name is an error.
func Only(cfg wire.NotifyConfig, channels []string) (wire.NotifyConfig, error) {
	if len(channels) == 0 {
		return cfg, nil
	}
	var ntfy, webhook, chat bool
	for _, c := range channels {
		switch c {
		case wire.ChannelNtfy:
			ntfy = true
		case wire.ChannelWebhook:
			webhook = true
		case wire.ChannelChat:
			chat = true
		default:
			return cfg, fmt.Errorf("unknown channel %q (ntfy, webhook or chat)", c)
		}
	}
	cfg.Ntfy.Enabled = cfg.Ntfy.Enabled && ntfy
	cfg.Webhook.Enabled = cfg.Webhook.Enabled && webhook
	cfg.Chat.Enabled = cfg.Chat.Enabled && chat
	return cfg, nil
}

// Wants reports whether the reasons of cfg (with defaults applied) push n:
// an exited notification is abnormal when its level is warn.
func Wants(cfg wire.NotifyConfig, n wire.NotificationData) bool {
	r := cfg.Reasons
	on := func(b *bool) bool { return b != nil && *b }
	switch n.Reason {
	case "needs_approval":
		return on(r.NeedsApproval)
	case "waiting_input":
		return on(r.WaitingInput)
	case "turn_complete":
		return on(r.TurnComplete)
	case "terminal":
		return on(r.Terminal)
	case "exited":
		return r.Exited == wire.ExitedAll || r.Exited == wire.ExitedError && n.Level == wire.LevelWarn
	}
	return true
}

// Send queues n for delivery to cfg's enabled channels without blocking.
// It reports false when the queue is full (the push is dropped) or the
// sender is closed.
func (s *Sender) Send(cfg wire.NotifyConfig, n wire.NotificationData) bool {
	if !Enabled(cfg) {
		return true
	}
	return s.enqueue(job{cfg: cfg, n: n})
}

// SendDigest queues the notifications of one digest window (see Fold) like
// Send. A chat message of several remembers their sessions: it is marked
// resolved once they have all settled.
func (s *Sender) SendDigest(cfg wire.NotifyConfig, ns []wire.NotificationData) bool {
	if !Enabled(cfg) || len(ns) == 0 {
		return true
	}
	j := job{cfg: cfg, n: Fold(ns, cfg.Lang)}
	if len(ns) > 1 {
		j.sessions = []string{}
		for _, n := range ns {
			if n.SessionID != "" && !slices.Contains(j.sessions, n.SessionID) {
				j.sessions = append(j.sessions, n.SessionID)
			}
		}
	}
	return s.enqueue(j)
}

// Clear queues what follows session sessionID settling, behind the pushes
// queued before it: the dismissal of its ntfy notification (ntfy's clear:
// marked read and removed from the notification drawer) and its chat
// messages marked resolved. The generic webhook gets nothing.
func (s *Sender) Clear(cfg wire.NotifyConfig, sessionID string) bool {
	ntfy := cfg.Ntfy.Enabled && cfg.Ntfy.Topic != ""
	if sessionID == "" || !ntfy && cfg.Chat.URL == "" {
		return true
	}
	return s.enqueue(job{cfg: cfg, n: wire.NotificationData{SessionID: sessionID}, op: opClear})
}

func (s *Sender) enqueue(j job) bool {
	s.once.Do(func() {
		s.wg.Add(1)
		go s.run()
	})
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.queue <- j:
		return true
	default:
		s.logf("notify: queue full, dropping %q", j.n.Title)
		return false
	}
}

// Close stops the worker after the queued pushes (each request bounded by
// Timeout; a chat 429 adds a wait of Timeout at most and one resend).
func (s *Sender) Close() {
	select {
	case <-s.done:
		return
	default:
	}
	close(s.done)
	s.wg.Wait()
}

func (s *Sender) run() {
	defer s.wg.Done()
	for {
		select {
		case j := <-s.queue:
			s.deliver(j)
		case <-s.done:
			for {
				select {
				case j := <-s.queue:
					s.deliver(j)
				default:
					return
				}
			}
		}
	}
}

func (s *Sender) deliver(j job) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	switch j.op {
	case opClear:
		if j.cfg.Ntfy.Enabled && j.cfg.Ntfy.Topic != "" {
			// Not a failed push (last_error): the notification stays, as
			// on a server without clear.
			if err := clearNtfy(ctx, s.client, j.cfg.Ntfy, j.n.SessionID); err != nil {
				s.logf("notify: ntfy clear: %v (the server may predate clear, ntfy 2.16)", plain(err))
			}
		}
		s.resolveChatRefs(j.cfg, j.n.SessionID)
		return
	case opResolve:
		s.resolveChatRefs(j.cfg, j.n.SessionID)
		return
	case opForget:
		s.forgetChatRefs(j.cfg)
		return
	}
	n := labelled(j.n, j.cfg.HostLabel, s.hostname)
	outs, m := s.push(ctx, j.cfg, n, true)
	// Pushes keep going to a chat destination that fails (even one the
	// service disabled); while the same kind of failure repeats, only the
	// first is logged.
	var logged []error
	for _, o := range outs {
		if o.channel == wire.ChannelChat {
			var ce *chatError
			kind := ""
			if errors.As(o.err, &ce) {
				kind = ce.kind
			}
			if o.err != nil && kind != "" && kind == s.chatLogged {
				continue
			}
			s.chatLogged = kind
		}
		if o.err != nil {
			logged = append(logged, fmt.Errorf("%s: %w", o.channel, o.err))
		}
	}
	if err := errors.Join(logged...); err != nil {
		s.logf("notify: %v", err)
	}
	s.keepChatRef(j.cfg, n, j.sessions, m)
}

// outcome is the result of a push to one channel.
type outcome struct {
	channel string
	err     error
}

// Push sends n, titled with the host (labelled), to every enabled channel
// of cfg synchronously and records each channel's outcome (LastErrors).
// notify.test reports its error, which never contains the ntfy topic, the
// webhook URL or the chat URL. A 429 is not resent.
func (s *Sender) Push(ctx context.Context, cfg wire.NotifyConfig, n wire.NotificationData) error {
	outs, _ := s.push(ctx, cfg, labelled(n, cfg.HostLabel, s.hostname), false)
	var errs []error
	for _, o := range outs {
		if o.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", o.channel, o.err))
		}
	}
	return errors.Join(errs...)
}

// push sends n, labelled already, to every enabled channel of cfg and
// records and returns each channel's outcome, and names the message the
// chat destination took (zero when none). With resend, a chat send
// answered 429 with a wait of Timeout at most is sent once more after that
// wait, and only the resend's outcome counts.
func (s *Sender) push(ctx context.Context, cfg wire.NotifyConfig, n wire.NotificationData, resend bool) ([]outcome, ChatMessage) {
	var out []outcome
	push := func(channel string, send func() error) {
		err := plain(send())
		if channel != wire.ChannelChat || s.chatURLCurrent(cfg.Chat.URL) {
			s.record(channel, err)
		}
		out = append(out, outcome{channel, err})
	}
	if cfg.Ntfy.Enabled && cfg.Ntfy.Topic != "" {
		push(wire.ChannelNtfy, func() error {
			replaced, err := pushNtfy(ctx, s.client, cfg.Ntfy, n, ClickURL(cfg.ClickBase, s.hostID, n.SessionID))
			if err == nil && n.SessionID != "" && !replaced && !s.noReplace.Swap(true) {
				s.logf("notify: the ntfy server ignored the sequence ID: notifications of a session are neither replaced nor cleared (ntfy 2.16 does)")
			}
			return err
		})
	}
	if cfg.Webhook.Enabled && cfg.Webhook.URL != "" {
		push(wire.ChannelWebhook, func() error {
			return pushWebhook(ctx, s.client, cfg.Webhook, n, PageURL(cfg.ClickPage, s.hostID, n.SessionID))
		})
	}
	var m ChatMessage
	if cfg.Chat.Enabled && cfg.Chat.URL != "" {
		link := s.chatLink(cfg, n)
		send := func(ctx context.Context) error {
			var err error
			m, err = pushChat(ctx, s.client, cfg.Chat.URL, n, link, Text(cfg.Lang, PhraseOpen))
			return chatFailure(cfg.Chat.URL, plain(err))
		}
		push(wire.ChannelChat, func() error {
			err := send(ctx)
			if wait, ok := resendable(err); resend && ok {
				s.sleep(wait)
				// The first send may have used up ctx: the resend gets
				// a Timeout of its own.
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), Timeout)
				defer cancel()
				err = send(rctx)
			}
			return err
		})
	}
	return out, m
}

// labelled prefixes n's title with the host: hostLabel, else the host name
// (no prefix when that fails too). A digest reads "<host>: <title>",
// anything else "<host> · <title>".
func labelled(n wire.NotificationData, hostLabel string, hostname func() (string, error)) wire.NotificationData {
	label := strings.TrimSpace(hostLabel)
	if label == "" {
		if h, err := hostname(); err == nil {
			label = h
		}
	}
	if label == "" {
		return n
	}
	sep := " · "
	if n.Reason == "digest" {
		sep = ": "
	}
	n.Title = label + sep + n.Title
	return n
}

// ClickURL is the link an ntfy push of sessionID (may be "") opens (its
// Click): clickBase
// with h=<hostID> and, for a session, s=<sessionID> added to its query.
// It is "" — no link — when clickBase or hostID is empty or clickBase is
// not an absolute URL. Nothing else goes into the link.
func ClickURL(clickBase, hostID, sessionID string) string {
	if clickBase == "" || hostID == "" {
		return ""
	}
	u, err := url.Parse(clickBase)
	if err != nil || u.Scheme == "" {
		return ""
	}
	q := u.Query()
	q.Set("h", hostID)
	q.Del("s")
	if sessionID != "" {
		q.Set("s", sessionID)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// ValidClickPage reports whether clickPage is a notify.click_page: an
// absolute https URL with neither a query nor a fragment.
func ValidClickPage(clickPage string) bool {
	_, ok := parseClickPage(clickPage)
	return ok
}

func parseClickPage(clickPage string) (*url.URL, bool) {
	if strings.ContainsAny(clickPage, "?#") {
		return nil, false
	}
	u, err := url.Parse(clickPage)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, false
	}
	return u, true
}

// PageURL is the link to the open page clickPage for a push of sessionID
// (may be ""): h=<hostID> and, for a session, s=<sessionID> in its
// fragment (`<click_page>#h=…&s=…`), so they never reach the page's
// server. It is "" — no link — when hostID is empty or clickPage is not
// a notify.click_page (ValidClickPage; empty included).
func PageURL(clickPage, hostID, sessionID string) string {
	u, ok := parseClickPage(clickPage)
	if !ok || hostID == "" {
		return ""
	}
	v := url.Values{"h": {hostID}}
	if sessionID != "" {
		v.Set("s", sessionID)
	}
	return u.String() + "#" + v.Encode()
}

// LastErrors returns the last failed push per channel (never nil).
func (s *Sender) LastErrors() map[string]wire.NotifyFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.failures)
}

// record remembers a failed push to channel, or forgets the channel's last
// failure after a success, and saves the failures when they changed.
func (s *Sender) record(channel string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		if _, ok := s.failures[channel]; !ok {
			return
		}
		delete(s.failures, channel)
	} else {
		f := wire.NotifyFailure{At: time.Now().UnixMilli(), Error: err.Error()}
		var se *statusError
		if errors.As(err, &se) {
			f.Status = se.code
		}
		var ce *chatError
		if errors.As(err, &ce) {
			f.Kind = ce.kind
		}
		s.failures[channel] = f
	}
	b, err := json.Marshal(s.failures)
	if err == nil {
		err = writeFileAtomic(s.path, b)
	}
	if err != nil {
		s.logf("notify: last errors: %v", err)
	}
}

// chatURLCurrent reports whether rawURL is the configured chat URL (any is
// before Reconfigure names one).
func (s *Sender) chatURLCurrent(rawURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.chatURLSet || s.chatURL == chatFingerprint(rawURL)
}

// LoadFailures reads a last failures file (Sender). A missing file is no
// failure; the map is never nil, even with an error.
func LoadFailures(path string) (map[string]wire.NotifyFailure, error) {
	f := map[string]wire.NotifyFailure{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return map[string]wire.NotifyFailure{}, fmt.Errorf("%s: %w", path, err)
	}
	if f == nil {
		f = map[string]wire.NotifyFailure{}
	}
	return f, nil
}

// writeFileAtomic writes b to path via a temporary file and rename, 0600.
func writeFileAtomic(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, 0o600)); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SequenceID is the ntfy sequence ID of session sessionID's
// notifications: the id itself when ntfy accepts it (1–64 of
// [A-Za-z0-9_-]; stagent's 16 hex characters always are), else a hash of
// it that is.
func SequenceID(sessionID string) string {
	ok := len(sessionID) >= 1 && len(sessionID) <= 64
	for i := 0; ok && i < len(sessionID); i++ {
		c := sessionID[i]
		ok = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
	}
	if ok {
		return sessionID
	}
	sum := sha256.Sum256([]byte(sessionID))
	return hex.EncodeToString(sum[:16])
}

func ntfyTopicURL(c wire.NtfyConfig) string {
	server := c.Server
	if server == "" {
		server = "https://ntfy.sh"
	}
	return strings.TrimRight(server, "/") + "/" + strings.TrimLeft(c.Topic, "/")
}

// pushNtfy publishes n to the ntfy topic. A notification of one session
// carries its SequenceID (ntfy replaces the previous notification of the
// session); click, when not empty, is opened on tap. replaced reports that
// the server took the sequence ID (its reply names it).
func pushNtfy(ctx context.Context, client *http.Client, c wire.NtfyConfig, n wire.NotificationData, click string) (replaced bool, err error) {
	body := n.Body
	if body == "" {
		body = n.Title
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ntfyTopicURL(c), strings.NewReader(body))
	if err != nil {
		return false, err
	}
	// Header values must be ASCII; ntfy decodes RFC 2047 encoded words.
	req.Header.Set("Title", mime.BEncoding.Encode("UTF-8", n.Title))
	req.Header.Set("Priority", ntfyPriority(n))
	req.Header.Set("Tags", ntfyTags(n))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	seq := ""
	if n.SessionID != "" {
		seq = SequenceID(n.SessionID)
		req.Header.Set("X-Sequence-ID", seq)
	}
	if click != "" {
		req.Header.Set("Click", click)
	}
	reply, err := do(client, req)
	if err != nil || seq == "" {
		return false, err
	}
	var r struct {
		SequenceID string `json:"sequence_id"`
	}
	json.Unmarshal(reply, &r)
	return r.SequenceID == seq, nil
}

// clearNtfy dismisses the notification of session sessionID (PUT
// <topic>/<sequence id>/clear).
func clearNtfy(ctx context.Context, client *http.Client, c wire.NtfyConfig, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, ntfyTopicURL(c)+"/"+SequenceID(sessionID)+"/clear", nil)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	_, err = do(client, req)
	return err
}

func ntfyPriority(n wire.NotificationData) string {
	switch {
	case n.Level == wire.LevelWarn || n.Reason == "needs_approval":
		return "high"
	default:
		return "default"
	}
}

func ntfyTags(n wire.NotificationData) string {
	tags := []string{"stagent"}
	switch n.Reason {
	case "needs_approval":
		tags = append(tags, "lock")
	case "waiting_input":
		tags = append(tags, "hourglass")
	case "turn_complete":
		tags = append(tags, "white_check_mark")
	case "exited":
		if n.Level == wire.LevelWarn {
			tags = append(tags, "warning")
		} else {
			tags = append(tags, "checkered_flag")
		}
	case "digest":
		tags = append(tags, "bell")
	}
	return strings.Join(tags, ",")
}

// pushWebhook posts n as JSON with click_url (the open page, PageURL)
// added when click is not empty.
func pushWebhook(ctx context.Context, client *http.Client, c wire.WebhookConfig, n wire.NotificationData, click string) error {
	b, err := json.Marshal(struct {
		wire.NotificationData
		ClickURL string `json:"click_url,omitempty"`
	}{n, click})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.Headers {
		req.Header.Set(k, v)
	}
	_, err = do(client, req)
	return err
}

// statusError is a response other than 2xx. Its text is the status line
// only — the request URL (ntfy topic, webhook URL, chat URL) is a secret —
// followed, for the chat destination, by the service's reason
// (chatFailure).
type statusError struct {
	code       int
	status     string // e.g. "429 Too Many Requests"
	reason     string // chatFailure's, "" for none
	body       []byte // the start of the reply, never shown as it is
	retryAfter string // the Retry-After header
}

func (e *statusError) Error() string {
	if e.reason == "" {
		return e.status
	}
	return e.status + ": " + e.reason
}

// plain drops the request URL from a net/http error (*url.Error), keeping
// the cause: the URL holds the ntfy topic, the webhook URL or the chat
// URL (a Telegram bot token), all secrets.
func plain(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// do sends req and returns the start of the reply body (64 KiB at most).
func do(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		status := resp.Status
		if status == "" {
			status = fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
		}
		return nil, &statusError{code: resp.StatusCode, status: status, body: body, retryAfter: resp.Header.Get("Retry-After")}
	}
	return body, nil
}
