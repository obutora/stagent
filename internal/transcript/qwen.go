package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Qwen Code: <runtime dir>/projects/<cwd, every character but [A-Za-z0-9]
// as ->/chats/<session id>.jsonl, appended to (a resume appends to the
// same file). Every record has uuid, parentUuid, sessionId, timestamp,
// type, cwd and version; the conversation's are Gemini's contents:
//
//   - user: message {role: user, parts: [{text}]}; subtype
//     mid_turn_user_message for what the user sent while it worked;
//   - assistant: message {role: model, parts: [{text, thought?} or
//     {functionCall: {id, name, args}}]}, with usageMetadata;
//   - tool_result: message {role: user, parts: [{functionResponse: {id,
//     name, response}}]} and toolCallResult {callId, status, resultDisplay}
//     (status cancelled: the call was turned down or interrupted);
//   - system: telemetry, snapshots, compression and the like, some with a
//     message of their own (the memory catalog) — never a message of the
//     conversation.
//
// Records of other types, and lines that do not parse, are skipped. Beside
// the conversations chats/ holds sidecars: <id>.title.json (its title,
// displayName), <id>.runtime.json, <id>.ledger.jsonl and others; the
// conversations are the .jsonl files but the ledgers, as Qwen Code counts
// them.

var qwenFormat = format{
	dir:       func(root string) string { return root },
	enumerate: listQwenChats,
	named:     func(path, id string) bool { return filepath.Base(path) == id+".jsonl" },
	scan:      scanQwen,
	titler:    func(*Index) func(*entry) string { return titleOrPrompt },
	parse:     perRecord(parseQwen),
}

// listQwenChats lists <root>/<project>/chats/*.jsonl but the ledgers.
func listQwenChats(root string) ([]fileStat, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []fileStat
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		chats := filepath.Join(root, p.Name(), "chats")
		files, err := os.ReadDir(chats)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if f.IsDir() || !strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".ledger.jsonl") {
				continue
			}
			if info, err := f.Info(); err == nil {
				out = append(out, fileStat{filepath.Join(chats, name), info.ModTime().UnixNano(), info.Size()})
			}
		}
	}
	return out, nil
}

// scanQwen reads the session's id and cwd (any record's) and its first
// prompt, from where the previous scan stopped, then its title sidecar:
// the name Qwen Code shows in its session list (the first prompt, or the
// one given with /rename).
func scanQwen(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		if e.ID == "" {
			var r struct {
				SessionID string `json:"sessionId"`
				Cwd       string `json:"cwd"`
			}
			if json.Unmarshal(line, &r) == nil {
				e.ID, e.Cwd = r.SessionID, r.Cwd
			}
		}
		if e.Prompt == "" && bytes.Contains(line, []byte(`"type":"user"`)) {
			for _, m := range parseQwen(line) {
				if m.Role == RoleUser {
					e.Prompt = clip(m.Text, 1024)
				}
			}
		}
		return true
	}, &e.Scanned)
	if e.ID == "" {
		e.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	var title struct {
		DisplayName string `json:"displayName"`
	}
	if b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".title.json"); err == nil && json.Unmarshal(b, &title) == nil {
		e.Title = clip(strings.TrimSpace(title.DisplayName), 1024)
	}
}

type qwenPart struct {
	Text         string `json:"text"`
	Thought      bool   `json:"thought"`
	FunctionCall *struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall"`
	FunctionResponse *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"functionResponse"`
}

type qwenRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Parts []qwenPart `json:"parts"`
	} `json:"message"`
	ToolCallResult struct {
		Status        string `json:"status"`
		ResultDisplay struct {
			Answers []struct {
				Question string `json:"question"`
				Answer   string `json:"answer"`
			} `json:"answers"`
		} `json:"resultDisplay"`
	} `json:"toolCallResult"`
}

// parseQwen reads the user's prompts (also those sent while it worked),
// the model's texts (not its thoughts) and tool calls, and the answers to
// its questions (ask_user_question, a question row and its answer row).
// Other tool results and every other record are skipped.
func parseQwen(line []byte) []wire.Message {
	var r qwenRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	t := parseTime(r.Timestamp)
	switch r.Type {
	case "user":
		var texts []string
		for _, p := range r.Message.Parts {
			if s := strings.TrimSpace(p.Text); s != "" && !p.Thought {
				texts = append(texts, s)
			}
		}
		if len(texts) > 0 {
			return []wire.Message{{Role: RoleUser, Text: clip(strings.Join(texts, "\n\n"), maxText), Time: t}}
		}
	case "assistant":
		var out []wire.Message
		for _, p := range r.Message.Parts {
			switch {
			case p.FunctionCall != nil:
				if m, ok := qwenAskQuestion(p.FunctionCall.Name, p.FunctionCall.ID, p.FunctionCall.Args, t); ok {
					out = append(out, m)
					continue
				}
				out = append(out, wire.Message{Role: RoleTool, ToolName: p.FunctionCall.Name, Text: Summarize(p.FunctionCall.Args), Time: t})
			case p.Thought:
			case strings.TrimSpace(p.Text) != "":
				out = append(out, wire.Message{Role: RoleAssistant, Text: clip(strings.TrimSpace(p.Text), maxText), Time: t})
			}
		}
		return out
	case "tool_result":
		for _, p := range r.Message.Parts {
			if p.FunctionResponse != nil && p.FunctionResponse.Name == qwenQuestionTool {
				return []wire.Message{qwenAskAnswer(p.FunctionResponse.ID, r, t)}
			}
		}
	}
	return nil
}

// qwenQuestionTool is Qwen Code's question tool.
var qwenQuestionTool = harness.QuestionToolOf(wire.HarnessQwen)

// qwenAskQuestion reads the question row of an ask_user_question call:
// {questions: [{question, header, options: [{label, description}],
// multiSelect}]}.
func qwenAskQuestion(name, id string, args json.RawMessage, t int64) (wire.Message, bool) {
	if name != qwenQuestionTool {
		return wire.Message{}, false
	}
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			Options     []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(args, &in) != nil || len(in.Questions) == 0 {
		return wire.Message{}, false
	}
	items := make([]wire.QuestionItem, len(in.Questions))
	for i, q := range in.Questions {
		labels := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		items[i] = wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.MultiSelect, Options: labels}
	}
	return askQuestionRow(name, id, items, t), true
}

// qwenAskAnswer reads the answer row of an ask_user_question result: the
// answers by question (toolCallResult.resultDisplay.answers); a call
// cancelled or answered with nothing closes the question unanswered.
func qwenAskAnswer(id string, r qwenRecord, t int64) wire.Message {
	var items []wire.QuestionItem
	for _, a := range r.ToolCallResult.ResultDisplay.Answers {
		items = append(items, wire.QuestionItem{Question: a.Question, Answers: typedAnswers(nil, a.Answer)})
	}
	if r.ToolCallResult.Status != "success" || len(items) == 0 {
		return askDeclinedRow(id, t)
	}
	return askAnswerRow(id, items, t)
}
