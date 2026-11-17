package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// openCodeReader reads stagent's records of OpenCode sessions
// (transcript/opencode.go): <root>/<session id>.jsonl. Each finished
// assistant message (a reply of the model, one step) carries the tokens and
// cost of its request; the session record names the session and its
// directory.
var openCodeReader = &reader{
	files: func(root string) ([]string, error) {
		entries, err := os.ReadDir(root)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		var out []string
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, filepath.Join(root, e.Name()))
			}
		}
		return out, nil
	},
	parse: parseOpenCode,
}

var (
	openCodeSessionRecord = []byte(`{"type":"session"`)
	openCodeTokens        = []byte(`"tokens"`)
)

// openCodeLine is the part of a line usage is read from.
type openCodeLine struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Directory string `json:"directory"`
	Time      struct {
		Completed int64 `json:"completed"`
	} `json:"time"`
	Model struct {
		ID string `json:"id"`
	} `json:"model"`
	Cost   float64 `json:"cost"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
}

// parseOpenCode reads a finished reply's tokens as one request: its input
// (without the cached part), its output and reasoning as output, the cache
// read and written, and its cost. A reply that spent nothing is no request.
func parseOpenCode(st *lineState, line []byte) (record, bool) {
	if bytes.HasPrefix(line, openCodeSessionRecord) {
		var l openCodeLine
		if json.Unmarshal(line, &l) == nil {
			st.Session, st.Project = l.ID, l.Directory
		}
		return record{}, false
	}
	if !bytes.Contains(line, openCodeTokens) {
		return record{}, false
	}
	var l openCodeLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Tokens == nil || l.ID == "" || l.Time.Completed == 0 {
		return record{}, false
	}
	u := l.Tokens
	in, out := max(u.Input, 0), max(u.Output, 0)+max(u.Reasoning, 0)
	read, write := max(u.Cache.Read, 0), max(u.Cache.Write, 0)
	cost := int64(math.Round(max(l.Cost, 0) * 1e6))
	if in == 0 && out == 0 && read == 0 && write == 0 && cost == 0 {
		return record{}, false
	}
	return record{
		key:     "opencode:" + l.ID,
		at:      l.Time.Completed,
		model:   l.Model.ID,
		project: st.Project,
		session: st.Session,
		tokens:  tokens{in, out, read, write, 0, cost},
	}, true
}
