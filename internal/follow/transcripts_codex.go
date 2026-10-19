package follow

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Codex is found by the rollout it has open, else the session its command
// line resumes.
var codexFinder = finder{
	matcher:  codexMatcher,
	fromArgv: (*transcripts).codexFromArgv,
	fileTime: codexFileTime,
}

// codexMatcher recognizes rollouts below <codex home>/sessions.
func codexMatcher(a agentFacts) func(string) (string, bool) {
	sessions := newSpelledDir(filepath.Join(a.roots[wire.HarnessCodex], "sessions"))
	return func(p string) (string, bool) {
		name := filepath.Base(p)
		if !strings.HasPrefix(name, "rollout-") || !strings.HasSuffix(name, ".jsonl") {
			return "", false
		}
		return sessions.below(p, -1)
	}
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// codexResume reads `codex [options] resume [options] [SESSION_ID]`
// (also `codex exec resume …`): resumed reports the subcommand, id the
// session UUID — "" for --last, the picker, or a session name.
func codexResume(argv []string) (id string, resumed bool) {
	i := slices.Index(argv, "resume")
	if i < 0 {
		return "", false
	}
	rest := argv[i+1:]
	if slices.Contains(rest, "--last") {
		return "", true
	}
	for _, a := range rest {
		if uuidPattern.MatchString(a) {
			return a, true
		}
	}
	return "", true
}

// codexFromArgv returns the rollout of the session the agent's command
// line resumes.
func (t *transcripts) codexFromArgv(a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		if id, resumed := codexResume(a.argv(pid)); resumed {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string {
				if id == "" {
					return ""
				}
				p, _ := t.index(a.roots).Find(wire.HarnessCodex, id)
				return p
			}), false
		}
	}
	return "", true
}

// codexFileTime reads rollout-2006-01-02T15-04-05-<id>.jsonl, local time.
func codexFileTime(base string) (time.Time, bool) {
	if len(base) > 27 {
		if at, err := time.ParseInLocation("2006-01-02T15-04-05", base[8:27], time.Local); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}
