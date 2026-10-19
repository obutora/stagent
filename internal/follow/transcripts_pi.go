package follow

import (
	"path/filepath"
	"strings"

	"github.com/obutora/stagent/internal/wire"
)

// Pi opens its session file only to append a record and closes it at
// once, so it is seldom seen open, and it keeps no record of the session
// of a terminal. Its process title overwrites its arguments in the
// process table on macOS and Linux (argv reads ["pi", "", ...]), so its
// session options count only where they are readable (Windows). Mostly
// the fallback finds its session: the newest of its directory written
// since it started that no other agent writes. A session the shell
// wrapper started is followed by what Pi's extension reports instead.
var piFinder = finder{
	matcher:  piMatcher,
	fromArgv: (*transcripts).piFromArgv,
	argvCwd:  func(argv []string) bool { return parsePiArgs(argv) != (piArgs{}) },
	fileTime: ompFileTime, // Pi names its sessions as omp does
}

// piMatcher recognizes sessions below the sessions root or the agent's
// --session-dir.
func piMatcher(a agentFacts) func(string) (string, bool) {
	root := newSpelledDir(a.roots[wire.HarnessPi])
	var dir *spelledDir
	if o := parsePiArgs(a.argv(firstPID(a))); o.sessionDir != "" {
		d := newSpelledDir(absFrom(a.cwd, o.sessionDir))
		dir = &d
	}
	return func(p string) (string, bool) {
		// <root>/--<cwd>--/<session>.jsonl, or <--session-dir>/<session>.jsonl.
		if !strings.HasSuffix(p, ".jsonl") {
			return "", false
		}
		if q, ok := root.below(p, 2); ok {
			return q, true
		}
		if dir != nil {
			return dir.below(p, 1)
		}
		return "", false
	}
}

func firstPID(a agentFacts) int {
	if len(a.pids) == 0 {
		return 0
	}
	return a.pids[0]
}

// piArgs are the session options of a pi command line.
type piArgs struct {
	session    string // --session: a session file or id prefix
	sessionID  string // --session-id: an exact id, created when missing
	resumed    bool   // -r/--resume: picked in the TUI
	cont       bool   // -c/--continue: the last written session of the directory
	fork       bool   // --fork: a new session
	sessionDir string // --session-dir: sessions stored there, without project directories
}

func parsePiArgs(argv []string) piArgs {
	var o piArgs
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		value := func(name string) (string, bool) {
			if a == name {
				if i+1 < len(argv) {
					i++
					return argv[i], true
				}
				return "", true
			}
			if v, ok := strings.CutPrefix(a, name+"="); ok {
				return v, true
			}
			return "", false
		}
		if v, ok := value("--session"); ok {
			o.session = v
		} else if v, ok := value("--session-id"); ok {
			o.sessionID = v
		} else if _, ok := value("--fork"); ok {
			o.fork = true
		} else if v, ok := value("--session-dir"); ok {
			o.sessionDir = v
		} else if a == "-r" || a == "--resume" {
			o.resumed = true
		} else if a == "-c" || a == "--continue" {
			o.cont = true
		} else if a == "--" {
			break
		}
	}
	return o
}

// piFromArgv returns the session the agent's command line opens. A fork
// and a --session-id not written yet start a new one.
func (t *transcripts) piFromArgv(a agentFacts) (path string, fresh bool) {
	pid := firstPID(a)
	o := parsePiArgs(a.argv(pid))
	switch {
	case o.fork:
		return "", true
	case o.session != "" || o.sessionID != "" || o.cont:
		p := t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string { return t.piFromArgs(a, o) })
		return p, p == "" && !o.cont && o.session == ""
	case o.resumed:
		return "", false
	}
	return "", true
}

// piFromArgs resolves --session <path | id prefix> (the newest match),
// --session-id <id>, and --continue (the session of the agent's directory
// written last before the agent started).
func (t *transcripts) piFromArgs(a agentFacts, o piArgs) string {
	root, sessionDir := a.roots[wire.HarnessPi], ""
	if o.sessionDir != "" {
		sessionDir = absFrom(a.cwd, o.sessionDir)
	}
	if s := o.session; s != "" && (strings.ContainsAny(s, `/\`) || strings.HasSuffix(s, ".jsonl")) {
		if p := absFrom(a.cwd, s); isFile(p) {
			return p
		}
		return ""
	}
	var files []sessionFile
	switch {
	case o.session != "" || o.sessionID != "":
		files = ompFiles(root, sessionDir)
	case sessionDir != "":
		files = ompFiles("", sessionDir)
	case a.cwd != "":
		files = ompFiles("", filepath.Join(root, piProjectDir(a.cwd)))
	}
	var best sessionFile
	for _, f := range files {
		var ok bool
		switch {
		case o.sessionID != "":
			ok = f.id == o.sessionID
		case o.session != "":
			ok = strings.HasPrefix(f.id, o.session)
		default: // --continue: Pi opens the session of the directory written last
			at, known := createdAt(ompFileTime, f.path, f.id)
			ok = known && at.Before(a.start.Add(startSlack))
		}
		if ok && (best.path == "" || f.mtime.After(best.mtime)) {
			best = f
		}
	}
	return best.path
}

// piProjectDir is the directory Pi keeps the sessions of cwd in below its
// sessions root: the path without its leading separator, every / \ and :
// as -, between -- and --.
func piProjectDir(cwd string) string {
	if strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) {
		cwd = cwd[1:]
	}
	return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(cwd) + "--"
}
