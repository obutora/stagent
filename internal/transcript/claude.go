package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/obutora/stagent/internal/harness"
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
	parse:  perRecord(parseClaude),
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
	// ToolUseResult is the output of the tool whose tool_result the record
	// carries.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
}

type claudePart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
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
		var out []wire.Message
		add := func(s string) {
			if m, ok := claudeUserText(s); ok {
				m.Time = t
				out = append(out, m)
			}
		}
		if s, ok := jsonString(r.Message.Content); ok {
			add(s)
			return out
		}
		var parts []claudePart
		if json.Unmarshal(r.Message.Content, &parts) != nil {
			return nil
		}
		for _, p := range parts {
			switch p.Type {
			case "text":
				add(p.Text)
			case "image":
				add("[image]")
			case "tool_result":
				if m, ok := claudeAskAnswer(p.ToolUseID, r.ToolUseResult, t); ok {
					out = append(out, m)
				}
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
				if p.Name == claudeAskTool {
					if m, ok := claudeAskQuestion(p, t); ok {
						out = append(out, m)
						continue
					}
				}
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

// claudeAskTool is Claude Code's tool for asking the user questions.
var claudeAskTool = harness.QuestionToolOf(wire.HarnessClaude)

type claudeAskQuestionInput struct {
	Question    string `json:"question"`
	Header      string `json:"header"`
	MultiSelect bool   `json:"multiSelect"`
	Options     []struct {
		Label string `json:"label"`
	} `json:"options"`
}

// claudeAskOutput is AskUserQuestion's output (toolUseResult): the
// questions, the answers by question text, notes typed beside them and the
// text the user replied with instead.
type claudeAskOutput struct {
	Questions   []claudeAskQuestionInput   `json:"questions"`
	Answers     map[string]json.RawMessage `json:"answers"`
	Annotations map[string]struct {
		Notes string `json:"notes"`
	} `json:"annotations"`
	Response string `json:"response"`
}

// claudeNotesOnly is the answer of a question the user only wrote notes
// on.
const claudeNotesOnly = "(notes only)"

func claudeAskQuestion(p claudePart, t int64) (wire.Message, bool) {
	var in struct {
		Questions []claudeAskQuestionInput `json:"questions"`
	}
	if json.Unmarshal(p.Input, &in) != nil || len(in.Questions) == 0 {
		return wire.Message{}, false
	}
	items := make([]wire.QuestionItem, len(in.Questions))
	for i, q := range in.Questions {
		items[i] = wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.MultiSelect}
	}
	return askQuestionRow(p.Name, p.ID, items, t), true
}

// claudeAskAnswer reads the answer row from the toolUseResult of a
// tool_result record, if it is AskUserQuestion's. A rejected call records
// no output of it; the interrupt that follows closes the question.
func claudeAskAnswer(id string, raw json.RawMessage, t int64) (wire.Message, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return wire.Message{}, false
	}
	var out claudeAskOutput
	if json.Unmarshal(raw, &out) != nil || len(out.Questions) == 0 || out.Answers == nil {
		return wire.Message{}, false
	}
	items := make([]wire.QuestionItem, 0, len(out.Questions)+1)
	for _, q := range out.Questions {
		labels := make([]string, len(q.Options))
		for i, o := range q.Options {
			labels[i] = o.Label
		}
		answers := claudeAnswerList(out.Answers[q.Question], q.MultiSelect, labels)
		answers = typedAnswers(answers, out.Annotations[q.Question].Notes)
		items = append(items, wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.MultiSelect, Answers: answers})
	}
	if s := strings.TrimSpace(out.Response); s != "" {
		items = append(items, wire.QuestionItem{Answers: []string{s}})
	}
	return askAnswerRow(id, items, t), true
}

// claudeAnswerList is the answer to one question: the option chosen, the
// options of a multi-select (joined by ", ", labels holding ", " or a quote
// JSON-quoted, the text typed beside them last) or the text typed.
func claudeAnswerList(raw json.RawMessage, multi bool, labels []string) []string {
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return typedAnswers(nil, list...)
	}
	s, ok := jsonString(raw)
	if s = strings.TrimSpace(s); !ok || s == "" || s == claudeNotesOnly {
		return nil
	}
	if multi {
		if parts := splitClaudeMulti(s); len(parts) > 0 && allIn(parts[:len(parts)-1], labels) {
			return parts
		}
	}
	return []string{s}
}

// splitClaudeMulti undoes Claude Code's join of multi-select labels; nil
// when s is not such a join.
func splitClaudeMulti(s string) []string {
	var out []string
	for {
		if strings.HasPrefix(s, `"`) {
			end, esc := -1, false
			for i := 1; i < len(s); i++ {
				switch {
				case esc:
					esc = false
				case s[i] == '\\':
					esc = true
				case s[i] == '"':
					end = i
				}
				if end >= 0 {
					break
				}
			}
			if end < 0 {
				return nil
			}
			var label string
			if json.Unmarshal([]byte(s[:end+1]), &label) != nil {
				return nil
			}
			out = append(out, label)
			s = s[end+1:]
		} else {
			label, rest, found := strings.Cut(s, ", ")
			if strings.Contains(label, `"`) {
				return nil
			}
			out = append(out, label)
			s = ""
			if found {
				s = ", " + rest
			}
		}
		if s == "" {
			return out
		}
		if !strings.HasPrefix(s, ", ") {
			return nil
		}
		if s = s[2:]; s == "" {
			return nil
		}
	}
}

func allIn(xs, set []string) bool {
	for _, x := range xs {
		found := false
		for _, s := range set {
			if x == s {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
