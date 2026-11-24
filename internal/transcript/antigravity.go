package transcript

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Antigravity CLI: <home>/brain/<conversation id>/.system_generated/logs/
// transcript_full.jsonl, appended to (a resume appends to the same file).
// transcript.jsonl beside it is the same with long values cut, and chunks/
// split copies — neither is read. Each line is {step_index, source, type,
// status (DONE | RUNNING | ERROR), created_at (RFC 3339), …}; step indexes
// grow but skip a step that was cancelled (never written):
//
//   - USER_INPUT: a prompt, content "<USER_REQUEST>…</USER_REQUEST>"
//     followed by Antigravity CLI's own blocks (<ADDITIONAL_METADATA>, the
//     settings the user changed);
//   - PLANNER_RESPONSE: a reply of the model, written once complete, with
//     its text (content), its thinking (not shown), its tool calls
//     (tool_calls: [{name, args}], no call ids) and its usage;
//   - GENERIC: the result of one tool call, written when the tool ended:
//     content is "Created At:"/"Completed At:" lines, then the output;
//     status ERROR with an error for a call that failed or was turned down,
//     RUNNING for a background command still running. The call a result
//     answers has no id either: the results of a reply's calls are the
//     steps right after it, in the order of its calls, so the n-th call
//     (from 0) of the reply at step s is answered at step s+1+n — the id
//     both rows share;
//   - SYSTEM_MESSAGE: Antigravity CLI's notes to the model (a background
//     task ended, reminders), never a message of the conversation;
//   - ERROR_MESSAGE: a model error [INFERENCE: not seen in 1.5.14 runs; read
//     as its error, else its content].
//
// Other types, and lines that do not parse, are skipped. The conversation's
// directory is in none of its files but <home>/history.jsonl, a line per
// prompt submitted (antigravityHistory); its title only in a SQLite
// database, so conversations are titled by their first prompt.

var antigravityFormat = format{
	dir:       func(root string) string { return filepath.Join(root, antigravityBrain) },
	enumerate: listAntigravityConversations,
	named:     func(path, id string) bool { return antigravityID(path) == id },
	scan:      scanAntigravity,
	titler:    func(*Index) func(*entry) string { return titleOrPrompt },
	parse:     perRecord(parseAntigravity),
}

const (
	antigravityBrain       = "brain"
	antigravityTranscript  = "transcript_full.jsonl"
	antigravityHistoryFile = "history.jsonl"
)

// antigravityLogs is where a conversation's transcript lies below its
// brain directory.
var antigravityLogs = filepath.Join(".system_generated", "logs", antigravityTranscript)

// antigravityQuestionTool is Antigravity CLI's question tool.
var antigravityQuestionTool = harness.QuestionToolOf(wire.HarnessAntigravity)

// listAntigravityConversations lists <brain>/<id>/.system_generated/logs/
// transcript_full.jsonl. A conversation old enough to have none (only its
// database) is not listed.
func listAntigravityConversations(brain string) ([]fileStat, error) {
	dirs, err := os.ReadDir(brain)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []fileStat
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(brain, d.Name(), antigravityLogs)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			out = append(out, fileStat{p, info.ModTime().UnixNano(), info.Size()})
		}
	}
	return out, nil
}

// antigravityID is the conversation id of a transcript path: its brain
// directory's name.
func antigravityID(path string) string {
	return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
}

// antigravityHome is the Antigravity CLI home a transcript path lies in.
func antigravityHome(path string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(path)))))
}

// scanAntigravity takes the id from the path and reads the first prompt
// from where the previous scan stopped (nothing after it is needed), then
// the cwd from history.jsonl until it is known.
func scanAntigravity(path string, e *entry) {
	e.ID = antigravityID(path)
	var first antigravityPrompt
	if e.Prompt == "" {
		first = firstAntigravityPrompt(path, e.Scanned, &e.Scanned)
		e.Prompt = clip(first.text, 1024)
	}
	if e.Cwd == "" {
		e.Cwd = antigravityCwd(path, first)
	}
}

