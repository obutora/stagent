package usage

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// grokReader reads Grok Build's sessions: <sessions>/<cwd,
// percent-encoded>/<session id>/updates.jsonl (transcript/grok.go). Each
// turn ends with a turn_completed update holding the turn's usage by
// model; the path names the session and its directory.
var grokReader = &reader{
	files: func(root string) ([]string, error) {
		files, err := filepath.Glob(filepath.Join(root, "*", "*", "updates.jsonl"))
		if err != nil {
			return nil, err
		}
		return files, nil
	},
	parse: parseGrok,
	start: func(path string) lineState {
		dir := filepath.Dir(path)
		return lineState{Session: filepath.Base(dir), Project: grokCwd(filepath.Dir(dir))}
	},
}

// grokCwd is the working directory of the sessions kept in group: its name
// decoded, or the .cwd file of a group named by a slug and a hash.
func grokCwd(group string) string {
	if s, err := url.PathUnescape(filepath.Base(group)); err == nil && (strings.HasPrefix(s, "/") || len(s) > 2 && s[1] == ':') {
		return s
	}
	if b, err := os.ReadFile(filepath.Join(group, ".cwd")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

var grokTurnCompleted = []byte(`"turn_completed"`)

// grokLine is the part of a turn_completed update usage is read from.
type grokLine struct {
	Timestamp float64 `json:"timestamp"`
	Params    struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			PromptID      string `json:"prompt_id"`
			Usage         struct {
				grokUsage
				ModelUsage map[string]grokUsage `json:"modelUsage"`
			} `json:"usage"`
		} `json:"update"`
		Meta struct {
			AgentTimestampMs int64 `json:"agentTimestampMs"`
		} `json:"_meta"`
	} `json:"params"`
}

type grokUsage struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CachedRead    int64 `json:"cachedReadTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	Reasoning     int64 `json:"reasoningTokens"`
	Total         int64 `json:"totalTokens"`
}

// parseGrok reads a turn's usage as one request [INFERENCE: the input
// holds the cached part, as xAI's API counts it, which is taken out of it
// and counted as cache reads]; the reasoning is output. The model is the
// one that spent the most of the turn's tokens (modelUsage). A turn that
// spent nothing is no request.
func parseGrok(st *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, grokTurnCompleted) {
		return record{}, false
	}
	var l grokLine
	u := &l.Params.Update
	if json.Unmarshal(line, &l) != nil || u.SessionUpdate != "turn_completed" || u.PromptID == "" {
		return record{}, false
	}
	m := u.Usage.grokUsage
	cached := min(max(m.CachedRead, 0), max(m.Input, 0))
	t := tokens{max(m.Input, 0) - cached, max(m.Output, 0) + max(m.Reasoning, 0), cached, max(m.CacheCreation, 0), 0, 0}
	if t == (tokens{}) {
		return record{}, false
	}
	var model string
	var most int64 = -1
	for name, mu := range u.Usage.ModelUsage {
		if mu.Total > most || mu.Total == most && name < model {
			model, most = name, mu.Total
		}
	}
	at := l.Params.Meta.AgentTimestampMs
	if at == 0 {
		at = int64(l.Timestamp * 1000)
	}
	return record{
		key:     "grok:" + st.Session + ":" + u.PromptID,
		at:      at,
		model:   model,
		project: st.Project,
		session: st.Session,
		tokens:  t,
	}, true
}
