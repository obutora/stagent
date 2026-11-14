package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"time"
)

// ompReader reads the session files of omp and Pi (omp is a fork of Pi and
// keeps its format): <sessions>/<project>/<timestamp>_<id>.jsonl, and the
// sessions of sub-agents below them. Line 1 of an omp file (the title) is
// rewritten in place; the requests already counted stay counted once
// because their keys are the same.
var ompReader = &reader{
	files: listJSONL,
	parse: parseOmp,
}

var (
	ompSessionType = []byte(`"session"`)
	ompUsage       = []byte(`"usage"`)
)

// ompLine is the part of a session line usage is read from.
type ompLine struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	Message   *struct {
		Role  string `json:"role"`
		Model string `json:"model"`
		Usage *struct {
			Input      int64 `json:"input"`
			Output     int64 `json:"output"`
			CacheRead  int64 `json:"cacheRead"`
			CacheWrite int64 `json:"cacheWrite"`
			// The part of CacheWrite written with a 1-hour lifetime: Pi
			// writes cacheWrite1h, omp cttl.ephemeral1h.
			CacheWrite1h int64 `json:"cacheWrite1h"`
			Cttl         *struct {
				Ephemeral1h int64 `json:"ephemeral1h"`
			} `json:"cttl"`
			Cost *struct {
				Total float64 `json:"total"`
			} `json:"cost"`
		} `json:"usage"`
	} `json:"message"`
}

// parseOmp reads a session line: the session header gives the session and
// the project; each assistant message with usage is one request, with the
// cost the agent wrote for it. A message that spent nothing (a request
// that failed) is not a request.
func parseOmp(st *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, ompUsage) && !bytes.Contains(line, ompSessionType) {
		return record{}, false
	}
	var l ompLine
	if json.Unmarshal(line, &l) != nil {
		return record{}, false
	}
	if l.Type == "session" {
		st.Session, st.Project = l.ID, l.Cwd
		return record{}, false
	}
	if l.Type != "message" || l.Message == nil || l.Message.Role != "assistant" || l.Message.Usage == nil {
		return record{}, false
	}
	u := l.Message.Usage
	var cost int64
	if u.Cost != nil {
		cost = int64(math.Round(max(u.Cost.Total, 0) * 1e6))
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && cost == 0 {
		return record{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil || l.ID == "" {
		return record{}, false
	}
	write1h := u.CacheWrite1h
	if u.Cttl != nil {
		write1h = max(write1h, u.Cttl.Ephemeral1h)
	}
	write1h = min(max(write1h, 0), u.CacheWrite)
	return record{
		// A forked session copies the entries with their ids and times.
		key:     "omp:" + l.Timestamp + ":" + l.ID,
		at:      at.UnixMilli(),
		model:   l.Message.Model,
		project: st.Project,
		session: st.Session,
		tokens:  tokens{u.Input, u.Output, u.CacheRead, u.CacheWrite - write1h, write1h, cost},
	}, true
}
