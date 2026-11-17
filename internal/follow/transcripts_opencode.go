package follow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// OpenCode keeps its sessions in a database; their transcripts are
// stagent's records of them (transcript/opencode.go), which OpenCode never
// opens. Beside each record, <id>.proc names the OpenCode terminal UI that
// last reported the session and when it started: a session is found by
// the sidecar naming one of the agent's processes, else by the session its
// command line resumes (--session / -s), else as the newest session of its
// directory.
var openCodeFinder = finder{
	matcher:    func(agentFacts) func(string) (string, bool) { return func(string) (string, bool) { return "", false } },
	breadcrumb: openCodeProc,
	fromArgv:   openCodeFromArgv,
}

// openCodeProc returns the session whose process sidecar names one of the
// agent's processes, started when it did.
func openCodeProc(a agentFacts, _ func(string) (string, bool)) (string, bool) {
	root := a.roots[wire.HarnessOpenCode]
	if root == "" {
		return "", false
	}
	sidecars, _ := filepath.Glob(filepath.Join(root, "*.proc"))
	for _, s := range sidecars {
		b, err := os.ReadFile(s)
		if err != nil {
			continue
		}
		var p struct {
			PID     int   `json:"pid"`
			Started int64 `json:"started"`
		}
		if json.Unmarshal(b, &p) != nil || !slices.Contains(a.pids, p.PID) {
			continue
		}
		if d := time.UnixMilli(p.Started).Sub(a.start); d < -startSlack || d > startSlack {
			continue // a process of the same pid before this one
		}
		path := strings.TrimSuffix(s, ".proc") + ".jsonl"
		if !isFile(path) {
			return "", true
		}
		return path, true
	}
	return "", false
}

// openCodeResume reads the session an OpenCode command line continues:
// `--session[=]<id>`, `-s <id>`; resumed without an id with -c /
// --continue.
func openCodeResume(argv []string) (id string, resumed bool) {
	for i, a := range argv {
		switch {
		case a == "--session" || a == "-s":
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--session="):
			return strings.TrimPrefix(a, "--session="), true
		case a == "--continue" || a == "-c":
			return "", true
		}
	}
	return "", false
}

// openCodeFromArgv returns stagent's record of the session the agent's
// command line continues, once it is written.
func openCodeFromArgv(_ *transcripts, a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		argv := a.argv(pid)
		if len(argv) == 0 {
			continue
		}
		if id, resumed := openCodeResume(argv[1:]); resumed {
			if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
				return "", false
			}
			p := filepath.Join(a.roots[wire.HarnessOpenCode], id+".jsonl")
			if !isFile(p) {
				return "", false
			}
			return p, false
		}
	}
	return "", true
}
