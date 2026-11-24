package transcript

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// RoleToolOutput: an Entry holding one tool call's output (never a
// wire.Message).
const RoleToolOutput = "tool_output"

// Entry is one entry of a conversation as the harness recorded it, before
// it is shaped for the chat view (wire.Message) or the relay document.
// Each harness's parser reads its records as entries; messages shapes them
// for the chat view in one place.
type Entry struct {
	Role string // RoleUser | RoleAssistant | RoleTool (a tool call) | RoleSystem | RoleToolOutput
	// Text is whole, never clipped: user/assistant/system text; a tool
	// output's text; a tool call's one-line summary (what the chat row
	// shows).
	Text     string
	ToolName string // tool call; tool output when known
	CallID   string // tool call id (call and output share it) when the harness records one
	// Input is a tool call's whole input as recorded (JSON text, or a
	// free-form patch/code).
	Input  string
	Images []Image // images carried by a user/assistant/tool-output entry
	// Touched: the paths a file-changing tool call names, as written in
	// its input.
	Touched []string
	Time    int64 // unix ms
	Ask     *wire.Ask
}

// Image is one image of an entry: Data (decoded) when the transcript holds
// it, else Path when the transcript only names a file.
type Image struct {
	MediaType string // image/png, image/jpeg, image/gif, image/webp ("" unknown)
	Data      []byte
	Path      string
}

// imageRow is what the chat view shows for an image.
const imageRow = "[image]"

// message shapes e for the chat view: tool outputs are not shown, an image
// shows as imageRow after the text, the text is clipped to maxText.
func (e Entry) message() (wire.Message, bool) {
	if e.Role == RoleToolOutput {
		return wire.Message{}, false
	}
	text := e.Text
	if e.Role != RoleTool {
		for range e.Images {
			if text != "" {
				text += "\n\n"
			}
			text += imageRow
		}
	}
	return wire.Message{Role: e.Role, ToolName: e.ToolName, Text: clip(text, maxText), Time: e.Time, Ask: e.Ask}, true
}

// messages shapes entries for the chat view (see Entry.message).
func messages(es []Entry) []wire.Message {
	var out []wire.Message
	for _, e := range es {
		if m, ok := e.message(); ok {
			out = append(out, m)
		}
	}
	return out
}

// maxEntryLine skips records larger than this in ReadEntries. It is above
// maxLine so that the document keeps the images transcripts hold inline
// (base64 of the largest image it carries).
const maxEntryLine = 64 << 20

// ReadEntries reads harness's transcript at path from its start up to end
// (bytes; end <= 0 or past the file: the whole file) as entries, complete
// records only as ReadFrom reads them: thinking/reasoning, sub-agents'
// (sidechain) records and harness noise are left out — the same rows
// transcript.get leaves out — and tool outputs are in.
func ReadEntries(path, harness string, end int64) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if end <= 0 || end > st.Size() {
		end = st.Size()
	}
	end = completeEnd(f, end)
	r := bufio.NewReaderSize(io.NewSectionReader(f, 0, end), readChunk)
	lk := &questionLookup{f: f, harness: harness}
	var out []Entry
	var line []byte
	var start, n int64 // the line's offset and its length so far
	over := false      // the line passed maxEntryLine: skipped
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !over && len(line)+len(chunk) > maxEntryLine {
			over, line = true, nil
		}
		if !over {
			line = append(line, chunk...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if !over && len(line) > 0 {
			lk.start = start
			out = append(out, parseEntries(harness, line, lk.asked)...)
		}
		if err != nil { // io.EOF
			return out, nil
		}
		start += n
		n, line, over = 0, line[:0], false
	}
}

// ---------------------------------------------------------------------------
// Tool calls and outputs

// toolCall is the entry of a tool call: its summary as the text, its whole
// input, the paths it changes.
func toolCall(name, id, input, summary string, t int64) Entry {
	return Entry{Role: RoleTool, ToolName: name, CallID: id, Input: input, Text: summary, Touched: touchedPaths(name, input), Time: t}
}

// toolOutput is the entry of a tool call's output; none when it holds
// neither text nor images.
func toolOutput(name, id, text string, images []Image, t int64) []Entry {
	text = strings.TrimSpace(text)
	if text == "" && len(images) == 0 {
		return nil
	}
	return []Entry{{Role: RoleToolOutput, ToolName: name, CallID: id, Text: text, Images: images, Time: t}}
}

// rawInput is a JSON tool input as its text.
func rawInput(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	return string(raw)
}

