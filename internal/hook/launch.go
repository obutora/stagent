package hook

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/ptable"
)

// Windows: the hook command runs under the harness, possibly through a
// shell of its own (Claude Code runs hooks with Git Bash; other hooks are
// PowerShell or cmd one-liners). The harness is the nearest ancestor that
// is not such a shell; a terminal's shell above it tells a launch from a
// terminal apart from one by an app that starts the harness itself (the
// Codex desktop app, or any other program).

// winShells are the shells looked past and looked for, by image name
// without ".exe", lower case.
var winShells = map[string]bool{"powershell": true, "pwsh": true, "cmd": true, "bash": true, "sh": true}

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 32

func imageName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

// winAncestry finds the harness among the ancestors of self in s and
// whether a shell (PowerShell, pwsh, cmd, bash, sh) runs above it. harness
// is 0 when the parent cannot be read.
func winAncestry(s *ptable.Snapshot, self int) (harness int, terminal bool) {
	p := s.Get(self)
	seen := map[int]bool{self: true}
	for range maxAncestors {
		if p == nil || p.PPID <= 0 || seen[p.PPID] {
			return harness, false
		}
		seen[p.PPID] = true
		p = s.Get(p.PPID)
		if p == nil {
			return harness, false
		}
		if harness == 0 {
			if !winShells[imageName(p.Name)] {
				harness = p.PID
			}
			continue
		}
		if winShells[imageName(p.Name)] {
			return harness, true
		}
	}
	return harness, false
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

func codexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
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
