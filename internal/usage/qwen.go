package usage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// qwenReader reads Qwen Code's conversations:
// <projects>/<project>/chats/<session id>.jsonl, the ledgers beside them
// (.ledger.jsonl) left out. Each assistant record (a reply of the model)
// carries the Gemini usageMetadata of its request.
var qwenReader = &reader{
	files: func(root string) ([]string, error) {
		files, err := listJSONL(root)
		out := files[:0]
		for _, p := range files {
			if filepath.Base(filepath.Dir(p)) == "chats" && !strings.HasSuffix(p, ".ledger.jsonl") {
				out = append(out, p)
			}
		}
		return out, err
	},
	parse: parseQwen,
}

var qwenUsage = []byte(`"usageMetadata"`)

// qwenLine is the part of a record usage is read from.
type qwenLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Model     string `json:"model"`
	Usage     *struct {
		Prompt     int64 `json:"promptTokenCount"`
		Candidates int64 `json:"candidatesTokenCount"`
		Thoughts   int64 `json:"thoughtsTokenCount"`
		Cached     int64 `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

// parseQwen reads an assistant record's usageMetadata as one request: the
// prompt less its cached part is the input, the cached part cache reads,
// the reply and the thoughts the output. A reply that spent nothing is no
// request. A resumed or forked conversation keeps the records' uuids.
func parseQwen(_ *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, qwenUsage) {
		return record{}, false
	}
	var l qwenLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Usage == nil || l.UUID == "" {
		return record{}, false
	}
	u := l.Usage
	cached := min(max(u.Cached, 0), max(u.Prompt, 0))
	in, out := max(u.Prompt, 0)-cached, max(u.Candidates, 0)+max(u.Thoughts, 0)
	if in == 0 && out == 0 && cached == 0 {
		return record{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return record{}, false
	}
	return record{
		key:     "qwen:" + l.UUID,
		at:      at.UnixMilli(),
		model:   l.Model,
		project: l.Cwd,
		session: l.SessionID,
		tokens:  tokens{in, out, cached, 0, 0, 0},
	}, true
}
