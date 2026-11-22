package usage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"time"

	"github.com/obutora/stagent/internal/transcript"
)

// antigravityReader reads Antigravity CLI's conversations:
// <home>/brain/<conversation id>/.system_generated/logs/
// transcript_full.jsonl (transcript/antigravity.go), not the truncated
// transcript.jsonl beside it nor its chunks. Each PLANNER_RESPONSE record
// (a reply of the model) carries the usage of its request; the path names
// the conversation, history.jsonl its directory.
var antigravityReader = &reader{
	files: func(root string) ([]string, error) {
		return filepath.Glob(filepath.Join(root, "brain", "*", ".system_generated", "logs", "transcript_full.jsonl"))
	},
	parse: parseAntigravity,
	start: func(path string) lineState {
		return lineState{
			Session: filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))),
			Project: transcript.AntigravityCwd(path),
		}
	},
}

var antigravityPlanner = []byte(`"PLANNER_RESPONSE"`)

// antigravityLine is the part of a record usage is read from.
type antigravityLine struct {
	StepIndex *int   `json:"step_index"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Model     string `json:"model"`
	Input     int64  `json:"input_tokens"`
	CacheRead int64  `json:"cache_read_tokens"`
	Output    int64  `json:"output_tokens"`
}

// parseAntigravity reads a reply's usage as one request: input_tokens is
// the input without its cached part, cache_read_tokens, counted as cache
// reads (in Antigravity CLI 1.5.14's conversations the cache reads are
// mostly larger than input_tokens, which they could not be were they part
// of it, and input plus cache reads never shrinks from one reply to the
// next: the whole prompt, grown by what the conversation added);
// output_tokens is the reply [INFERENCE: its thinking included, no count
// of its own is written]. A reply that spent nothing is no request. The
// step index names the request within its conversation.
func parseAntigravity(st *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, antigravityPlanner) {
		return record{}, false
	}
	var l antigravityLine
	if json.Unmarshal(line, &l) != nil || l.Type != "PLANNER_RESPONSE" || l.StepIndex == nil {
		return record{}, false
	}
	t := tokens{max(l.Input, 0), max(l.Output, 0), max(l.CacheRead, 0), 0, 0, 0}
	if t == (tokens{}) {
		return record{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, l.CreatedAt)
	if err != nil {
		return record{}, false
	}
	return record{
		key:     "antigravity:" + st.Session + ":" + strconv.Itoa(*l.StepIndex),
		at:      at.UnixMilli(),
		model:   l.Model,
		project: st.Project,
		session: st.Session,
		tokens:  t,
	}, true
}
