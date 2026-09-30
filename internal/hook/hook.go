// Package hook is `stagent hook <harness> [event] [json]`, the command
// harness hooks run. It forwards the hook payload to the daemon and, for
// blocking permission requests, prints the decision the app made. It never
// breaks a harness: every path exits 0, the daemon is never started from
// here, and waiting is bounded.
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// maxPayload bounds the hook payload read from stdin.
	maxPayload = 1 << 20
	// stdinWait bounds reading stdin; harnesses write the payload at once.
	stdinWait = 2 * time.Second
	// dialTimeout: an absent daemon must not slow the harness down.
	dialTimeout = 500 * time.Millisecond
	// callTimeout bounds non-blocking events.
	callTimeout = 3 * time.Second
	// approvalMargin is added to the approval timeout for the blocking
	// wait; the installer gives the harness hook approval timeout + 10s.
	approvalMargin = 5 * time.Second

	eventPermissionRequest = "PermissionRequest"
	codexNotify            = "notify"
)

// procIO is the process environment, replaceable in tests.
type procIO struct {
	stdin     io.Reader
	stdinTTY  bool
	stdout    io.Writer
	sessionID string
}

// Main runs the hook command. It always returns 0.
func Main(args []string) int {
	return run(args, procIO{
		stdin:     os.Stdin,
		stdinTTY:  term.IsTerminal(int(os.Stdin.Fd())),
		stdout:    os.Stdout,
		sessionID: os.Getenv(wire.EnvSessionID),
	})
}

func run(args []string, pio procIO) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if len(args) == 0 {
		return 0
	}
	harness, rest := args[0], args[1:]
	event := ""
	var payload []byte
	switch {
	case harness == wire.HarnessCodex && len(rest) > 0 && rest[0] == codexNotify:
		// Codex's notify program gets the JSON as its last argument.
		event = codexNotify
		if len(rest) > 1 {
			payload = []byte(rest[len(rest)-1])
		}
	default:
		if len(rest) > 0 {
			event = rest[0]
		}
		if len(rest) > 1 && json.Valid([]byte(rest[len(rest)-1])) {
			payload = []byte(rest[len(rest)-1])
		} else if !pio.stdinTTY && pio.stdin != nil {
			payload = readPayload(pio.stdin)
		}
	}
	payload = normalizePayload(payload)
	if event == "" {
		event = eventName(payload)
	}

	l, err := paths.Resolve()
	if err != nil {
		return 0
	}
	conn, err := daemonclient.Dial(l, dialTimeout)
	if err != nil {
		return 0
	}
	client := rpc.NewClient(conn, nil)
	defer client.Close()

	blocking := event == eventPermissionRequest
	timeout := callTimeout
	if blocking {
		timeout = approvalTimeout(l) + approvalMargin
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var res wire.HookEventResult
	err = client.Call(ctx, wire.MethodHookEvent, wire.HookEventParams{
		Harness:   harness,
		Event:     event,
		SessionID: pio.sessionID,
		Payload:   payload,
	}, &res)
	if err != nil || !blocking {
		return 0
	}
	if out := permissionOutput(harness, res); out != nil {
		pio.stdout.Write(out)
	}
	return 0
}

// permissionDecision is hookSpecificOutput.decision of a PermissionRequest
// hook, the same shape for Claude Code and Codex.
type permissionDecision struct {
	Behavior string `json:"behavior"` // allow | deny
	Message  string `json:"message,omitempty"`
}

// permissionOutput renders the app's decision for the harness, or nil when
// there is none (the harness then asks in the terminal as usual).
//
// Claude Code and Codex both read
// {"hookSpecificOutput":{"hookEventName":"PermissionRequest",
// "decision":{"behavior":"allow"|"deny","message":"..."}}}; Codex rejects
// unknown fields (updatedInput, updatedPermissions, interrupt fail closed),
// so only behavior and, for deny, message are sent.
func permissionOutput(harness string, res wire.HookEventResult) []byte {
	if harness != wire.HarnessClaude && harness != wire.HarnessCodex {
		return nil
	}
	if res.Decision != wire.DecisionAllow && res.Decision != wire.DecisionDeny {
		return nil
	}
	dec := permissionDecision{Behavior: res.Decision}
	if res.Decision == wire.DecisionDeny {
		dec.Message = res.Message
	}
	b, err := json.Marshal(map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName": eventPermissionRequest,
			"decision":      dec,
		},
	})
	if err != nil {
		return nil
	}
	return append(b, '\n')
}

