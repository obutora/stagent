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
			for _, m := range messages(parseQwen(line)) {
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
		ID       string          `json:"id"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response"`
	} `json:"functionResponse"`
	// Images: inline (base64) or a file's URI.
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
	FileData *struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	} `json:"fileData"`
}

// image is the part's image, if it is one.
func (p qwenPart) image() (Image, bool) {
	switch {
	case p.InlineData != nil && imageMediaType(p.InlineData.MimeType):
		return base64Image(p.InlineData.MimeType, p.InlineData.Data), true
	case p.FileData != nil && imageMediaType(p.FileData.MimeType):
		return urlImage(p.FileData.MimeType, p.FileData.FileURI), true
	}
	return Image{}, false
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

// parseQwen reads the user's prompts (also those sent while it worked) and
// their images, the model's texts (not its thoughts) and tool calls, the
// tool results, and the answers to its questions (ask_user_question, a
// question row and its answer row). Every other record is skipped.
func parseQwen(line []byte) []Entry {
	var r qwenRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	t := parseTime(r.Timestamp)
	switch r.Type {
	case "user":
		var texts []string
		var images []Image
		for _, p := range r.Message.Parts {
			if img, ok := p.image(); ok {
				images = append(images, img)
			} else if s := strings.TrimSpace(p.Text); s != "" && !p.Thought {
				texts = append(texts, s)
			}
		}
		if len(texts) > 0 || len(images) > 0 {
			return []Entry{{Role: RoleUser, Text: strings.Join(texts, "\n\n"), Images: images, Time: t}}
		}
	case "assistant":
		var out []Entry
		for _, p := range r.Message.Parts {
			switch {
			case p.FunctionCall != nil:
				if e, ok := qwenAskQuestion(p.FunctionCall.Name, p.FunctionCall.ID, p.FunctionCall.Args, t); ok {
					out = append(out, e)
					continue
				}
				out = append(out, toolCall(p.FunctionCall.Name, p.FunctionCall.ID, rawInput(p.FunctionCall.Args), Summarize(p.FunctionCall.Args), t))
			case p.Thought:
			case strings.TrimSpace(p.Text) != "":
				out = append(out, Entry{Role: RoleAssistant, Text: strings.TrimSpace(p.Text), Time: t})
			}
		}
		return out
	case "tool_result":
		var out []Entry
		var images []Image
		for _, p := range r.Message.Parts {
			if img, ok := p.image(); ok {
				images = append(images, img)
			}
		}
		for _, p := range r.Message.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			if fr.Name == qwenQuestionTool {
				return []Entry{qwenAskAnswer(fr.ID, r, t)}
			}
			out = append(out, toolOutput(fr.Name, fr.ID, qwenResponseText(fr.Response), images, t)...)
			images = nil
		}
		return out
	}
	return nil
}

// qwenResponseText is a function response's text: its output, else its
// error, else the response as recorded.
func qwenResponseText(raw json.RawMessage) string {
	var resp struct {
		Output json.RawMessage `json:"output"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &resp) == nil {
		for _, v := range []json.RawMessage{resp.Output, resp.Error} {
			if s, ok := jsonString(v); ok && strings.TrimSpace(s) != "" {
				return s
			}
		}
	}
	return rawInput(raw)
}

// qwenQuestionTool is Qwen Code's question tool.
var qwenQuestionTool = harness.QuestionToolOf(wire.HarnessQwen)

// qwenAskQuestion reads the question row of an ask_user_question call:
// {questions: [{question, header, options: [{label, description}],
// multiSelect}]}.
func qwenAskQuestion(name, id string, args json.RawMessage, t int64) (Entry, bool) {
	if name != qwenQuestionTool {
		return Entry{}, false
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
		return Entry{}, false
	}
	items := make([]wire.QuestionItem, len(in.Questions))
	for i, q := range in.Questions {
		labels := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		items[i] = wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.MultiSelect, Options: labels}
	}
	return askQuestionRow(name, id, rawInput(args), items, t), true
}

// qwenAskAnswer reads the answer row of an ask_user_question result: the
// answers by question (toolCallResult.resultDisplay.answers); a call
// cancelled or answered with nothing closes the question unanswered.
func qwenAskAnswer(id string, r qwenRecord, t int64) Entry {
	var items []wire.QuestionItem
	for _, a := range r.ToolCallResult.ResultDisplay.Answers {
		items = append(items, wire.QuestionItem{Question: a.Question, Answers: typedAnswers(nil, a.Answer)})
	}
	if r.ToolCallResult.Status != "success" || len(items) == 0 {
		return askDeclinedRow(id, t)
	}
	return askAnswerRow(id, items, t)
}
