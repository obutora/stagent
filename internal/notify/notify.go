// Package notify sends notifications to offline push channels: an ntfy
// topic and/or a generic webhook. Sending is asynchronous through a small
// bounded queue; nothing here ever carries terminal output (callers only
// pass wire.NotificationData, whose body is a fixed phrase).
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Timeout bounds one HTTP request.
const Timeout = 5 * time.Second

// queueSize bounds pending pushes; when full, new pushes are dropped.
const queueSize = 16

// Sender delivers notifications to the enabled channels of a config.
type Sender struct {
	client *http.Client
	logf   func(format string, args ...any)

	once  sync.Once
	queue chan job
	done  chan struct{}
	wg    sync.WaitGroup
}

type job struct {
	cfg wire.NotifyConfig
	n   wire.NotificationData
}

// NewSender returns a Sender. logf (may be nil) receives delivery errors.
func NewSender(logf func(format string, args ...any)) *Sender {
	if logf == nil {
		logf = log.Printf
	}
	return &Sender{
		client: &http.Client{Timeout: Timeout},
		logf:   logf,
		queue:  make(chan job, queueSize),
		done:   make(chan struct{}),
	}
}

// Enabled reports whether cfg has any channel to send to.
func Enabled(cfg wire.NotifyConfig) bool {
	return (cfg.Ntfy.Enabled && cfg.Ntfy.Topic != "") || (cfg.Webhook.Enabled && cfg.Webhook.URL != "")
}

// Send queues n for delivery to cfg's enabled channels without blocking.
// It reports false when the queue is full (the push is dropped) or the
// sender is closed.
func (s *Sender) Send(cfg wire.NotifyConfig, n wire.NotificationData) bool {
	if !Enabled(cfg) {
		return true
	}
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
	case s.queue <- job{cfg, n}:
		return true
	default:
		s.logf("notify: queue full, dropping %q", n.Title)
		return false
	}
}

// Close stops the worker after the queued pushes (each bounded by Timeout).
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
	if err := Push(ctx, s.client, j.cfg, j.n); err != nil {
		s.logf("notify: %v", err)
	}
}

// Push sends n to cfg's enabled channels synchronously with the sender's
// HTTP client (notify.test reports its outcome).
func (s *Sender) Push(ctx context.Context, cfg wire.NotifyConfig, n wire.NotificationData) error {
	return Push(ctx, s.client, cfg, n)
}

// Push sends n to every enabled channel of cfg synchronously.
func Push(ctx context.Context, client *http.Client, cfg wire.NotifyConfig, n wire.NotificationData) error {
	var errs []error
	if cfg.Ntfy.Enabled && cfg.Ntfy.Topic != "" {
		if err := pushNtfy(ctx, client, cfg.Ntfy, n); err != nil {
			errs = append(errs, fmt.Errorf("ntfy: %w", err))
		}
	}
	if cfg.Webhook.Enabled && cfg.Webhook.URL != "" {
		if err := pushWebhook(ctx, client, cfg.Webhook, n); err != nil {
			errs = append(errs, fmt.Errorf("webhook: %w", err))
		}
	}
	return errors.Join(errs...)
}

func pushNtfy(ctx context.Context, client *http.Client, c wire.NtfyConfig, n wire.NotificationData) error {
	server := c.Server
	if server == "" {
		server = "https://ntfy.sh"
	}
	url := strings.TrimRight(server, "/") + "/" + strings.TrimLeft(c.Topic, "/")
	body := n.Body
	if body == "" {
		body = n.Title
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	// Header values must be ASCII; ntfy decodes RFC 2047 encoded words.
	req.Header.Set("Title", mime.BEncoding.Encode("UTF-8", n.Title))
	req.Header.Set("Priority", ntfyPriority(n))
	req.Header.Set("Tags", ntfyTags(n))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return do(client, req)
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

func pushWebhook(ctx context.Context, client *http.Client, c wire.WebhookConfig, n wire.NotificationData) error {
	b, err := json.Marshal(n)
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
	return do(client, req)
}

func do(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s", req.Method, req.URL.Redacted(), resp.Status)
	}
	return nil
}