// AntigravityCwd is the working directory of the Antigravity CLI
// conversation whose transcript is path, "" while history.jsonl does not
// tell it.
func AntigravityCwd(path string) string {
	return antigravityCwd(path, antigravityPrompt{})
}

// antigravityCwd looks the conversation of path up in its home's
// history.jsonl: by its id, else by its first prompt (read from path unless
// given), which Antigravity CLI records without the id of the conversation
// it then creates.
func antigravityCwd(path string, first antigravityPrompt) string {
	h := antigravityHistoryOf(filepath.Join(antigravityHome(path), antigravityHistoryFile))
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refresh()
	if cwd := h.byID[antigravityID(path)]; cwd != "" {
		return cwd
	}
	if first.at == 0 {
		first = firstAntigravityPrompt(path, 0, nil)
	}
	return h.firstPromptCwd(first)
}

// antigravityPrompt is a conversation's first prompt: when it was sent
// (unix ms) and its text.
type antigravityPrompt struct {
	at   int64
	text string
}

// firstAntigravityPrompt reads the first prompt of path from offset on.
// scanned, when non-nil, receives how far the file was read.
func firstAntigravityPrompt(path string, offset int64, scanned *int64) antigravityPrompt {
	var p antigravityPrompt
	forLines(path, offset, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		for _, m := range messages(parseAntigravity(line)) {
			if m.Role == RoleUser {
				p = antigravityPrompt{m.Time, m.Text}
				return false
			}
		}
		return true
	}, scanned)
	return p
}

// ---------------------------------------------------------------------------
// history.jsonl

// antigravityHistory is what a home's history.jsonl tells of its
// conversations' directories. Each line is a prompt submitted: {display
// (the prompt), timestamp (unix ms), workspace (the cwd), conversationId};
// a slash command has type slash_command and no conversation id, and so
// has the first prompt of a new conversation (the conversation is created
// after it). The file is append-only: it is read on from where the last
// read stopped, and kept for every scan of the process.
type antigravityHistory struct {
	path string

	mu          sync.Mutex
	size, mtime int64 // the file when last read
	scanned     int64
	// byID maps conversation ids to the workspace of their first line.
	byID map[string]string
	// firsts are the prompts recorded without a conversation id.
	firsts []antigravityFirst
}

type antigravityFirst struct {
	at        int64
	workspace string
	display   string // clipped to maxAntigravityDisplay
}

const (
	// maxAntigravityDisplay bounds the prompt text kept per first prompt.
	maxAntigravityDisplay = 1024
	// antigravityFirstSlack is how far apart a first prompt's history line
	// and its transcript record may be: the record's time has whole
	// seconds.
	antigravityFirstSlack = 3000
)

var antigravityHistories = struct {
	sync.Mutex
	m map[string]*antigravityHistory
}{m: map[string]*antigravityHistory{}}

func antigravityHistoryOf(path string) *antigravityHistory {
	antigravityHistories.Lock()
	defer antigravityHistories.Unlock()
	h := antigravityHistories.m[path]
	if h == nil {
		h = &antigravityHistory{path: path, byID: map[string]string{}}
		antigravityHistories.m[path] = h
	}
	return h
}

// refresh reads what was appended since the last read; a file that shrank
// was rewritten and is read again. Lines longer than forLines' buffer (a
// huge pasted prompt, which comes first in the line) are skipped. Called
// with h.mu held.
func (h *antigravityHistory) refresh() {
	info, err := os.Stat(h.path)
	if err != nil {
		return
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	if size == h.size && mtime == h.mtime {
		return
	}
	if size < h.scanned {
		h.scanned, h.byID, h.firsts = 0, map[string]string{}, nil
	}
	h.size, h.mtime = size, mtime
	forLines(h.path, h.scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		var l struct {
			Display        string `json:"display"`
			Timestamp      int64  `json:"timestamp"`
			Workspace      string `json:"workspace"`
			ConversationID string `json:"conversationId"`
			Type           string `json:"type"`
		}
		if json.Unmarshal(line, &l) != nil || l.Workspace == "" || l.Type == "slash_command" {
			return true
		}
		switch {
		case l.ConversationID != "":
			if h.byID[l.ConversationID] == "" {
				h.byID[l.ConversationID] = l.Workspace
			}
		case l.Timestamp > 0:
			h.firsts = append(h.firsts, antigravityFirst{l.Timestamp, l.Workspace, clip(strings.TrimSpace(l.Display), maxAntigravityDisplay)})
		}
		return true
	}, &h.scanned)
}

