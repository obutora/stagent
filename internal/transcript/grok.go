package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Grok Build: <grok home>/sessions/<cwd>/<session id>/updates.jsonl, the
// session's updates as its Agent Client Protocol sends them, appended to
// (a resume appends to the same file). <cwd> is the working directory
// percent-encoded (every byte but A-Z a-z 0-9 - . _ ~), or for one longer
// than 255 bytes encoded a slug and a hash, with the directory in a .cwd
// file there. Each line is {"timestamp": <unix s>, "method", "params":
// {"sessionId", "update": {"sessionUpdate", …}, "_meta": {"agentTimestampMs",
// "promptId"?, …}}}:
//
//   - session/update user_message_chunk: a prompt, content.text;
//   - session/update agent_message_chunk: the model's text, content.text
//     (agent_thought_chunk: its thinking, not shown);
//   - session/update tool_call: a tool call, toolCallId, title (the tool's
//     name), rawInput, _meta["x.ai/tool"].name;
//   - session/update tool_call_update: the call's progress and, with status
//     completed or failed, its end (rawOutput);
//   - _x.ai/session/update: Grok Build's own, turn_completed (with the
//     turn's usage), hook_execution, background_tasks — never a message.
//
// Other updates, and lines that do not parse, are skipped. summary.json
// beside the file holds the session's cwd and its titles (generated_title,
// session_summary); chat_history.jsonl is what the model was sent, the
// prompts wrapped in Grok Build's reminders.

var grokFormat = format{
	dir:       func(root string) string { return root },
	enumerate: listGrokSessions,
	named:     func(path, id string) bool { return filepath.Base(filepath.Dir(path)) == id },
	scan:      scanGrok,
	titler:    func(*Index) func(*entry) string { return titleOrPrompt },
	parse:     parseGrok,
}

const (
	grokUpdates = "updates.jsonl"
	grokSummary = "summary.json"
	// maxGrokSummary bounds the summary.json read.
	maxGrokSummary = 256 << 10
)

// grokQuestionTool is Grok Build's question tool.
var grokQuestionTool = harness.QuestionToolOf(wire.HarnessGrok)

// listGrokSessions lists <root>/<cwd>/<session id>/updates.jsonl.
func listGrokSessions(root string) ([]fileStat, error) {
	groups, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []fileStat
	for _, g := range groups {
		if !g.IsDir() {
			continue
		}
		files, err := listTwoLevels(filepath.Join(root, g.Name()), func(name string) bool { return name == grokUpdates })
		if err != nil {
			continue
		}
		out = append(out, files...)
	}
	return out, nil
}

// scanGrok reads the session's id and first prompt from where the previous
// scan stopped, then its summary.json: its cwd and title. Without one the
// cwd is read from the directory the session is kept in.
func scanGrok(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		if e.ID == "" {
			var r struct {
				Params struct {
					SessionID string `json:"sessionId"`
				} `json:"params"`
			}
			if json.Unmarshal(line, &r) == nil {
				e.ID = r.Params.SessionID
			}
		}
		if e.Prompt == "" && bytes.Contains(line, []byte(`"user_message_chunk"`)) {
			for _, m := range messages(parseGrok(line, nil)) {
				if m.Role == RoleUser {
					e.Prompt = clip(m.Text, 1024)
				}
			}
		}
		return true
	}, &e.Scanned)
	dir := filepath.Dir(path)
	if e.ID == "" {
		e.ID = filepath.Base(dir)
	}
	var s struct {
		Info struct {
			Cwd string `json:"cwd"`
		} `json:"info"`
		GeneratedTitle string `json:"generated_title"`
		SessionSummary string `json:"session_summary"`
	}
	if b, err := readLimited(filepath.Join(dir, grokSummary), maxGrokSummary); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	e.Title = clip(strings.TrimSpace(firstNonEmpty(s.GeneratedTitle, s.SessionSummary)), 1024)
	switch {
	case s.Info.Cwd != "":
		e.Cwd = s.Info.Cwd
	case e.Cwd == "":
		e.Cwd = grokGroupCwd(filepath.Dir(dir))
	}
}

