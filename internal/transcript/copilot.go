package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Copilot CLI: <copilot home>/session-state/<session id>/events.jsonl, an
// append-only log of session events, {"type","id","parentId","timestamp",
// "agentId"?,"ephemeral"?,"data"} (schemas/session-events.schema.json of the
// npm package; ephemeral events are not written). workspace.yaml beside it
// holds the session's cwd and name. Copilot CLI promises no stable format
// (github/copilot-cli#3551): records of a type not read here, and lines
// that do not parse, are skipped, and the conversation reads on.

var copilotFormat = format{
	dir: func(root string) string { return root },
	enumerate: func(dir string) ([]fileStat, error) {
		return listTwoLevels(dir, func(name string) bool { return name == copilotEvents })
	},
	named:  func(path, id string) bool { return filepath.Base(filepath.Dir(path)) == id },
	scan:   scanCopilot,
	titler: func(*Index) func(*entry) string { return titleOrPrompt },
	parse:  parseCopilot,
}

const (
	copilotEvents    = "events.jsonl"
	copilotWorkspace = "workspace.yaml"
	// maxWorkspace bounds the workspace.yaml read.
	maxWorkspace = 64 << 10
)

// scanCopilot reads the session's id and cwd (session.start) and first
// prompt from where the previous scan stopped, then its workspace.yaml:
// the cwd there is the session's current one, and its name (else its
// summary) titles it, as Copilot CLI's session list does.
func scanCopilot(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		switch {
		case e.ID == "" && bytes.Contains(line, []byte(`"session.start"`)):
			var r struct {
				Type string `json:"type"`
				Data struct {
					SessionID string `json:"sessionId"`
					Context   struct {
						Cwd string `json:"cwd"`
					} `json:"context"`
				} `json:"data"`
			}
			if json.Unmarshal(line, &r) == nil && r.Type == "session.start" {
				e.ID = r.Data.SessionID
				if e.Cwd == "" {
					e.Cwd = r.Data.Context.Cwd
				}
			}
		case e.Prompt == "":
			for _, m := range parseCopilot(line) {
				if m.Role == RoleUser {
					e.Prompt = clip(m.Text, 1024)
				}
			}
		}
		return true
	}, &e.Scanned)
	if e.ID == "" {
		e.ID = filepath.Base(filepath.Dir(path))
	}
	ws := readCopilotWorkspace(filepath.Join(filepath.Dir(path), copilotWorkspace))
	if ws["cwd"] != "" {
		e.Cwd = ws["cwd"]
	}
	e.Title = clip(strings.TrimSpace(firstNonEmpty(ws["name"], ws["summary"])), 1024)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// readCopilotWorkspace reads the top-level scalar fields of a
// workspace.yaml (plain, single- or double-quoted); block scalars and
// nested values are left out.
func readCopilotWorkspace(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(io.LimitReader(f, maxWorkspace))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if v, ok := yamlScalar(strings.TrimSpace(value)); ok {
			out[strings.TrimSpace(key)] = v
		}
	}
	return out
}

// yamlScalar reads a one-line YAML scalar.
func yamlScalar(v string) (string, bool) {
	switch {
	case v == "" || v == "|" || v == ">" || strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">"):
		return "", false
	case v[0] == '"':
		s, err := strconv.Unquote(v)
		return s, err == nil
	case v[0] == '\'':
		if len(v) < 2 || v[len(v)-1] != '\'' {
			return "", false
		}
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'"), true
	case v == "null" || v == "~":
		return "", false
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	return v, true
}

type copilotRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	// AgentID marks a sub-agent's event.
	AgentID string `json:"agentId"`
	Data    struct {
		Content         string `json:"content"`
		Source          string `json:"source"`
		ParentToolCall  string `json:"parentToolCallId"`
		AutopilotResume bool   `json:"isAutopilotContinuation"`
		ToolRequests    []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			ToolTitle string          `json:"toolTitle"`
		} `json:"toolRequests"`
	} `json:"data"`
}

// parseCopilot reads the user's prompts and the main agent's messages and
// tool calls. A sub-agent's messages (agentId, or parentToolCallId in
// older records), prompts Copilot CLI sends itself (a skill's hidden
// instructions `skill-…`, another agent's `agent-…`, autopilot's
// continuations) and every other record type are skipped.
func parseCopilot(line []byte) []wire.Message {
	if !bytes.Contains(line, []byte(`"user.message"`)) && !bytes.Contains(line, []byte(`"assistant.message"`)) {
		return nil
	}
	var r copilotRecord
	if json.Unmarshal(line, &r) != nil || r.AgentID != "" || r.Data.ParentToolCall != "" {
		return nil
	}
	t := parseTime(r.Timestamp)
	switch r.Type {
	case "user.message":
		if r.Data.AutopilotResume || strings.HasPrefix(r.Data.Source, "skill-") || strings.HasPrefix(r.Data.Source, "agent-") {
			return nil
		}
		if s := strings.TrimSpace(r.Data.Content); s != "" {
			return []wire.Message{{Role: RoleUser, Text: clip(s, maxText), Time: t}}
		}
	case "assistant.message":
		var out []wire.Message
		if s := strings.TrimSpace(r.Data.Content); s != "" {
			out = append(out, wire.Message{Role: RoleAssistant, Text: clip(s, maxText), Time: t})
		}
		for _, tr := range r.Data.ToolRequests {
			s := Summarize(tr.Arguments)
			if s == "" {
				s = oneLine(tr.ToolTitle, maxSummary)
			}
			out = append(out, wire.Message{Role: RoleTool, ToolName: tr.Name, Text: s, Time: t})
		}
		return out
	}
	return nil
}
