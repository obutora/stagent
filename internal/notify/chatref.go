package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// On Discord and Telegram, stagent keeps the unresolved messages the chat
// destination took (chatRef, in the chat messages file): the next message
// of a session replaces the session's older one (sent first, then the
// older deleted) and a session settling (Clear) marks its messages
// resolved — edited, not deleted. A digest's message lists its sessions
// and is marked resolved once they have all settled; a later message of
// one of them leaves it alone. Slack names no message: it only gets
// pushes. Nothing here retries or counts as a failed push (last_error);
// a message that cannot be deleted or edited is forgotten (released),
// with one log line unless the service no longer has it (404).

// chatRef is an unresolved message the chat destination took. It never
// holds the chat URL: URLSHA256 is the SHA-256 of notify.chat.url it was
// sent through, and a message of another URL is forgotten, never edited.
type chatRef struct {
	// Sessions: the session of the message or, for a digest, those of
	// its sessions that have not settled yet.
	Sessions  []string `json:"sessions"`
	Digest    bool     `json:"digest,omitempty"`
	ChatID    string   `json:"chat_id,omitempty"` // Telegram
	MessageID string   `json:"message_id"`
	SentAt    int64    `json:"sent_at"` // unix ms
	URLSHA256 string   `json:"url_sha256"`
	// What marking it resolved writes again: the title as sent without
	// the reason's emoji (host-labelled), the body and the link button.
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	Link  string `json:"link,omitempty"`
	Label string `json:"label,omitempty"`
}

func (r chatRef) message() ChatMessage { return ChatMessage{ChatID: r.ChatID, ID: r.MessageID} }

// chatFingerprint is the hex SHA-256 of a chat URL; "" for none.
func chatFingerprint(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rawURL))
	return hex.EncodeToString(sum[:])
}

