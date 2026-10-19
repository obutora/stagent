package hook

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// codexNotify is the event of Codex's notify program (the legacy
// integration), which gets the JSON as its last argument.
const codexNotify = "notify"

var codexHook = hookHarness{
	argv: func(rest []string) (event string, payload []byte, ok bool) {
		if len(rest) == 0 || rest[0] != codexNotify {
			return "", nil, false
		}
		if len(rest) > 1 {
			payload = []byte(rest[len(rest)-1])
		}
		return codexNotify, payload, true
	},
	launch: func(li *launchInfo, payload []byte) {
		li.originator, li.subagent = codexOrigin(payload)
	},
}

// codexOrigin reads how the Codex conversation of payload was started from
// the first line of its rollout file (session_meta): originator (codex-tui
// for the terminal UI, codex_exec for `codex exec`, "Codex Desktop" for the
// desktop app) and whether it is a subagent thread of another one. The
// file is payload's transcript_path, else found by the notify program's
// thread-id under $CODEX_HOME/sessions.
func codexOrigin(payload []byte) (originator string, subagent bool) {
	var pl struct {
		TranscriptPath string `json:"transcript_path"`
		ThreadID       string `json:"thread-id"`
	}
	if json.Unmarshal(payload, &pl) != nil {
		return "", false
	}
	path := pl.TranscriptPath
	if path == "" && pl.ThreadID != "" && !strings.ContainsAny(pl.ThreadID, `/\*?[`) {
		path = findRollout(codexHome(), pl.ThreadID)
	}
	if path == "" {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	return parseSessionMeta(io.LimitReader(f, maxSessionMeta))
}

// maxSessionMeta bounds the first line read (it carries the instructions).
const maxSessionMeta = 4 << 20

func parseSessionMeta(r io.Reader) (originator string, subagent bool) {
	var line struct {
		Type    string `json:"type"`
		Payload struct {
			Originator string          `json:"originator"`
			Source     json.RawMessage `json:"source"`
		} `json:"payload"`
	}
	if json.NewDecoder(r).Decode(&line) != nil || line.Type != "session_meta" {
		return "", false
	}
	var src map[string]json.RawMessage
	if json.Unmarshal(line.Payload.Source, &src) == nil {
		_, subagent = src["subagent"]
	}
	return line.Payload.Originator, subagent
}

// codexHome is Codex's transcript root (the registry's): $CODEX_HOME, else
// ~/.codex; "" when neither can be told.
func codexHome() string {
	home, err := os.UserHomeDir()
	if err != nil && os.Getenv("CODEX_HOME") == "" {
		return ""
	}
	h, _ := harness.Lookup(wire.HarnessCodex)
	return h.TranscriptRoot(home, os.Getenv)
}

// findRollout finds sessions/YYYY/MM/DD/rollout-…-<thread>.jsonl (the
// newest when several match).
func findRollout(home, thread string) string {
	if home == "" {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}
