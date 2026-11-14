package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"time"
)

// claudeReader reads Claude Code's transcripts: every .jsonl below the
// projects directory, sub-agent runs (<project>/<session>/subagents/)
// included. Each assistant record carries the usage of the API response
// it is part of; a streamed response is written as one record per content
// block, all with the same message.id and requestId.
var claudeReader = &reader{
	files: listJSONL,
	parse: parseClaude,
}

// listJSONL lists the .jsonl files anywhere below root.
func listJSONL(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			if d != nil && d.IsDir() && p != root {
				return fs.SkipDir // unreadable directory: skip it
			}
			return err
		}
		if !d.IsDir() && d.Type().IsRegular() && strings.EqualFold(filepath.Ext(p), ".jsonl") {
			out = append(out, p)
		}
		return nil
	})
	return out, err
}

var (
	claudeAssistant = []byte(`"assistant"`)
	claudeUsage     = []byte(`"usage"`)
)

// claudeLine is the part of a transcript record usage is read from.
type claudeLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	RequestID string `json:"requestId"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"sessionId"`
	Message   struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			Output        int64 `json:"output_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			Detail        *struct {
				Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// parseClaude reads the request of one assistant record. Records of the
// <synthetic> model (a request that never reached the API) are not
// requests.
func parseClaude(_ *lineState, line []byte) (record, bool) {
	if !bytes.Contains(line, claudeAssistant) || !bytes.Contains(line, claudeUsage) {
		return record{}, false
	}
	var l claudeLine
	if json.Unmarshal(line, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil ||
		l.Message.Model == "" || l.Message.Model == "<synthetic>" {
		return record{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil {
		return record{}, false
	}
	key := l.Message.ID
	switch {
	case key != "" && l.RequestID != "":
		key += ":" + l.RequestID
	case key == "":
		key = l.UUID
	}
	if key == "" {
		return record{}, false
	}
	u := l.Message.Usage
	var write1h int64
	if u.Detail != nil {
		write1h = min(u.Detail.Ephemeral1h, u.CacheCreation)
	}
	return record{
		key:     key,
		at:      at.UnixMilli(),
		model:   l.Message.Model,
		project: l.Cwd,
		session: l.SessionID,
		tokens:  tokens{u.Input, u.Output, u.CacheRead, u.CacheCreation - write1h, write1h},
	}, true
}
