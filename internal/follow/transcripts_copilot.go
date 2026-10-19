package follow

import (
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Copilot CLI keeps a session in <copilot home>/session-state/<id>/: its
// event log events.jsonl and its other files (workspace.yaml, …). It is
// found by a file of that directory it has open, else by the session its
// command line resumes, else as the newest session of its directory.
var copilotFinder = finder{
	matcher:  copilotMatcher,
	fromArgv: (*transcripts).copilotFromArgv,
}

// copilotMatcher recognizes the files of a session directory below
// session-state and returns that session's event log.
func copilotMatcher(a agentFacts) func(string) (string, bool) {
	root := newSpelledDir(a.roots[wire.HarnessCopilot])
	return func(p string) (string, bool) {
		p, ok := root.below(p, 2)
		if !ok {
			return "", false
		}
		events := filepath.Join(filepath.Dir(p), "events.jsonl")
		return events, isFile(events)
	}
}

// copilotResume reads the session a Copilot CLI command line resumes:
// `--resume[=]<id>`, `-r <id>`, `--session-id[=]<id>`; resumed without an
// id for the picker or --continue.
func copilotResume(argv []string) (id string, resumed bool) {
	for i, a := range argv {
		switch {
		case a == "--resume" || a == "-r" || a == "--session-id":
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume="), true
		case strings.HasPrefix(a, "--session-id="):
			return strings.TrimPrefix(a, "--session-id="), true
		case a == "--continue":
			return "", true
		}
	}
	return "", false
}

// copilotFromArgv returns the event log of the session the agent's command
// line resumes (or names with --session-id, once it is written).
func (t *transcripts) copilotFromArgv(a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		argv := a.argv(pid)
		if len(argv) == 0 {
			continue
		}
		if id, resumed := copilotResume(argv[1:]); resumed {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string {
				if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
					return ""
				}
				p := filepath.Join(a.roots[wire.HarnessCopilot], id, "events.jsonl")
				if !isFile(p) {
					return ""
				}
				return p
			}), false
		}
	}
	return "", true
}
