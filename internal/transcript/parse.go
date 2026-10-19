// Package transcript reads the conversation files coding agents write — a
// JSONL format per harness — and normalizes them to wire.Message.
// Files are only ever read: backwards from the end for paging, forwards from
// a remembered offset for tailing, never whole unless a cache entry is new.
package transcript

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/wire"
)

// Roles of wire.Message.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
	RoleSystem    = "system"
)

const (
	// maxText bounds one message's text; the app shows conversations, not
	// file dumps.
	maxText = 32 << 10
	// maxSummary bounds a tool call's one-line summary.
	maxSummary = 200
)

// ParseLine normalizes one JSONL record of harness to zero or more messages
// (in order). Unknown or noise records yield none.
func ParseLine(harness string, line []byte) []wire.Message {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil
	}
	f, ok := formats[harness]
	if !ok {
		return nil
	}
	return f.parse(line)
}

// ---------------------------------------------------------------------------
// Shared helpers

var (
	patchFile   = regexp.MustCompile(`(?m)^\*\*\* (Add|Update|Delete) File: (.+)$`)
	codeModeCmd = regexp.MustCompile(`\bcmd\s*:\s*("(?:[^"\\]|\\.)*")`)
)

// summarizeFreeform summarizes a free-form tool input (apply_patch patches,
// code-mode JavaScript).
func summarizeFreeform(name, input string) string {
	if ms := patchFile.FindAllStringSubmatch(input, -1); ms != nil {
		files := make([]string, 0, len(ms))
		for _, m := range ms {
			files = append(files, m[1]+" "+strings.TrimSpace(m[2]))
		}
		return oneLine(strings.Join(files, ", "), maxSummary)
	}
	if m := codeModeCmd.FindStringSubmatch(input); m != nil {
		if s, err := strconv.Unquote(m[1]); err == nil {
			return oneLine(s, maxSummary)
		}
	}
	for _, l := range strings.Split(input, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return oneLine(l, maxSummary)
		}
	}
	return ""
}

// summaryKeys are tool argument keys in the order they best describe a call.
var summaryKeys = []string{
	"command", "cmd", "file_path", "notebook_path", "path", "pattern", "query",
	"url", "description", "prompt", "skill", "task", "intent", "i",
}

// Summarize renders tool arguments (a JSON object, or a JSON string holding
// one) as a single short line: the command, file path, pattern, … whichever
// comes first in summaryKeys.
func Summarize(input json.RawMessage) string {
	input = bytes.TrimSpace(input)
	if len(input) == 0 {
		return ""
	}
	if input[0] == '"' {
		var s string
		if json.Unmarshal(input, &s) != nil {
			return ""
		}
		input = json.RawMessage(s)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil {
		return ""
	}
	for _, k := range summaryKeys {
		v, ok := args[k]
		if !ok {
			continue
		}
		if s, ok := jsonString(v); ok && strings.TrimSpace(s) != "" {
			if strings.HasPrefix(strings.TrimSpace(s), "*** Begin Patch") {
				return summarizeFreeform("apply_patch", s) // Codex apply_patch approvals
			}
			return oneLine(s, maxSummary)
		}
		var list []string
		if json.Unmarshal(v, &list) == nil && len(list) > 0 {
			return oneLine(strings.Join(list, " "), maxSummary)
		}
	}
	return ""
}

func jsonString(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// oneLine collapses whitespace and clips to max runes.
func oneLine(s string, max int) string {
	return clip(strings.Join(strings.Fields(s), " "), max)
}

// clip shortens s to at most max bytes on a rune boundary, marking the cut.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}