// fileTools are the file-changing tools of the harnesses (lower case)
// whose input names the path: Claude Code's Edit, Write, MultiEdit,
// NotebookEdit; omp's and Pi's edit, write, ast_edit; OpenCode's edit,
// write, multiedit; Qwen Code's replace, write_file, edit; Copilot CLI's
// edit, create; Grok Build's write, edit, search_replace; Antigravity
// CLI's write_file, edit_file (and its kin's replace_file_content,
// write_to_file). apply_patch and patch name theirs in the patch.
var fileTools = map[string]bool{
	"edit": true, "write": true, "multiedit": true, "multi_edit": true, "notebookedit": true,
	"ast_edit": true, "create": true, "replace": true, "write_file": true, "edit_file": true,
	"search_replace": true, "str_replace": true, "str_replace_editor": true, "str_replace_based_edit_tool": true,
	"replace_file_content": true, "multi_replace_file_content": true, "write_to_file": true,
	"create_file": true, "delete_file": true,
}

// pathKeys are the input keys that name a file-changing tool's path, in
// the order looked for.
var pathKeys = []string{
	"file_path", "filePath", "notebook_path", "path", "Path", "TargetFile",
	"target_file", "File", "file", "absolute_path",
}

// patchPath matches the lines of an apply_patch patch that name a file.
var patchPath = regexp.MustCompile(`(?m)^\*\*\* (?:Add File|Update File|Delete File|Move to): (.+?)\s*$`)

// touchedPaths are the paths a tool call changes, as its input names them:
// those of an apply_patch patch (free-form, or a string inside the JSON
// input, such as a shell command's heredoc), and the path argument of a
// file-changing tool (fileTools).
func touchedPaths(name, input string) []string {
	var out []string
	add := func(p string) {
		if p = strings.TrimSpace(p); p != "" && !containsString(out, p) {
			out = append(out, p)
		}
	}
	if strings.Contains(input, "*** ") {
		for _, s := range patchTexts(input) {
			for _, m := range patchPath.FindAllStringSubmatch(s, -1) {
				add(m[1])
			}
		}
	}
	if fileTools[strings.ToLower(name)] {
		add(inputPath(input))
	}
	return out
}

// inputPath is the path a JSON tool input names (pathKeys), "" if none.
func inputPath(input string) string {
	raw := json.RawMessage(strings.TrimSpace(input))
	if s, ok := jsonString(raw); ok {
		raw = json.RawMessage(s)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(raw, &args) != nil {
		return ""
	}
	for _, k := range pathKeys {
		if s, ok := jsonString(args[k]); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// patchTexts are the texts of input a patch may be in: its string values
// when it is JSON, else input itself.
func patchTexts(input string) []string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed[0] != '{' && trimmed[0] != '"' && trimmed[0] != '[' {
		return []string{input}
	}
	var v any
	if json.Unmarshal([]byte(trimmed), &v) != nil {
		return []string{input}
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case string:
			if strings.Contains(v, "*** ") {
				out = append(out, v)
			}
			if t := strings.TrimSpace(v); t != "" && (t[0] == '{' || t[0] == '[') && json.Valid([]byte(t)) {
				out = append(out, patchTexts(t)...) // a JSON string holding JSON (Codex arguments)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		case map[string]any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(v)
	return out
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Images

// base64Image is an image the transcript holds as base64. Data that does
// not decode is left out (the image is still counted).
func base64Image(mediaType, data string) Image {
	img := Image{MediaType: strings.ToLower(strings.TrimSpace(mediaType))}
	data = strings.TrimSpace(data)
	if data == "" {
		return img
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(data); err == nil {
			img.Data = b
			break
		}
	}
	return img
}

// urlImage is an image the transcript names by URL: a data: URL holds it,
// a file: URL or a path names a file.
func urlImage(mediaType, u string) Image {
	u = strings.TrimSpace(u)
	if rest, ok := strings.CutPrefix(u, "data:"); ok {
		meta, data, _ := strings.Cut(rest, ",")
		mt, _, _ := strings.Cut(meta, ";")
		if mediaType == "" {
			mediaType = mt
		}
		if strings.HasSuffix(meta, ";base64") {
			return base64Image(mediaType, data)
		}
		s, err := url.PathUnescape(data)
		if err != nil {
			s = data
		}
		return Image{MediaType: strings.ToLower(mediaType), Data: []byte(s)}
	}
	if strings.HasPrefix(u, "file://") {
		if p, err := url.Parse(u); err == nil {
			u = p.Path
		}
	}
	return Image{MediaType: strings.ToLower(mediaType), Path: u}
}

// imageMediaType reports whether mediaType is an image's.
func imageMediaType(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(mediaType), "image/")
}
