package follow

import (
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Antigravity CLI keeps a conversation's transcript in <home>/brain/<id>/
// .system_generated/logs/transcript_full.jsonl, which it does not hold
// open, and holds open <home>/presence/<id>.lock (and the conversation's
// database, <home>/conversations/<id>.db with its -wal and -shm, while it
// works on it) for the conversation it shows — also after /resume, /clear
// or /fork switched to another in the same process. It is found by one of
// those files it has open, else by the conversation its command line
// resumes, else as the newest conversation of its directory.
var antigravityFinder = finder{
	matcher:  antigravityMatcher,
	fromArgv: (*transcripts).antigravityFromArgv,
}

// antigravityMatcher recognizes a conversation's presence lock or database
// one level below presence/ or conversations/ and returns that
// conversation's transcript, once it is written.
func antigravityMatcher(a agentFacts) func(string) (string, bool) {
	home := a.roots[wire.HarnessAntigravity]
	presence := newSpelledDir(filepath.Join(home, "presence"))
	conversations := newSpelledDir(filepath.Join(home, "conversations"))
	return func(p string) (string, bool) {
		var id string
		if q, ok := presence.below(p, 1); ok {
			id, ok = strings.CutSuffix(filepath.Base(q), ".lock")
			if !ok {
				return "", false
			}
		} else if q, ok := conversations.below(p, 1); ok {
			base := filepath.Base(q)
			for _, ext := range [...]string{".db", ".db-wal", ".db-shm"} {
				if s, found := strings.CutSuffix(base, ext); found {
					id = s
					break
				}
			}
		}
		if id == "" || id == "." || id == ".." {
			return "", false
		}
		transcript := antigravityTranscript(home, id)
		return transcript, isFile(transcript)
	}
}

// antigravityTranscript is the transcript of conversation id.
func antigravityTranscript(home, id string) string {
	return filepath.Join(home, "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
}

// antigravityResume reads the conversation an Antigravity CLI command line
// resumes: `--conversation[=]<id>`; resumed without an id (the most
// recent) for -c / --continue.
func antigravityResume(argv []string) (id string, resumed bool) {
	for i, a := range argv {
		switch {
		case a == "--conversation":
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				return argv[i+1], true
			}
			return "", true
		case strings.HasPrefix(a, "--conversation="):
			return strings.TrimPrefix(a, "--conversation="), true
		case a == "--continue" || a == "-c":
			return "", true
		}
	}
	return "", false
}

// antigravityFromArgv returns the conversation the agent's command line
// resumes, once its transcript is written.
func (t *transcripts) antigravityFromArgv(a agentFacts) (path string, fresh bool) {
	for _, pid := range a.pids {
		argv := a.argv(pid)
		if len(argv) == 0 {
			continue
		}
		if id, resumed := antigravityResume(argv[1:]); resumed {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string {
				if id == "" || strings.ContainsAny(id, `/\*?[`) || id == "." || id == ".." {
					return ""
				}
				p, _ := t.index(a.roots).Find(wire.HarnessAntigravity, id)
				return p
			}), false
		}
	}
	return "", true
}
