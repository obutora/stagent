// Package transcript reads the conversation files coding agents write —
// Claude Code, Codex and omp JSONL — and normalizes them to wire.Message.
// Files are only ever read: backwards from the end for paging, forwards from
// a remembered offset for tailing, never whole unless a cache entry is new.
package transcript

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/wire"
)

// Roles of wire.Message.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

const (
	// maxText bounds one message's text; the app shows conversations, not
	// file dumps.
	maxText = 32 << 10
	// maxSummary bounds a tool call's one-line summary.
	maxSummary = 200
)

// ParseLine normalizes one JSONL record of harness to zero or more messages
// (in order). Unknown or noise records yield none.
func ParseLine(harness string, line []byte) []wire.Message {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	switch harness {
	case wire.HarnessClaude:
		return parseClaude(line)
	case wire.HarnessCodex:
		return parseCodex(line)
	case wire.HarnessOmp:
		return parseOmp(line)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Claude Code: ~/.claude/projects/<project>/<uuid>.jsonl

type claudeRecord struct {
	Type             string `json:"type"`
	IsMeta           bool   `json:"isMeta"`
	IsSidechain      bool   `json:"isSidechain"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	Timestamp        string `json:"timestamp"`
	Origin           *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudePart struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Tags whose user records are harness plumbing, not something the user said.
var claudeNoiseTags = []string{
	"<local-command-caveat>", "<local-command-stdout>", "<local-command-stderr>",
	"<task-notification>", "<system-reminder>", "<bash-stdout>", "<bash-stderr>",
	"<user-memory-input>", "<post-tool-use-hook>",
}

var (
	claudeCommandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	claudeCommandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	claudeBashInput   = regexp.MustCompile(`(?s)^<bash-input>(.*?)</bash-input>`)
)

func parseClaude(line []byte) []wire.Message {
	// Cheap reject of the record types that never carry messages.
	if !bytes.Contains(line, []byte(`"type":"user"`)) && !bytes.Contains(line, []byte(`"type":"assistant"`)) {
		return nil
	}
	var r claudeRecord
	if json.Unmarshal(line, &r) != nil || r.IsSidechain {
		return nil
	}
	t := parseTime(r.Timestamp)
	switch r.Type {
	case "user":
		if r.IsCompactSummary {
			return []wire.Message{{Role: RoleSystem, Text: "Conversation compacted", Time: t}}
		}
		if r.IsMeta || (r.Origin != nil && r.Origin.Kind != "" && r.Origin.Kind != "human") {
			return nil
		}
		var texts []string
		if s, ok := jsonString(r.Message.Content); ok {
			texts = []string{s}
		} else {
			var parts []claudePart
			if json.Unmarshal(r.Message.Content, &parts) != nil {
				return nil
			}
			for _, p := range parts {
				switch p.Type {
				case "text":
					texts = append(texts, p.Text)
				case "image":
					texts = append(texts, "[image]")
				}
			}
		}
		var out []wire.Message
		for _, s := range texts {
			if m, ok := claudeUserText(s); ok {
				m.Time = t
				out = append(out, m)
			}
		}
		return out
	case "assistant":
		if r.Message.Model == "<synthetic>" {
			return nil
		}
		var parts []claudePart
		if json.Unmarshal(r.Message.Content, &parts) != nil {
			return nil
		}
		var out []wire.Message
		for _, p := range parts {
			switch p.Type {
			case "text":
				if s := strings.TrimSpace(p.Text); s != "" {
					out = append(out, wire.Message{Role: RoleAssistant, Text: clip(s, maxText), Time: t})
				}
			case "tool_use":
				out = append(out, wire.Message{Role: RoleTool, ToolName: p.Name, Text: Summarize(p.Input), Time: t})
			}
		}
		return out
	}
	return nil
}

// claudeUserText classifies the text of a user record.
func claudeUserText(s string) (wire.Message, bool) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return wire.Message{}, false
	}
	if strings.HasPrefix(trimmed, "[Request interrupted by user") {
		return wire.Message{Role: RoleSystem, Text: "Interrupted by user"}, true
	}
	if strings.HasPrefix(trimmed, "<command-name>") || strings.HasPrefix(trimmed, "<command-message>") {
		m := claudeCommandName.FindStringSubmatch(trimmed)
		if m == nil {
			return wire.Message{}, false
		}
		text := strings.TrimSpace(m[1])
		if a := claudeCommandArgs.FindStringSubmatch(trimmed); a != nil {
			if args := strings.TrimSpace(a[1]); args != "" {
				text += " " + args
			}
		}
		return wire.Message{Role: RoleUser, Text: clip(text, maxText)}, true
	}
	if m := claudeBashInput.FindStringSubmatch(trimmed); m != nil {
		return wire.Message{Role: RoleUser, Text: clip("!"+m[1], maxText)}, true
	}
	for _, tag := range claudeNoiseTags {
		if strings.HasPrefix(trimmed, tag) {
			return wire.Message{}, false
		}
	}
	return wire.Message{Role: RoleUser, Text: clip(trimmed, maxText)}, true
}

// ---------------------------------------------------------------------------
// Codex: ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl
//
// Only response_item records are read; event_msg records duplicate them for
// the TUI (and differ between history modes).

type codexRecord struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Input     string `json:"input"`
	Action    *struct {
		Command []string `json:"command"`
		Query   string   `json:"query"`
	} `json:"action"`
}

// Context Codex injects as user messages: <environment_context>,
// <user_instructions>, AGENTS.md, <turn_aborted>, …
var codexInjected = regexp.MustCompile(`^(<[A-Za-z_][A-Za-z0-9_ -]*>|# AGENTS\.md instructions)`)

func parseCodex(line []byte) []wire.Message {
	if !bytes.Contains(line, []byte(`"response_item"`)) {
		return nil
	}
	var r codexRecord
	if json.Unmarshal(line, &r) != nil || r.Type != "response_item" {
		return nil
	}
	var it codexItem
	if json.Unmarshal(r.Payload, &it) != nil {
		return nil
	}
	t := parseTime(r.Timestamp)
	switch it.Type {
	case "message":
		var texts []string
		for _, c := range it.Content {
			s := strings.TrimSpace(c.Text)
			if s == "" {
				continue
			}
			switch {
			case it.Role == "user" && c.Type == "input_text":
				if codexInjected.MatchString(s) {
					continue
				}
				texts = append(texts, s)
			case it.Role == "assistant" && c.Type == "output_text":
				texts = append(texts, s)
			}
		}
		if len(texts) == 0 {
			return nil
		}
		role := RoleUser
		if it.Role == "assistant" {
			role = RoleAssistant
		}
		return []wire.Message{{Role: role, Text: clip(strings.Join(texts, "\n\n"), maxText), Time: t}}
	case "function_call":
		return []wire.Message{{Role: RoleTool, ToolName: it.Name, Text: Summarize(json.RawMessage(it.Arguments)), Time: t}}
	case "custom_tool_call":
		return []wire.Message{{Role: RoleTool, ToolName: it.Name, Text: summarizeFreeform(it.Name, it.Input), Time: t}}
	case "local_shell_call":
		var s string
		if it.Action != nil {
			s = oneLine(strings.Join(it.Action.Command, " "), maxSummary)
		}
		return []wire.Message{{Role: RoleTool, ToolName: "shell", Text: s, Time: t}}
	case "web_search_call":
		var s string
		if it.Action != nil {
			s = oneLine(it.Action.Query, maxSummary)
		}
		return []wire.Message{{Role: RoleTool, ToolName: "web_search", Text: s, Time: t}}
	}
	return nil
}

var (
	patchFile   = regexp.MustCompile(`(?m)^\*\*\* (Add|Update|Delete) File: (.+)$`)
	codeModeCmd = regexp.MustCompile(`\bcmd\s*:\s*("(?:[^"\\]|\\.)*")`)
)

// summarizeFreeform summarizes a free-form tool input (apply_patch patches,
// code-mode JavaScript).
func summarizeFreeform(name, input string) string {
	if ms := patchFile.FindAllStringSubmatch(input, -1); ms != nil {
		files := make([]string, 0, len(ms))
		for _, m := range ms {
			files = append(files, m[1]+" "+strings.TrimSpace(m[2]))
		}
		return oneLine(strings.Join(files, ", "), maxSummary)
	}
	if m := codeModeCmd.FindStringSubmatch(input); m != nil {
		if s, err := strconv.Unquote(m[1]); err == nil {
			return oneLine(s, maxSummary)
		}
	}
	for _, l := range strings.Split(input, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return oneLine(l, maxSummary)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// omp: ~/.omp/agent/sessions/<project>/<timestamp>_<id>.jsonl

type ompRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Intent    string          `json:"intent"`
		} `json:"content"`
		Attribution string `json:"attribution"`
		Timestamp   int64  `json:"timestamp"`
	} `json:"message"`
}

func parseOmp(line []byte) []wire.Message {
	if !bytes.HasPrefix(line, []byte(`{"type":"message"`)) && !bytes.Contains(line, []byte(`"type":"message"`)) {
		return nil
	}
	var r ompRecord
	if json.Unmarshal(line, &r) != nil || r.Type != "message" {
		return nil
	}
	t := r.Message.Timestamp
	if t == 0 {
		t = parseTime(r.Timestamp)
	}
	var out []wire.Message
	switch r.Message.Role {
	case "user":
		if r.Message.Attribution != "" && r.Message.Attribution != "user" {
			return nil
		}
		var texts []string
		for _, c := range r.Message.Content {
			switch c.Type {
			case "text":
				if s := strings.TrimSpace(c.Text); s != "" {
					texts = append(texts, s)
				}
			case "image":
				texts = append(texts, "[image]")
			}
		}
		if len(texts) > 0 {
			out = append(out, wire.Message{Role: RoleUser, Text: clip(strings.Join(texts, "\n\n"), maxText), Time: t})
		}
	case "assistant":
		var texts []string
		flush := func() {
			if len(texts) > 0 {
				out = append(out, wire.Message{Role: RoleAssistant, Text: clip(strings.Join(texts, "\n\n"), maxText), Time: t})
				texts = nil
			}
		}
		for _, c := range r.Message.Content {
			switch c.Type {
			case "text":
				if s := strings.TrimSpace(c.Text); s != "" {
					texts = append(texts, s)
				}
			case "toolCall":
				flush()
				s := Summarize(c.Arguments)
				if s == "" {
					s = oneLine(c.Intent, maxSummary)
				}
				out = append(out, wire.Message{Role: RoleTool, ToolName: c.Name, Text: s, Time: t})
			}
		}
		flush()
	}
	return out
}

// ---------------------------------------------------------------------------
// Shared helpers

// summaryKeys are tool argument keys in the order they best describe a call.
var summaryKeys = []string{
	"command", "cmd", "file_path", "notebook_path", "path", "pattern", "query",
	"url", "description", "prompt", "skill", "task", "intent", "i",
}

// Summarize renders tool arguments (a JSON object, or a JSON string holding
// one) as a single short line: the command, file path, pattern, … whichever
// comes first in summaryKeys.
func Summarize(input json.RawMessage) string {
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return ""
	}
	if input[0] == '"' {
		var s string
		if json.Unmarshal(input, &s) != nil {
			return ""
		}
		input = json.RawMessage(s)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return ""
	}
	for _, k := range summaryKeys {
		v, ok := args[k]
		if !ok {
			continue
		}
		if s, ok := jsonString(v); ok && strings.TrimSpace(s) != "" {
			if strings.HasPrefix(strings.TrimSpace(s), "*** Begin Patch") {
				return summarizeFreeform("apply_patch", s) // Codex apply_patch approvals
			}
			return oneLine(s, maxSummary)
		}
		var list []string
		if json.Unmarshal(v, &list) == nil && len(list) > 0 {
			return oneLine(strings.Join(list, " "), maxSummary)
		}
	}
	return ""
}

func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// oneLine collapses whitespace and clips to max runes.
func oneLine(s string, max int) string {
	return clip(strings.Join(strings.Fields(s), " "), max)
}

// clip shortens s to at most max bytes on a rune boundary, marking the cut.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
