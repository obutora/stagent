package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// omp: ~/.omp/agent/sessions/<project>/<timestamp>_<id>.jsonl

var ompFormat = ompSessionFormat(scanOmp, ompEntryTitle)

// ompSessionFormat is the format of omp's session files, which Pi (omp is
// a fork of Pi) shares: <sessions>/<project>/<timestamp>_<id>.jsonl, with
// omp's message entries. The harnesses differ in their metadata (scan) and
// titles.
func ompSessionFormat(scan func(path string, e *entry), title func(*entry) string) format {
	return format{
		dir: func(root string) string { return root },
		enumerate: func(dir string) ([]fileStat, error) {
			return listTwoLevels(dir, func(name string) bool { return strings.HasSuffix(name, ".jsonl") })
		},
		named:  func(path, id string) bool { return strings.HasSuffix(filepath.Base(path), "_"+id+".jsonl") },
		scan:   scan,
		titler: func(*Index) func(*entry) string { return title },
		parse:  perRecord(parseOmp),
	}
}

// ompFileID is the session id in the name of a session file of omp's
// format ("" when the name has none).
func ompFileID(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.IndexByte(base, '_'); i >= 0 {
		return base[i+1:]
	}
	return ""
}

// scanOmp reads the title record (line 1, rewritten in place) and the
// session header (line 2).
func scanOmp(path string, e *entry) {
	e.Title = ""
	forLines(path, 0, 2, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		var r struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Cwd   string `json:"cwd"`
			Title string `json:"title"`
		}
		if json.Unmarshal(line, &r) != nil {
			return true
		}
		switch r.Type {
		case "title":
			if r.Title != "" {
				e.Title = r.Title
			}
		case "session":
			e.ID, e.Cwd = r.ID, r.Cwd
			if e.Title == "" {
				e.Title = r.Title
			}
		}
		return true
	}, nil)
	if e.ID == "" {
		e.ID = ompFileID(path)
	}
	// Line 1 changes in place; never scan incrementally.
	e.Scanned = 0
}

func ompEntryTitle(e *entry) string { return oneLine(e.Title, maxSummary) }

type ompRecord struct {
	Type      string     `json:"type"`
	Timestamp string     `json:"timestamp"`
	Message   ompMessage `json:"message"`
}

type ompMessage struct {
	Role        string      `json:"role"`
	Content     ompContents `json:"content"`
	Attribution string      `json:"attribution"`
	Timestamp   int64       `json:"timestamp"`
	// Tool results
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Details    json.RawMessage `json:"details"`
	IsError    bool            `json:"isError"`
}

type ompContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Intent    string          `json:"intent"`
}

// ompContents are a message's content blocks. A user message may hold a
// plain string instead (Pi), read as one text block.
type ompContents []ompContent

func (c *ompContents) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*c = ompContents{{Type: "text", Text: s}}
		return nil
	}
	return json.Unmarshal(b, (*[]ompContent)(c))
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
				if c.Name == ompAskTool {
					if m, ok := ompAskQuestion(c, t); ok {
						out = append(out, m)
						continue
					}
				}
				s := Summarize(c.Arguments)
				if s == "" {
					s = oneLine(c.Intent, maxSummary)
				}
				out = append(out, wire.Message{Role: RoleTool, ToolName: c.Name, Text: s, Time: t})
			}
		}
		flush()
	case "toolResult":
		if r.Message.ToolName == ompAskTool {
			out = append(out, ompAskAnswer(r.Message, t))
		}
	}
	return out
}

// ompAskTool is omp's tool for asking the user questions.
var ompAskTool = harness.QuestionToolOf(wire.HarnessOmp)

func ompAskQuestion(c ompContent, t int64) (wire.Message, bool) {
	var args struct {
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			Multi    bool   `json:"multi"`
		} `json:"questions"`
	}
	if json.Unmarshal(c.Arguments, &args) != nil || len(args.Questions) == 0 {
		return wire.Message{}, false
	}
	items := make([]wire.QuestionItem, len(args.Questions))
	for i, q := range args.Questions {
		items[i] = wire.QuestionItem{ID: q.ID, Header: q.Header, Question: q.Question, Multi: q.Multi}
	}
	return askQuestionRow(c.Name, c.ID, items, t), true
}

// ompAskResult is the answer to one question in an ask result's details.
type ompAskResult struct {
	ID              string   `json:"id"`
	Question        string   `json:"question"`
	Multi           bool     `json:"multi"`
	SelectedOptions []string `json:"selectedOptions"`
	CustomInput     string   `json:"customInput"`
	Note            string   `json:"note"`
}

func (r ompAskResult) item() wire.QuestionItem {
	return wire.QuestionItem{
		ID: r.ID, Question: r.Question, Multi: r.Multi,
		Answers: typedAnswers(typedAnswers(nil, r.SelectedOptions...), r.CustomInput, r.Note),
	}
}

// ompAskAnswer reads the answer row from an ask result: details holds one
// question's answer, or results with one per question. An error result is
// a question closed without an answer (cancelled, skipped).
func ompAskAnswer(m ompMessage, t int64) wire.Message {
	id := m.ToolCallID
	if m.IsError {
		return askDeclinedRow(id, t)
	}
	var d struct {
		ompAskResult
		Results []ompAskResult `json:"results"`
	}
	_ = json.Unmarshal(m.Details, &d)
	var items []wire.QuestionItem
	switch {
	case len(d.Results) > 0:
		for _, r := range d.Results {
			items = append(items, r.item())
		}
	case d.Question != "":
		items = []wire.QuestionItem{d.ompAskResult.item()}
	default:
		// Details of a shape not known: the result's text as the answer.
		var texts []string
		for _, c := range m.Content {
			if c.Type == "text" {
				texts = typedAnswers(texts, c.Text)
			}
		}
		items = []wire.QuestionItem{{Answers: texts}}
	}
	return askAnswerRow(id, items, t)
}
