package transcript

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Codex: <codex home>/sessions/YYYY/MM/DD/rollout-<ts>-<id>.jsonl

var codexFormat = format{
	dir:       func(root string) string { return filepath.Join(root, "sessions") },
	enumerate: listCodex,
	named:     func(path, id string) bool { return strings.HasSuffix(filepath.Base(path), "-"+id+".jsonl") },
	scan:      scanCodex,
	titler:    codexTitler,
	parse:     parseCodex,
}

// listCodex walks the date directories of the sessions directory for
// rollouts.
func listCodex(root string) ([]fileStat, error) {
	var out []fileStat
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return fs.SkipAll
			}
			return nil
		}
		if d.IsDir() {
			if p != root && strings.Count(strings.TrimPrefix(p, root), string(filepath.Separator)) > 3 {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil {
			out = append(out, fileStat{p, info.ModTime().UnixNano(), info.Size()})
		}
		return nil
	})
	return out, err
}

type codexMeta struct {
	Type    string `json:"type"`
	Payload struct {
		ID         string          `json:"id"`
		Cwd        string          `json:"cwd"`
		Originator string          `json:"originator"`
		Source     json.RawMessage `json:"source"`
	} `json:"payload"`
}

// The first prompt is looked for within codexPromptScan bytes /
// codexPromptLines records of a rollout (developer instructions come first).
const (
	codexPromptScan  = 4 << 20
	codexPromptLines = 2000
)

// scanCodex reads the session_meta header (id, cwd; exec and sub-agent
// rollouts are skipped) and the first user prompt.
func scanCodex(path string, e *entry) {
	if e.Scanned == 0 {
		forLines(path, 0, 1, func(line []byte, complete bool) bool {
			var m codexMeta
			if !complete || json.Unmarshal(line, &m) != nil || m.Type != "session_meta" {
				return false
			}
			e.ID, e.Cwd = m.Payload.ID, m.Payload.Cwd
			src := bytes.TrimSpace(m.Payload.Source)
			if m.Payload.Originator == "codex_exec" || string(src) == `"exec"` || (len(src) > 0 && src[0] == '{') {
				e.Skip = true
			}
			return false
		}, nil)
		if e.ID == "" {
			// rollout-YYYY-MM-DDThh-mm-ss-<id>.jsonl
			base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
			if len(base) > len("rollout-2006-01-02T15-04-05-") {
				e.ID = base[len("rollout-2006-01-02T15-04-05-"):]
			}
		}
	}
	if e.Skip || e.Prompt != "" || e.Scanned >= codexPromptScan {
		return
	}
	forLines(path, e.Scanned, codexPromptLines, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		for _, m := range parseCodex(line, nil) {
			if m.Role == RoleUser {
				e.Prompt = clip(m.Text, 1024)
				return false
			}
		}
		return true
	}, &e.Scanned)
	if e.Scanned == 0 {
		e.Scanned = 1 // header read; keep the entry incremental
	}
}

// codexTitler titles conversations by their thread name, else their first
// prompt.
func codexTitler(x *Index) func(*entry) string {
	names := x.codexThreadNames()
	return func(e *entry) string {
		if t := oneLine(names[e.ID], maxSummary); t != "" {
			return t
		}
		return oneLine(e.Prompt, maxSummary)
	}
}

// codexThreadNames maps Codex thread ids to their names from
// session_index.jsonl (last record wins), re-read when the file changes.
func (x *Index) codexThreadNames() map[string]string {
	p := filepath.Join(x.roots[wire.HarnessCodex], "session_index.jsonl")
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	key := [2]int64{st.ModTime().UnixNano(), st.Size()}
	if x.threadNames != nil && key == x.threadNamesStat {
		return x.threadNames
	}
	names := map[string]string{}
	forLines(p, 0, 0, func(line []byte, complete bool) bool {
		var r struct {
			ID         string `json:"id"`
			ThreadName string `json:"thread_name"`
		}
		if complete && json.Unmarshal(line, &r) == nil && r.ID != "" && r.ThreadName != "" {
			names[r.ID] = r.ThreadName
		}
		return true
	}, nil)
	x.threadNames, x.threadNamesStat = names, key
	return names
}

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
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    *struct {
		Command []string `json:"command"`
		Query   string   `json:"query"`
	} `json:"action"`
}