// firstPromptCwd is the workspace of the history line of a conversation's
// first prompt: the line without a conversation id sent within
// antigravityFirstSlack of it with the same text, else the nearest such
// line within a second. Called with h.mu held.
func (h *antigravityHistory) firstPromptCwd(p antigravityPrompt) string {
	if p.at == 0 {
		return ""
	}
	text := clip(p.text, maxAntigravityDisplay)
	best, bestGap := "", int64(1000)+1
	for _, f := range h.firsts {
		gap := f.at - p.at
		if gap < 0 {
			gap = -gap
		}
		if gap > antigravityFirstSlack {
			continue
		}
		if f.display == text {
			return f.workspace
		}
		if gap < bestGap {
			best, bestGap = f.workspace, gap
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Records

type antigravityRecord struct {
	StepIndex int    `json:"step_index"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
	Error     string `json:"error"`
	ToolCalls []struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"tool_calls"`
	// Media are the images of a step, as files (a tool result's: an image
	// read).
	Media []struct {
		MimeType string `json:"mime_type"`
		URI      string `json:"uri"`
	} `json:"media"`
}

func (r antigravityRecord) images() []Image {
	var out []Image
	for _, m := range r.Media {
		if imageMediaType(m.MimeType) {
			out = append(out, urlImage(m.MimeType, m.URI))
		}
	}
	return out
}

// parseAntigravity reads the user's prompts, the model's texts (not its
// thinking) and tool calls, its questions and their answers (ask_question,
// a question row and its answer row), the results of the other tool calls
// (the step after the call's: a call's id is its result's step), the tool
// calls that failed or were turned down (the first line of their error)
// and model errors. Antigravity CLI's notes are skipped.
func parseAntigravity(line []byte) []Entry {
	var r antigravityRecord
	if json.Unmarshal(line, &r) != nil {
		return nil
	}
	t := parseTime(r.CreatedAt)
	switch r.Type {
	case "USER_INPUT":
		images := r.images()
		if s := antigravityUserText(r.Content); s != "" || len(images) > 0 {
			return []Entry{{Role: RoleUser, Text: s, Images: images, Time: t}}
		}
	case "PLANNER_RESPONSE":
		var out []Entry
		if s := strings.TrimSpace(r.Content); s != "" {
			out = append(out, Entry{Role: RoleAssistant, Text: s, Time: t})
		}
		for i, c := range r.ToolCalls {
			id := antigravityCallID(r.StepIndex + 1 + i)
			if c.Name == antigravityQuestionTool {
				if e, ok := antigravityAskQuestion(id, c.Args, t); ok {
					out = append(out, e)
					continue
				}
			}
			out = append(out, toolCall(c.Name, id, rawInput(c.Args), Summarize(c.Args), t))
		}
		return out
	case "GENERIC":
		id, text := antigravityCallID(r.StepIndex), antigravityOutput(r.Content)
		switch r.Status {
		case "ERROR":
			out := toolOutput("", id, firstNonEmpty(text, r.Error), r.images(), t)
			if s := firstLine(firstNonEmpty(r.Error, text)); s != "" {
				out = append(out, Entry{Role: RoleSystem, Text: s, Time: t})
			}
			return out
		case "DONE":
			if e, ok := antigravityAskAnswer(id, text, t); ok {
				return []Entry{e}
			}
		}
		return toolOutput("", id, text, r.images(), t)
	case "ERROR_MESSAGE":
		if s := firstLine(firstNonEmpty(r.Error, r.Content)); s != "" {
			return []Entry{{Role: RoleSystem, Text: s, Time: t}}
		}
	}
	return nil
}

// antigravityCallID is the id of the tool call answered at step: its
// question row and its answer row share it.
func antigravityCallID(step int) string { return strconv.Itoa(step) }

const (
	antigravityRequestOpen  = "<USER_REQUEST>"
	antigravityRequestClose = "</USER_REQUEST>"
)

// antigravityUserText is what the user typed in a USER_INPUT's content: the
// inside of <USER_REQUEST>, else the content when it is no block of
// Antigravity CLI's.
func antigravityUserText(content string) string {
	if i := strings.Index(content, antigravityRequestOpen); i >= 0 {
		s := content[i+len(antigravityRequestOpen):]
		if j := strings.LastIndex(s, antigravityRequestClose); j >= 0 {
			s = s[:j]
		}
		return strings.TrimSpace(s)
	}
	if s := strings.TrimSpace(content); !strings.HasPrefix(s, "<") {
		return s
	}
	return ""
}

// antigravityOutput is a tool result's output: its content without the
// leading Created At / Completed At lines.
func antigravityOutput(content string) string {
	for {
		line, rest, found := strings.Cut(content, "\n")
		if !strings.HasPrefix(line, "Created At: ") && !strings.HasPrefix(line, "Completed At: ") {
			return strings.TrimSpace(content)
		}
		if !found {
			return ""
		}
		content = rest
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// antigravityAskQuestion reads the question row of an ask_question call:
// {questions: [{question, options: [<label>], is_multi_select}]}.
func antigravityAskQuestion(id string, args json.RawMessage, t int64) (Entry, bool) {
	var in struct {
		Questions []struct {
			Question    string   `json:"question"`
			Options     []string `json:"options"`
			MultiSelect bool     `json:"is_multi_select"`
		} `json:"questions"`
	}
	if json.Unmarshal(args, &in) != nil || len(in.Questions) == 0 {
		return Entry{}, false
	}
	items := make([]wire.QuestionItem, len(in.Questions))
	for i, q := range in.Questions {
		items[i] = wire.QuestionItem{Question: q.Question, Multi: q.MultiSelect, Options: q.Options}
	}
	return askQuestionRow(antigravityQuestionTool, id, rawInput(args), items, t), true
}

var antigravityAnswerLine = regexp.MustCompile(`^A([0-9]+): ?(.*)$`)

// antigravitySkipped is the answer of a question the user skipped (Esc).
const antigravitySkipped = "User Skipped"

// antigravityAskAnswer reads the answer row from a tool result's output, if
// it is ask_question's: one "A<n>: <answer>" line per question, in order
// (a multi-select's options joined by ", ", as Antigravity CLI writes
// them); a line that is not one continues the answer before it (a typed
// answer of several lines). The result names no tool, so the shape alone
// tells. A question skipped has no answer; all of them skipped close the
// question unanswered.
func antigravityAskAnswer(id, out string, t int64) (Entry, bool) {
	if !strings.HasPrefix(out, "A1:") {
		return Entry{}, false
	}
	var answers []string
	for _, l := range strings.Split(out, "\n") {
		m := antigravityAnswerLine.FindStringSubmatch(l)
		if m == nil || m[1] != strconv.Itoa(len(answers)+1) {
			answers[len(answers)-1] += "\n" + l
			continue
		}
		answers = append(answers, m[2])
	}
	items := make([]wire.QuestionItem, len(answers))
	answered := false
	for i, a := range answers {
		if a = strings.TrimSpace(a); a != antigravitySkipped {
			items[i].Answers = typedAnswers(nil, a)
			answered = answered || len(items[i].Answers) > 0
		}
	}
	if !answered {
		return askDeclinedRow(id, t), true
	}
	return askAnswerRow(id, items, t), true
}