// grokGroupCwd is the working directory of the sessions kept in group: its
// name decoded, or the .cwd file of a group whose name is a slug and a
// hash.
func grokGroupCwd(group string) string {
	if b, err := readLimited(filepath.Join(group, ".cwd"), 64<<10); err == nil {
		if s := strings.TrimSpace(string(b)); s != "" {
			return s
		}
	}
	if s, err := url.PathUnescape(filepath.Base(group)); err == nil && filepath.IsAbs(s) {
		return s
	}
	return ""
}
func readLimited(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

type grokRecord struct {
	Timestamp float64 `json:"timestamp"`
	Method    string  `json:"method"`
	Params    struct {
		Update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			Content       json.RawMessage `json:"content"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			RawOutput     json.RawMessage `json:"rawOutput"`
			Locations     []struct {
				Path string `json:"path"`
			} `json:"locations"`
			Meta struct {
				Tool struct {
					Name string `json:"name"`
					Kind string `json:"kind"`
				} `json:"x.ai/tool"`
			} `json:"_meta"`
		} `json:"update"`
		Meta struct {
			AgentTimestampMs int64 `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

// grokOutput is the part of a tool call's rawOutput read: its type (an
// AskUserQuestion's answers) and the text the model was given.
type grokOutput struct {
	Type         string `json:"type"`
	UserAnswered *struct {
		Answers []struct {
			Question string   `json:"question"`
			Picked   []string `json:"picked"`
		} `json:"answers"`
	} `json:"UserAnswered"`
	OutputForPrompt string `json:"output_for_prompt"`
	EditsApplied    *struct {
		ToolOutputForPrompt string `json:"tool_output_for_prompt"`
	} `json:"EditsApplied"`
}

// grokBlock is an ACP content block: text, or an image (base64 data, or
// a URI).
type grokBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
	URI      string `json:"uri"`
}

func (b grokBlock) image() Image {
	if b.Data != "" {
		return base64Image(b.MimeType, b.Data)
	}
	return urlImage(b.MimeType, b.URI)
}

// grokFileKinds are the kinds of Grok Build's tools that change files.
var grokFileKinds = map[string]bool{"edit": true, "write": true, "delete": true, "move": true}

// parseGrok reads the user's prompts, the model's texts (not its
// thinking) and tool calls, their outputs and the answers to its questions
// (ask_user_question, a question row and its answer row). The other
// updates of a tool call and every other record are skipped. The model's
// text comes as one record per streamed chunk, each its own row.
func parseGrok(line []byte, asked askedFunc) []Entry {
	if !bytes.Contains(line, []byte(`"session/update"`)) {
		return nil
	}
	var r grokRecord
	if json.Unmarshal(line, &r) != nil || r.Method != "session/update" {
		return nil
	}
	u := &r.Params.Update
	t := r.Params.Meta.AgentTimestampMs
	if t == 0 {
		t = int64(r.Timestamp * 1000)
	}
	switch u.SessionUpdate {
	case "user_message_chunk", "agent_message_chunk":
		var c grokBlock
		if json.Unmarshal(u.Content, &c) != nil {
			return nil
		}
		role := RoleAssistant
		if u.SessionUpdate == "user_message_chunk" {
			role = RoleUser
		}
		switch c.Type {
		case "text":
			if s := strings.TrimSpace(c.Text); s != "" {
				return []Entry{{Role: role, Text: s, Time: t}}
			}
		case "image":
			return []Entry{{Role: role, Images: []Image{c.image()}, Time: t}}
		}
	case "tool_call":
		name := firstNonEmpty(u.Meta.Tool.Name, u.Title)
		if name == grokQuestionTool {
			if e, ok := grokAskQuestion(u.ToolCallID, u.RawInput, t); ok {
				return []Entry{e}
			}
		}
		e := toolCall(name, u.ToolCallID, rawInput(u.RawInput), Summarize(u.RawInput), t)
		if len(e.Touched) == 0 && grokFileKinds[u.Meta.Tool.Kind] {
			if p := inputPath(e.Input); p != "" {
				e.Touched = []string{p}
			}
			for _, l := range u.Locations {
				if l.Path != "" && !containsString(e.Touched, l.Path) {
					e.Touched = append(e.Touched, l.Path)
				}
			}
		}
		return []Entry{e}
	case "tool_call_update":
		if u.Status != "completed" && u.Status != "failed" {
			return nil
		}
		var out grokOutput
		_ = json.Unmarshal(u.RawOutput, &out)
		if out.Type == "AskUserQuestion" || asked != nil && asked(u.ToolCallID) {
			var items []wire.QuestionItem
			if a := out.UserAnswered; a != nil && u.Status == "completed" {
				for _, it := range a.Answers {
					items = append(items, wire.QuestionItem{Question: it.Question, Answers: typedAnswers(nil, it.Picked...)})
				}
			}
			if len(items) == 0 {
				return []Entry{askDeclinedRow(u.ToolCallID, t)}
			}
			return []Entry{askAnswerRow(u.ToolCallID, items, t)}
		}
		text := out.OutputForPrompt
		if text == "" && out.EditsApplied != nil {
			text = out.EditsApplied.ToolOutputForPrompt
		}
		var blocks []struct {
			Type    string    `json:"type"`
			Content grokBlock `json:"content"`
			Path    string    `json:"path"`
		}
		_ = json.Unmarshal(u.Content, &blocks)
		var texts []string
		var images []Image
		for _, b := range blocks {
			switch {
			case b.Type == "content" && b.Content.Type == "image":
				images = append(images, b.Content.image())
			case b.Type == "content" && strings.TrimSpace(b.Content.Text) != "":
				texts = append(texts, b.Content.Text)
			}
		}
		if strings.TrimSpace(text) == "" {
			text = strings.Join(texts, "\n")
		}
		return toolOutput("", u.ToolCallID, text, images, t)
	}
	return nil
}

// grokAskQuestion reads the question row of an ask_user_question call:
// {questions: [{question, header?, options: [{label, description}],
// multi_select | multiSelect}]}.
func grokAskQuestion(id string, args json.RawMessage, t int64) (Entry, bool) {
	var in struct {
		Questions []struct {
			Question    string `json:"question"`
			Header      string `json:"header"`
			MultiSelect bool   `json:"multiSelect"`
			MultiSnake  bool   `json:"multi_select"`
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
		items[i] = wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.MultiSelect || q.MultiSnake, Options: labels}
	}
	return askQuestionRow(grokQuestionTool, id, rawInput(args), items, t), true
}