// Context Codex injects as user messages: <environment_context>,
// <user_instructions>, AGENTS.md, <turn_aborted>, …
var codexInjected = regexp.MustCompile(`^(<[A-Za-z_][A-Za-z0-9_ -]*>|# AGENTS\.md instructions)`)

func parseCodex(line []byte, asked askedFunc) []wire.Message {
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
		if it.Name == codexAskTool {
			if m, ok := codexAskQuestion(it, t); ok {
				return []wire.Message{m}
			}
		}
		return []wire.Message{{Role: RoleTool, ToolName: it.Name, Text: Summarize(json.RawMessage(it.Arguments)), Time: t}}
	case "function_call_output":
		if m, ok := codexAskAnswer(it, t, asked); ok {
			return []wire.Message{m}
		}
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

// codexAskTool is Codex's tool for asking the user questions.
var codexAskTool = harness.QuestionToolOf(wire.HarnessCodex)

// codexAskCancelled is the output of a request_user_input call closed
// without a response.
var codexAskCancelled = codexAskTool + " was cancelled"

// codexAborted is the output of a function call the user interrupted (Esc
// on request_user_input's question interrupts the turn): it names no tool,
// so it may be any function call's (`wait`, an MCP tool's); it closes a
// question only when its call id is one asked before it.
const codexAborted = "aborted by user"

// codexUserNote prefixes the notes the user typed among a question's
// answers.
const codexUserNote = "user_note: "

func codexAskQuestion(it codexItem, t int64) (wire.Message, bool) {
	var args struct {
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
		} `json:"questions"`
	}
	if json.Unmarshal([]byte(it.Arguments), &args) != nil || len(args.Questions) == 0 {
		return wire.Message{}, false
	}
	items := make([]wire.QuestionItem, len(args.Questions))
	for i, q := range args.Questions {
		items[i] = wire.QuestionItem{ID: q.ID, Header: q.Header, Question: q.Question}
	}
	return askQuestionRow(it.Name, it.CallID, items, t), true
}

// codexAskAnswer reads the answer row from a function call's output, if it
// is request_user_input's: its response, {"answers":{<question id>:
// {"answers":[…]}}}, the message of its cancellation, or the output of an
// interrupted call whose id asked knows. The output names no tool, so
// otherwise the shape alone tells.
func codexAskAnswer(it codexItem, t int64, asked askedFunc) (wire.Message, bool) {
	out := codexOutputText(it.Output)
	if strings.HasPrefix(out, codexAskCancelled) ||
		strings.HasPrefix(out, codexAborted) && asked != nil && asked(it.CallID) {
		return askDeclinedRow(it.CallID, t), true
	}
	if !strings.HasPrefix(out, `{"answers":`) {
		return wire.Message{}, false
	}
	var resp struct {
		Answers map[string]struct {
			Answers *[]string `json:"answers"`
		} `json:"answers"`
	}
	if json.Unmarshal([]byte(out), &resp) != nil || resp.Answers == nil {
		return wire.Message{}, false
	}
	ids := make([]string, 0, len(resp.Answers))
	for id, a := range resp.Answers {
		if a.Answers == nil {
			return wire.Message{}, false
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]wire.QuestionItem, len(ids))
	for i, id := range ids {
		var answers []string
		for _, a := range *resp.Answers[id].Answers {
			answers = typedAnswers(answers, strings.TrimPrefix(a, codexUserNote))
		}
		items[i] = wire.QuestionItem{ID: id, Answers: answers}
	}
	return askAnswerRow(it.CallID, items, t), true
}

// codexOutputText is a function call output's text: a string, or the text
// of its content items.
func codexOutputText(raw json.RawMessage) string {
	if s, ok := jsonString(raw); ok {
		return strings.TrimSpace(s)
	}
	var items []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range items {
		b.WriteString(c.Text)
	}
	return strings.TrimSpace(b.String())
}
