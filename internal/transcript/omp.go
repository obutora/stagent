package transcript

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// omp: ~/.omp/agent/sessions/<project>/<timestamp>_<id>.jsonl

var ompFormat = format{
	dir: func(root string) string { return root },
	enumerate: func(dir string) ([]fileStat, error) {
		return listTwoLevels(dir, func(name string) bool { return strings.HasSuffix(name, ".jsonl") })
	},
	named:  func(base, id string) bool { return strings.HasSuffix(base, "_"+id+".jsonl") },
	scan:   scanOmp,
	titler: func(*Index) func(*entry) string { return ompEntryTitle },
	parse:  parseOmp,
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
		base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if i := strings.IndexByte(base, '_'); i >= 0 {
			e.ID = base[i+1:]
		}
	}
	// Line 1 changes in place; never scan incrementally.
	e.Scanned = 0
}

func ompEntryTitle(e *entry) string { return oneLine(e.Title, maxSummary) }

type ompRecord struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Intent    string          `json:"intent"`
		} `json:"content"`
		Attribution string `json:"attribution"`
		Timestamp   int64  `json:"timestamp"`
	} `json:"message"`
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
				s := Summarize(c.Arguments)
				if s == "" {
					s = oneLine(c.Intent, maxSummary)
				}
				out = append(out, wire.Message{Role: RoleTool, ToolName: c.Name, Text: s, Time: t})
			}
		}
		flush()
	}
	return out
}
