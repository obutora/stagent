package follow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Claude Code is found by the session file it keeps per process.
var claudeFinder = finder{session: (*transcripts).claude}

// claudeStaleSlack: a session file written this long before its pid's
// process started was left by an earlier process with the same pid.
const claudeStaleSlack = 5 * time.Second

// claudeSession is <config dir>/sessions/<pid>.json, which Claude Code
// keeps per running process and rewrites when /clear or a resume switches
// conversations.
type claudeSession struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // unix ms
}

func (t *transcripts) claude(a agentFacts) (path, cwd string) {
	projects := a.roots[wire.HarnessClaude]
	dir := filepath.Join(filepath.Dir(projects), "sessions")
	for _, pid := range a.pids {
		b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)+".json"))
		if err != nil {
			continue
		}
		var cs claudeSession
		if json.Unmarshal(b, &cs) != nil || !validID(cs.SessionID) {
			continue
		}
		if cs.StartedAt > 0 && !a.start.IsZero() && time.UnixMilli(cs.StartedAt).Before(a.start.Add(-claudeStaleSlack)) {
			continue
		}
		return t.claudePath(projects, cs.SessionID), cs.Cwd
	}
	return "", ""
}

// claudePath finds projects/*/<id>.jsonl ("" until the conversation's first
// record is written).
func (t *transcripts) claudePath(projects, id string) string {
	key := projects + "\x00" + id
	if p, ok := t.claudePaths[key]; ok {
		if isFile(p) {
			return p
		}
		delete(t.claudePaths, key)
	}
	dirs, err := os.ReadDir(projects)
	if err != nil {
		return ""
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		if p := filepath.Join(projects, d.Name(), id+".jsonl"); isFile(p) {
			t.claudePaths[key] = p
			return p
		}
	}
	return ""
}

// validID accepts a session id only if it cannot leave the projects
// directory.
func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`)
}
