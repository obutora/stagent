package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Claude Code: <claude config dir>/projects/<project>/<uuid>.jsonl

var claudeFormat = format{
	dir: func(root string) string { return root },
	enumerate: func(dir string) ([]fileStat, error) {
		return listTwoLevels(dir, func(name string) bool { return claudeIDName.MatchString(name) })
	},
	named:  func(path, id string) bool { return filepath.Base(path) == id+".jsonl" },
	scan:   scanClaude,
	titler: func(*Index) func(*entry) string { return claudeEntryTitle },
	parse:  parseClaude,
}

var claudeIDName = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\.jsonl$`)

var (
	cwdKey        = []byte(`"cwd":"`)
	entrypointKey = []byte(`"entrypoint":"`)
	claudeTitle   = map[string]string{
		"custom-title": "customTitle", "ai-title": "aiTitle",
		"summary": "summary", "last-prompt": "lastPrompt",
	}
)

// scanClaude takes the id from the file name and reads from e.Scanned:
// the first cwd, the first entrypoint (sdk-* runs are automation and
// skipped) and the latest title records.
func scanClaude(path string, e *entry) {
	e.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if e.Cwd == "" {
			if s, ok := stringAfter(line, cwdKey); ok {
				e.Cwd = s
			}
		}
		if !e.Checked {
			if s, ok := stringAfter(line, entrypointKey); ok {
				e.Checked = true
				if strings.HasPrefix(s, "sdk") {
					e.Skip = true
					return false
				}
			}
		}
		if complete && bytes.HasPrefix(line, []byte(`{"type":"`)) {
			var r map[string]json.RawMessage
			if t, ok := stringAfter(line, []byte(`{"type":"`)); ok {
				if field, want := claudeTitle[t]; want && json.Unmarshal(line, &r) == nil {
					if v, ok := jsonString(r[field]); ok && strings.TrimSpace(v) != "" {
						if e.Titles == nil {
							e.Titles = map[string]string{}
						}
						e.Titles[t] = clip(v, 1024)
					}
				}
			}
		}
		return true
	}, &e.Scanned)
}

// claudeEntryTitle picks the best of the title records.
func claudeEntryTitle(e *entry) string {
	for _, k := range []string{"custom-title", "ai-title", "summary", "last-prompt"} {
		if t := oneLine(e.Titles[k], maxSummary); t != "" {
			return t
		}
	}
	return ""
}

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
