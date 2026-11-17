package follow

import (
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Grok Build keeps a session in <grok home>/sessions/<cwd,
// percent-encoded>/<id>/: its updates.jsonl and other files, each with a
// .lock it holds while it runs. It is found by a file of that directory it
// has open, else by the session its command line resumes, else as the
// newest session of its directory.
var grokFinder = finder{
	matcher:  grokMatcher,
	fromArgv: (*transcripts).grokFromArgv,
}

// grokMatcher recognizes the files of a session directory two levels below
// sessions and returns that session's updates.jsonl.
func grokMatcher(a agentFacts) func(string) (string, bool) {
	root := newSpelledDir(a.roots[wire.HarnessGrok])
	return func(p string) (string, bool) {
		p, ok := root.below(p, 3)
		if !ok {
			return "", false
		}
		updates := filepath.Join(filepath.Dir(p), "updates.jsonl")
		return updates, isFile(updates)
	}
}

// grokResume reads the session a Grok Build command line resumes:
// `--resume[=]<id>`, `-r <id>`, `--session-id[=]<id>`, `-s <id>`; resumed
// without an id (the latest, or a title) for -c / --continue or a bare -r.
func grokResume(argv []string) (id string, resumed bool) {
	for i, a := range argv {
		switch {
		case a == "--resume" || a == "-r" || a == "--session-id" || a == "-s":
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--resume="):
			return strings.TrimPrefix(a, "--resume="), true
		case strings.HasPrefix(a, "--session-id="):
			return strings.TrimPrefix(a, "--session-id="), true
		case a == "--continue" || a == "-c":
			return "", true
		}
	}
	return "", false
}

// grokFromArgv returns the session the agent's command line resumes (or
// names with --session-id, once it is written), in whichever directory's
// sessions it is: the id is a UUIDv7.
func (t *transcripts) grokFromArgv(a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		argv := a.argv(pid)
		if len(argv) == 0 {
			continue
		}
		if id, resumed := grokResume(argv[1:]); resumed {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string {
				if id == "" || strings.ContainsAny(id, `/\*?[`) || id == "." || id == ".." {
					return ""
				}
				p, _ := t.index(a.roots).Find(wire.HarnessGrok, id)
				return p
			}), false
		}
	}
	return "", true
}