// loadChatRefs reads a chat messages file. A missing file (or path "")
// holds none.
func loadChatRefs(path string) ([]chatRef, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refs []chatRef
	if err := json.Unmarshal(b, &refs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return refs, nil
}

// ChatSessions returns the sessions with an unresolved chat message sent
// through cfg's chat URL (from before a restart, for the daemon to take
// back).
func (s *Sender) ChatSessions(cfg wire.NotifyConfig) []string {
	fp := chatFingerprint(cfg.Chat.URL)
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, r := range s.chatRefs {
		if r.URLSHA256 != fp {
			continue
		}
		for _, id := range r.Sessions {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// ResolveChat queues marking session sessionID's chat messages resolved,
// as Clear does, without touching ntfy: the session is gone (it did not
// survive a restart).
func (s *Sender) ResolveChat(cfg wire.NotifyConfig, sessionID string) bool {
	if sessionID == "" || cfg.Chat.URL == "" {
		return true
	}
	return s.enqueue(job{cfg: cfg, n: wire.NotificationData{SessionID: sessionID}, op: opResolve})
}

// Reconfigure takes notify config cfg. A chat URL other than the one
// configured before (changed, or none now) forgets the chat's last failure
// at once, and the outcomes of pushes still queued for the old URL are not
// recorded: a failure of the old URL says nothing about the new one. The
// first config and disabling the chat keep it. Then, behind the pushes
// queued before it, the chat messages sent through another chat URL are
// forgotten, never edited through the old URL.
func (s *Sender) Reconfigure(cfg wire.NotifyConfig) bool {
	fp := chatFingerprint(cfg.Chat.URL)
	s.mu.Lock()
	changed := s.chatURLSet && s.chatURL != fp
	s.chatURL, s.chatURLSet = fp, true
	s.mu.Unlock()
	if changed {
		s.record(wire.ChannelChat, nil)
	}
	return s.enqueue(job{cfg: cfg, op: opForget})
}

// chatLink is the link of a chat message of n: only a push of one session
// gets one (a digest and notify.test have no session).
func (s *Sender) chatLink(cfg wire.NotifyConfig, n wire.NotificationData) string {
	if n.SessionID == "" {
		return ""
	}
	return PageURL(cfg.ClickPage, s.hostID, n.SessionID)
}

// keepChatRef follows a push of n (labelled; sessions: a digest's) that
// made chat message m (zero: none): m is kept, then the older messages of
// its sessions are deleted (a digest's are not). Worker only.
func (s *Sender) keepChatRef(cfg wire.NotifyConfig, n wire.NotificationData, sessions []string, m ChatMessage) {
	digest := sessions != nil
	if !digest && n.SessionID != "" {
		sessions = []string{n.SessionID}
	}
	fp := chatFingerprint(cfg.Chat.URL)
	s.mu.Lock()
	changed := s.forgetLocked(fp)
	var older []chatRef
	if m.ID != "" && len(sessions) > 0 {
		for _, r := range s.chatRefs {
			if !r.Digest && len(r.Sessions) == 1 && slices.Contains(sessions, r.Sessions[0]) {
				older = append(older, r)
			}
		}
		r := chatRef{
			Sessions: sessions, Digest: digest, ChatID: m.ChatID, MessageID: m.ID,
			SentAt: time.Now().UnixMilli(), URLSHA256: fp, Title: n.Title, Body: n.Body,
			Link: s.chatLink(cfg, n),
		}
		if r.Link != "" {
			r.Label = Text(cfg.Lang, PhraseOpen)
		}
		s.chatRefs = append(s.chatRefs, r)
		changed = true
	}
	s.mu.Unlock()
	if changed {
		s.saveChatRefs()
	}
	if len(older) == 0 {
		return
	}
	for _, r := range older {
		s.replaceChat(cfg, r)
	}
	s.mu.Lock()
	s.chatRefs = slices.DeleteFunc(s.chatRefs, func(r chatRef) bool {
		return slices.ContainsFunc(older, func(o chatRef) bool { return o.ChatID == r.ChatID && o.MessageID == r.MessageID })
	})
	s.mu.Unlock()
	s.saveChatRefs()
}

// resolveChatRefs follows session sessionID settling: its message, and a
// digest's whose sessions have all settled, are marked resolved and
// forgotten. Worker only.
func (s *Sender) resolveChatRefs(cfg wire.NotifyConfig, sessionID string) {
	fp := chatFingerprint(cfg.Chat.URL)
	s.mu.Lock()
	changed := s.forgetLocked(fp)
	var done []chatRef
	kept := s.chatRefs[:0]
	for _, r := range s.chatRefs {
		if i := slices.Index(r.Sessions, sessionID); i >= 0 {
			r.Sessions = slices.Delete(slices.Clone(r.Sessions), i, i+1)
			if len(r.Sessions) == 0 {
				done = append(done, r)
				continue
			}
		}
		kept = append(kept, r)
	}
	s.chatRefs = kept
	s.mu.Unlock()
	for _, r := range done {
		s.markResolved(cfg, r)
	}
	if changed || len(done) > 0 {
		s.saveChatRefs()
	}
}

// forgetChatRefs forgets the messages sent through another chat URL than
// cfg's. Worker only.
func (s *Sender) forgetChatRefs(cfg wire.NotifyConfig) {
	s.mu.Lock()
	changed := s.forgetLocked(chatFingerprint(cfg.Chat.URL))
	s.mu.Unlock()
	if changed {
		s.saveChatRefs()
	}
}

// forgetLocked drops the messages not sent through the chat URL of
// fingerprint fp and reports whether there were any.
func (s *Sender) forgetLocked(fp string) bool {
	n := len(s.chatRefs)
	s.chatRefs = slices.DeleteFunc(s.chatRefs, func(r chatRef) bool { return r.URLSHA256 != fp })
	return len(s.chatRefs) != n
}

// replaceChat deletes r, replaced by a newer message of its session. One
// the service does not delete (Telegram's after 48 hours, another
// refusal) is marked resolved instead; a 429 or a connection error is
// not tried further.
func (s *Sender) replaceChat(cfg wire.NotifyConfig, r chatRef) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	err := deleteChat(ctx, s.client, cfg.Chat.URL, r.message())
	cancel()
	switch {
	case err == nil || chatStatus(err) == http.StatusNotFound:
		return
	case chatStatus(err) == http.StatusTooManyRequests || chatStatus(err) == 0:
		s.logf("notify: chat: a replaced message was not deleted: %v", plain(err))
		return
	}
	s.markResolved(cfg, r)
}

// markResolved edits r to resolved, logging a failure (but a 404: the
// message, the webhook or the bot's chat is gone).
func (s *Sender) markResolved(cfg wire.NotifyConfig, r chatRef) {
	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	err := resolveChat(ctx, s.client, cfg.Chat.URL, r.message(), r.Title, r.Body, r.Link, r.Label, Text(cfg.Lang, PhraseResolved))
	if err != nil && chatStatus(err) != http.StatusNotFound {
		s.logf("notify: chat: a message was not marked resolved: %v", plain(err))
	}
}

// chatStatus is the HTTP status of a failed request, 0 for none (a
// connection error).
func chatStatus(err error) int {
	var se *statusError
	if errors.As(err, &se) {
		return se.code
	}
	return 0
}

// saveChatRefs writes the chat messages file. Worker only.
func (s *Sender) saveChatRefs() {
	if s.chatPath == "" {
		return
	}
	s.mu.Lock()
	refs := s.chatRefs
	if refs == nil {
		refs = []chatRef{}
	}
	b, err := json.Marshal(refs)
	s.mu.Unlock()
	if err == nil {
		err = writeFileAtomic(s.chatPath, b)
	}
	if err != nil {
		s.logf("notify: chat messages: %v", err)
	}
}
