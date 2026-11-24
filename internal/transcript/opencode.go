package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// OpenCode (V2) keeps its sessions in a database, not in files. stagent
// keeps its own record of each session its plugin reports
// (install/opencode.go): <root>/<session id>.jsonl, where root is the
// registry's TranscriptRoot (stagent's state directory). The hook reads the
// session with `opencode session export --standalone <id>` on the plugin's
// events and appends what the file lacks (SyncOpenCode), so the file only
// grows and is read like any other harness's transcript. Its lines:
//
//   - {"type":"session","id","title","directory","created"}: the session,
//     written first and again whenever its title or directory changes (the
//     last one counts);
//   - the export's messages as they are, one per line, by id: user {text,
//     files, agents}; assistant {content: [{type: text, text} | {type:
//     reasoning} | {type: tool, id, name, state: {status, input, content,
//     metadata, error}}], finish, tokens, error}; idle (a turn ended);
//     synthetic (OpenCode's own notes, such as after a restart of its
//     server).
//
// Beside a session file the hook keeps <id>.proc, the OpenCode process
// that last reported it (follow/transcripts_opencode.go); it is no
// conversation.

var openCodeFormat = format{
	dir:       func(root string) string { return root },
	enumerate: listOpenCodeSessions,
	named:     func(path, id string) bool { return filepath.Base(path) == id+".jsonl" },
	scan:      scanOpenCode,
	titler:    func(*Index) func(*entry) string { return titleOrPrompt },
	parse:     perRecord(parseOpenCode),
}

// listOpenCodeSessions lists <root>/*.jsonl.
func listOpenCodeSessions(root string) ([]fileStat, error) {
	files, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []fileStat
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		if info, err := f.Info(); err == nil {
			out = append(out, fileStat{filepath.Join(root, f.Name()), info.ModTime().UnixNano(), info.Size()})
		}
	}
	return out, nil
}

// openCodeSession is stagent's session record.
type openCodeSession struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	Created   int64  `json:"created"`
}

var openCodeSessionRecord = []byte(`{"type":"session"`)

// scanOpenCode reads the session's id, directory and title (its last
// session record) and its first prompt, from where the previous scan
// stopped.
func scanOpenCode(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		if bytes.HasPrefix(line, openCodeSessionRecord) {
			var s openCodeSession
			if json.Unmarshal(line, &s) == nil {
				e.ID, e.Cwd, e.Title = s.ID, s.Directory, clip(strings.TrimSpace(s.Title), 1024)
			}
			return true
		}
		if e.Prompt == "" && bytes.Contains(line, []byte(`"type":"user"`)) {
			for _, m := range messages(parseOpenCode(line)) {
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
}

type openCodePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	ID    string `json:"id"`
	Name  string `json:"name"`
	State struct {
		Status   string          `json:"status"`
		Input    json.RawMessage `json:"input"`
		Content  json.RawMessage `json:"content"` // []openCodeFile
		Output   json.RawMessage `json:"output"`  // a string
		Error    json.RawMessage `json:"error"`   // {message}, or a string
		Metadata struct {
			Answers [][]string `json:"answers"`
		} `json:"metadata"`
	} `json:"state"`
}

// openCodeFile is a block of a tool result's content (text, or a file:
// an image as a data: or file: URL), or a file attached to a prompt.
type openCodeFile struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Mime      string `json:"mime"`
	MediaType string `json:"mediaType"`
	URL       string `json:"url"`
	Data      string `json:"data"`
	Path      string `json:"path"`
}

// image is the block's image, if it is one.
func (f openCodeFile) image() (Image, bool) {
	mt := firstNonEmpty(f.Mime, f.MediaType)
	if !imageMediaType(mt) && f.Type != "image" {
		return Image{}, false
	}
	switch {
	case f.Data != "":
		return base64Image(mt, f.Data), true
	case f.URL != "":
		return urlImage(mt, f.URL), true
	}
	return Image{MediaType: strings.ToLower(mt), Path: f.Path}, true
}

type openCodeMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Text    string          `json:"text"`
	Files   json.RawMessage `json:"files"` // []openCodeFile
	Content []openCodePart  `json:"content"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseOpenCode reads the user's prompts and the images attached to them,
// the assistant's texts (not its reasoning), tool calls and their results,
// its questions and their answers, and the error that cut a reply short.
// Session records, the ends of turns, OpenCode's own notes and messages of
// other types are skipped.
func parseOpenCode(line []byte) []Entry {
	var m openCodeMessage
	if json.Unmarshal(line, &m) != nil {
		return nil
	}
	t := m.Time.Created
	switch m.Type {
	case "user":
		var images []Image
		for _, f := range openCodeFiles(m.Files) {
			if img, ok := f.image(); ok {
				images = append(images, img)
			}
		}
		if s := strings.TrimSpace(m.Text); s != "" || len(images) > 0 {
			return []Entry{{Role: RoleUser, Text: s, Images: images, Time: t}}
		}
	case "assistant":
		var out []Entry
		for _, p := range m.Content {
			switch p.Type {
			case "text":
				if s := strings.TrimSpace(p.Text); s != "" {
					out = append(out, Entry{Role: RoleAssistant, Text: s, Time: t})
				}
			case "tool":
				if p.Name == openCodeQuestionTool {
					out = append(out, openCodeQuestion(p, t)...)
					continue
				}
				out = append(out, toolCall(p.Name, p.ID, rawInput(p.State.Input), Summarize(p.State.Input), t))
				out = append(out, openCodeOutput(p, t)...)
			}
		}
		if m.Error != nil && len(m.Content) == 0 || m.Error != nil && hasDeclinedTool(m.Content) {
			if s := strings.TrimSpace(m.Error.Message); s != "" {
				out = append(out, Entry{Role: RoleSystem, Text: s, Time: m.Time.Completed})
			}
		}
		return out
	}
	return nil
}

// openCodeOutput is the output of a tool call that ended: its content's
// text and images, else its output, else its error.
func openCodeOutput(p openCodePart, t int64) []Entry {
	var texts []string
	var images []Image
	for _, c := range openCodeFiles(p.State.Content) {
		if img, ok := c.image(); ok {
			images = append(images, img)
		} else if strings.TrimSpace(c.Text) != "" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.Join(texts, "\n")
	if text == "" {
		text, _ = jsonString(p.State.Output)
	}
	if text == "" {
		var e struct {
			Message string `json:"message"`
		}
		if s, ok := jsonString(p.State.Error); ok {
			text = s
		} else if json.Unmarshal(p.State.Error, &e) == nil {
			text = e.Message
		}
	}
	return toolOutput(p.Name, p.ID, text, images, t)
}

// openCodeFiles reads a list of content blocks or files; nil when it is
// none.
func openCodeFiles(raw json.RawMessage) []openCodeFile {
	var fs []openCodeFile
	if json.Unmarshal(raw, &fs) != nil {
		return nil
	}
	return fs
}

// hasDeclinedTool: a tool call of content failed (turned down, or
// interrupted), which ended the reply.
func hasDeclinedTool(content []openCodePart) bool {
	for _, p := range content {
		if p.Type == "tool" && p.State.Status == "error" && p.Name != openCodeQuestionTool {
			return true
		}
	}
	return false
}

// openCodeQuestionTool is OpenCode's question tool.
var openCodeQuestionTool = harness.QuestionToolOf(wire.HarnessOpenCode)

// openCodeQuestion reads a question tool call, {questions: [{question,
// header, options: [{label, description}], multiple}]}, as its question
// row, then its answer row once it ended: the answers its result records
// (metadata.answers, one list per question), or the question closed
// unanswered when it failed (dismissed) or recorded none.
func openCodeQuestion(p openCodePart, t int64) []Entry {
	var in struct {
		Questions []struct {
			Question string `json:"question"`
			Header   string `json:"header"`
			Multiple bool   `json:"multiple"`
			Options  []struct {
				Label string `json:"label"`
			} `json:"options"`
		} `json:"questions"`
	}
	if json.Unmarshal(p.State.Input, &in) != nil || len(in.Questions) == 0 {
		return []Entry{toolCall(p.Name, p.ID, rawInput(p.State.Input), Summarize(p.State.Input), t)}
	}
	items := make([]wire.QuestionItem, len(in.Questions))
	for i, q := range in.Questions {
		labels := make([]string, 0, len(q.Options))
		for _, o := range q.Options {
			labels = append(labels, o.Label)
		}
		items[i] = wire.QuestionItem{Header: q.Header, Question: q.Question, Multi: q.Multiple, Options: labels}
	}
	out := []Entry{askQuestionRow(p.Name, p.ID, rawInput(p.State.Input), items, t)}
	switch p.State.Status {
	case "completed":
		var answers []wire.QuestionItem
		for i, a := range p.State.Metadata.Answers {
			if i < len(in.Questions) {
				answers = append(answers, wire.QuestionItem{Question: in.Questions[i].Question, Answers: typedAnswers(nil, a...)})
			}
		}
		if len(answers) == 0 {
			return append(out, askDeclinedRow(p.ID, t))
		}
		return append(out, askAnswerRow(p.ID, answers, t))
	case "error":
		return append(out, askDeclinedRow(p.ID, t))
	}
	return out
}

// OpenCodeProcPath is the process sidecar of the session recorded at path:
// {"pid", "started" (unix ms)}, the OpenCode process (its terminal UI) that
// last reported the session.
func OpenCodeProcPath(path string) string {
	return strings.TrimSuffix(path, ".jsonl") + ".proc"
}

// SyncOpenCode brings stagent's record of an OpenCode session at path
// (<root>/<session id>.jsonl) up to export, the output of `opencode session
// export <id>` ({info, messages}): it appends the session record when the
// title or directory changed and the messages the file lacks, in the
// export's order up to the first reply still being written (an assistant
// message without its completion time), which a later export brings
// complete. What the file holds is never rewritten. The file is replaced
// whole (a reader sees it before or after, never a part); two syncs at once
// write what one of them read, which the next sync completes.
func SyncOpenCode(path string, export []byte) error {
	var ex struct {
		Info struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Time  struct {
				Created int64 `json:"created"`
			} `json:"time"`
			Location struct {
				Directory string `json:"directory"`
			} `json:"location"`
		} `json:"info"`
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(export, &ex); err != nil {
		return fmt.Errorf("opencode export: %w", err)
	}
	if id := strings.TrimSuffix(filepath.Base(path), ".jsonl"); ex.Info.ID != id {
		return fmt.Errorf("opencode export: session %q, want %q", ex.Info.ID, id)
	}
	cur, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	have := map[string]bool{}
	var last openCodeSession
	for line := range bytes.Lines(cur) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, openCodeSessionRecord) {
			json.Unmarshal(line, &last)
			continue
		}
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(line, &m) == nil && m.ID != "" {
			have[m.ID] = true
		}
	}

	var add bytes.Buffer
	s := openCodeSession{Type: "session", ID: ex.Info.ID, Title: ex.Info.Title, Directory: ex.Info.Location.Directory, Created: ex.Info.Time.Created}
	if len(cur) == 0 || s.Title != last.Title || s.Directory != last.Directory {
		b, _ := json.Marshal(s)
		add.Write(b)
		add.WriteByte('\n')
	}
	for _, raw := range ex.Messages {
		var m openCodeMessage
		if json.Unmarshal(raw, &m) != nil || m.ID == "" {
			continue
		}
		if m.Type == "assistant" && m.Time.Completed == 0 {
			break
		}
		if have[m.ID] {
			continue
		}
		if err := json.Compact(&add, raw); err != nil {
			continue
		}
		add.WriteByte('\n')
	}
	if add.Len() == 0 {
		return nil
	}
	if len(cur) > 0 && cur[len(cur)-1] != '\n' {
		cur = append(cur, '\n')
	}
	return fsutil.WriteFileAtomic(path, append(cur, add.Bytes()...))
}
