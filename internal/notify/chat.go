package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Services of a chat destination (notify.chat).
const (
	ServiceDiscord  = "discord"
	ServiceSlack    = "slack"
	ServiceTelegram = "telegram"
)

// ChatDest is where a notify.chat URL sends. endpoint holds the webhook
// token or the bot token: it is a secret and never goes into an error or
// a log.
type ChatDest struct {
	Service  string
	endpoint string // the URL posted to, built from checked parts only
	chatID   string // Telegram
}

// ChatMessage names a message a chat service took, for replacing it later:
// Discord's message id, or Telegram's chat id and message id. Slack's
// Incoming Webhooks name none (zero value).
type ChatMessage struct {
	ChatID string // Telegram
	ID     string
}

var (
	// Parts of a chat URL. Discord's webhook id is a snowflake and its
	// token URL-safe base64; Slack's three parts are alphanumeric;
	// Telegram's bot token is <bot id>:<secret> and the chat a number
	// (negative for groups) or @<channel name>.
	discordIDRe     = regexp.MustCompile(`^[0-9]+$`)
	discordTokenRe  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	slackPartRe     = regexp.MustCompile(`^[A-Za-z0-9]+$`)
	telegramTokenRe = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]+$`)
	telegramChatRe  = regexp.MustCompile(`^(-?[0-9]+|@[A-Za-z0-9_]+)$`)
)

// errChatURL never quotes the URL: it is a secret.
var errChatURL = errors.New("not a Discord webhook (https://discord.com/api/webhooks/<id>/<token>, discord://<id>/<token>), " +
	"Slack Incoming Webhook (https://hooks.slack.com/services/…, slack://<a>/<b>/<c>) " +
	"or Telegram (tgram://<bot_token>/<chat_id>) URL")

// ParseChat reads a notify.chat URL. Its error never contains the URL.
func ParseChat(raw string) (ChatDest, error) {
	switch {
	case strings.HasPrefix(raw, "discord://"):
		if p := strings.Split(strings.TrimPrefix(raw, "discord://"), "/"); len(p) == 2 {
			return discordDest(p[0], p[1])
		}
	case strings.HasPrefix(raw, "slack://"):
		if p := strings.Split(strings.TrimPrefix(raw, "slack://"), "/"); len(p) == 3 {
			return slackDest(p)
		}
	case strings.HasPrefix(raw, "tgram://"):
		// By hand: url.Parse takes "<bot id>:<secret>" for a host and port.
		p := strings.Split(strings.TrimPrefix(raw, "tgram://"), "/")
		if len(p) == 2 && telegramTokenRe.MatchString(p[0]) && telegramChatRe.MatchString(p[1]) {
			return ChatDest{Service: ServiceTelegram, endpoint: "https://api.telegram.org/bot" + p[0] + "/sendMessage", chatID: p[1]}, nil
		}
	case strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			break
		}
		p := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
		switch u.Host {
		case "discord.com", "discordapp.com":
			if len(p) == 4 && p[0] == "api" && p[1] == "webhooks" {
				return discordDest(p[2], p[3])
			}
		case "hooks.slack.com":
			if len(p) > 0 && (p[0] == "triggers" || p[0] == "workflows") {
				return ChatDest{}, errors.New("a Slack Workflow webhook (/triggers/, /workflows/) takes the generic webhook (notify.webhook); notify.chat takes an Incoming Webhook (https://hooks.slack.com/services/…)")
			}
			if len(p) == 4 && p[0] == "services" {
				return slackDest(p[1:])
			}
		}
	}
	return ChatDest{}, errChatURL
}

func discordDest(id, token string) (ChatDest, error) {
	if !discordIDRe.MatchString(id) || !discordTokenRe.MatchString(token) {
		return ChatDest{}, errChatURL
	}
	return ChatDest{Service: ServiceDiscord, endpoint: "https://discord.com/api/webhooks/" + id + "/" + token}, nil
}

func slackDest(p []string) (ChatDest, error) {
	for _, s := range p {
		if !slackPartRe.MatchString(s) {
			return ChatDest{}, errChatURL
		}
	}
	return ChatDest{Service: ServiceSlack, endpoint: "https://hooks.slack.com/services/" + strings.Join(p, "/")}, nil
}

// discordWarnColor is the embed color of a warn notification (yellow).
const discordWarnColor = 0xFEE75C

// pushChat sends n to the chat destination rawURL in its service's format,
// headed by the emoji of n's reason, and names the message it made. When
// link (the open page, PageURL) is not empty, the message links to it
// with label: a link button on Discord and Telegram, a link at the end of
// the text on Slack.
func pushChat(ctx context.Context, client *http.Client, rawURL string, n wire.NotificationData, link, label string) (ChatMessage, error) {
	d, err := ParseChat(rawURL)
	if err != nil {
		return ChatMessage{}, err
	}
	title := n.Title
	if e := chatEmoji(n); e != "" {
		title = e + " " + title
	}
	switch d.Service {
	case ServiceDiscord:
		type embed struct {
			Title       string `json:"title"`
			Description string `json:"description,omitempty"`
			Color       int    `json:"color,omitempty"`
		}
		e := embed{Title: title, Description: n.Body}
		if n.Level == wire.LevelWarn {
			e.Color = discordWarnColor
		}
		var reply struct {
			ID string `json:"id"`
		}
		body := map[string]any{
			"embeds":           []embed{e},
			"allowed_mentions": map[string][]string{"parse": {}},
		}
		// wait=true: Discord answers with the message (its id) once posted.
		endpoint := d.endpoint + "?wait=true"
		if link != "" {
			// A webhook not owned by an application sends link buttons
			// (style 5) only with with_components=true.
			endpoint += "&with_components=true"
			body["components"] = discordLinkRow(link, label)
		}
		err := sendJSON(ctx, client, http.MethodPost, endpoint, body, &reply)
		return ChatMessage{ID: reply.ID}, err
	case ServiceSlack:
		text := "*" + slackEscape(title) + "*"
		if n.Body != "" {
			text += "\n" + slackEscape(n.Body)
		}
		if link != "" {
			// Slack reads &, < and > in a link's URL escaped too.
			text += "\n<" + slackEscape(link) + "|" + slackEscape(label) + ">"
		}
		return ChatMessage{}, sendJSON(ctx, client, http.MethodPost, d.endpoint, map[string]any{"text": text, "unfurl_links": false}, nil)
	default: // ServiceTelegram
		text := title
		if n.Body != "" {
			text += "\n" + n.Body
		}
		var reply struct {
			Result struct {
				MessageID int64 `json:"message_id"`
				Chat      struct {
					ID int64 `json:"id"`
				} `json:"chat"`
			} `json:"result"`
		}
		// No parse_mode: plain text, nothing in it is markup.
		body := map[string]any{
			"chat_id":              d.chatID,
			"text":                 text,
			"link_preview_options": map[string]bool{"is_disabled": true},
		}
		if link != "" {
			body["reply_markup"] = telegramKeyboard(link, label)
		}
		err := sendJSON(ctx, client, http.MethodPost, d.endpoint, body, &reply)
		if err != nil || reply.Result.MessageID == 0 {
			return ChatMessage{}, err
		}
		chatID := d.chatID
		if reply.Result.Chat.ID != 0 {
			chatID = strconv.FormatInt(reply.Result.Chat.ID, 10)
		}
		return ChatMessage{ChatID: chatID, ID: strconv.FormatInt(reply.Result.MessageID, 10)}, nil
	}
}

// slackEscape escapes the characters Slack's mrkdwn reads as markup
// (links, mentions): &, < and >.
var slackEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

// chatEmoji heads a chat message: the emoji of ntfyTags' tag for n's
// reason, "" for none.
func chatEmoji(n wire.NotificationData) string {
	switch n.Reason {
	case "needs_approval":
		return "🔒"
	case "waiting_input":
		return "⏳"
	case "turn_complete":
		return "✅"
	case "exited":
		if n.Level == wire.LevelWarn {
			return "⚠️"
		}
		return "🏁"
	case "digest":
		return "🔔"
	}
	return ""
}

// discordLinkRow is Discord's components: one action row holding a link
// button (style 5) to link.
func discordLinkRow(link, label string) []any {
	return []any{map[string]any{
		"type":       1, // action row
		"components": []any{map[string]any{"type": 2, "style": 5, "label": label, "url": link}},
	}}
}

// telegramKeyboard is Telegram's reply_markup: one url button to link.
func telegramKeyboard(link, label string) map[string]any {
	return map[string]any{"inline_keyboard": [][]map[string]string{{{"text": label, "url": link}}}}
}

// discordResolvedColor is the embed color of a resolved message (grey).
const discordResolvedColor = 0x99AAB5

// resolvedEmoji heads a resolved message in place of its reason's emoji.
const resolvedEmoji = "☑️"

// deleteChat deletes message m at the chat destination rawURL: Discord's
// DELETE /webhooks/<id>/<token>/messages/<id>, Telegram's deleteMessage
// (only within 48 hours of sending). Slack's Incoming Webhooks name no
// message to delete.
func deleteChat(ctx context.Context, client *http.Client, rawURL string, m ChatMessage) error {
	d, err := ParseChat(rawURL)
	if err != nil {
		return err
	}
	switch d.Service {
	case ServiceDiscord:
		return sendJSON(ctx, client, http.MethodDelete, d.discordMessage(m), nil, nil)
	case ServiceTelegram:
		id, err := strconv.ParseInt(m.ID, 10, 64)
		if err != nil {
			return err
		}
		return sendJSON(ctx, client, http.MethodPost, d.telegramMethod("deleteMessage"), map[string]any{"chat_id": m.ChatID, "message_id": id}, nil)
	}
	return errNoChatMessage
}

// resolveChat rewrites message m at the chat destination rawURL as
// resolved: ☑️ in place of the reason's emoji, title (as sent, without
// the emoji) followed by suffix, body unchanged and, when link is not
// empty, the same link button again (Telegram drops one an edit does not
// repeat). Discord: PATCH /webhooks/<id>/<token>/messages/<id> with a grey
// embed; Telegram: editMessageText.
func resolveChat(ctx context.Context, client *http.Client, rawURL string, m ChatMessage, title, body, link, label, suffix string) error {
	d, err := ParseChat(rawURL)
	if err != nil {
		return err
	}
	title = resolvedEmoji + " " + title + suffix
	switch d.Service {
	case ServiceDiscord:
		e := map[string]any{"title": title, "color": discordResolvedColor}
		if body != "" {
			e["description"] = body
		}
		req := map[string]any{
			"embeds":           []any{e},
			"allowed_mentions": map[string][]string{"parse": {}},
		}
		endpoint := d.discordMessage(m)
		if link != "" {
			endpoint += "?with_components=true"
			req["components"] = discordLinkRow(link, label)
		}
		return sendJSON(ctx, client, http.MethodPatch, endpoint, req, nil)
	case ServiceTelegram:
		id, err := strconv.ParseInt(m.ID, 10, 64)
		if err != nil {
			return err
		}
		text := title
		if body != "" {
			text += "\n" + body
		}
		req := map[string]any{
			"chat_id":              m.ChatID,
			"message_id":           id,
			"text":                 text,
			"link_preview_options": map[string]bool{"is_disabled": true},
		}
		if link != "" {
			req["reply_markup"] = telegramKeyboard(link, label)
		}
		return sendJSON(ctx, client, http.MethodPost, d.telegramMethod("editMessageText"), req, nil)
	}
	return errNoChatMessage
}

var errNoChatMessage = errors.New("the chat service names no message")

// discordMessage is the webhook URL of Discord message m.
func (d ChatDest) discordMessage(m ChatMessage) string {
	return d.endpoint + "/messages/" + m.ID
}

// telegramMethod is the Bot API URL of method (d.endpoint is sendMessage's).
func (d ChatDest) telegramMethod(method string) string {
	return strings.TrimSuffix(d.endpoint, "sendMessage") + method
}

// sendJSON sends body (none when nil) as JSON with method and, when reply
// is not nil, decodes the answer into it (an answer it cannot decode
// leaves reply as it is).
func sendJSON(ctx context.Context, client *http.Client, method, endpoint string, body, reply any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	got, err := do(client, req)
	if err == nil && reply != nil {
		json.Unmarshal(got, reply)
	}
	return err
}