// approvalTimeout reads approval_timeout_sec from config.json (default 60s).
func approvalTimeout(l *paths.Layout) time.Duration {
	var c wire.Config
	if b, err := os.ReadFile(l.Config); err == nil {
		_ = json.Unmarshal(b, &c) // unreadable config: defaults
	}
	return time.Duration(c.WithDefaults().ApprovalTimeoutSec) * time.Second
}

// lockedBuf collects what the stdin reader saw, up to maxPayload+1 bytes.
type lockedBuf struct {
	mu sync.Mutex
	b  []byte
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := maxPayload + 1 - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (l *lockedBuf) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b...)
}

// readPayload reads one JSON value from r, returning as soon as it is
// complete (stdin need not be closed) or after stdinWait with whatever
// arrived. Oversized or truncated payloads come back as they are and are
// reduced by normalizePayload.
func readPayload(r io.Reader) []byte {
	var seen lockedBuf
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var raw json.RawMessage
		err := json.NewDecoder(io.TeeReader(io.LimitReader(r, maxPayload+1), &seen)).Decode(&raw)
		ch <- result{raw, err}
	}()
	select {
	case res := <-ch:
		if res.err == nil {
			return res.raw
		}
	case <-time.After(stdinWait):
	}
	return seen.bytes()
}

// Fields kept from a payload too large (or too broken) to forward whole.
var salvageFields = []string{
	"hook_event_name", "session_id", "transcript_path", "cwd", "tool_name",
	"notification_type", "permission_mode", "type", "thread-id",
}

var salvageInputFields = []string{"command", "file_path", "path", "url", "pattern", "description"}

// normalizePayload returns payload when it is valid JSON of at most
// maxPayload bytes; otherwise a small object with the fields stagent needs,
// scraped from the bytes that were read (nil when nothing is left).
func normalizePayload(payload []byte) []byte {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	if len(payload) <= maxPayload && json.Valid(payload) {
		return payload
	}
	obj := map[string]any{}
	for _, f := range salvageFields {
		if v, ok := scrapeString(payload, f); ok {
			obj[f] = v
		}
	}
	in := map[string]string{}
	for _, f := range salvageInputFields {
		if v, ok := scrapeString(payload, f); ok {
			in[f] = v
		}
	}
	if len(in) > 0 {
		obj["tool_input"] = in
	}
	if len(obj) == 0 {
		return nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return b
}

// maxScraped bounds a scraped string value.
const maxScraped = 4096

// scrapeString finds the first "field": "value" pair in raw (possibly
// truncated) JSON text.
func scrapeString(raw []byte, field string) (string, bool) {
	key := []byte(`"` + field + `"`)
	for from := 0; ; {
		i := bytes.Index(raw[from:], key)
		if i < 0 {
			return "", false
		}
		j := skipSpace(raw, from+i+len(key))
		from += i + len(key)
		if j >= len(raw) || raw[j] != ':' {
			continue // the name appeared as a value
		}
		j = skipSpace(raw, j+1)
		if j >= len(raw) || raw[j] != '"' {
			return "", false
		}
		for k := j + 1; k < len(raw) && k-j <= maxScraped; k++ {
			switch raw[k] {
			case '\\':
				k++
			case '"':
				var s string
				if json.Unmarshal(raw[j:k+1], &s) != nil {
					return "", false
				}
				return s, true
			}
		}
		return "", false
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// eventName reads hook_event_name from a payload.
func eventName(payload []byte) string {
	var p struct {
		Name string `json:"hook_event_name"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Name
}
