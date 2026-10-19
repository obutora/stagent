package follow

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// omp is found by the session it has open, else the session its
// terminal's breadcrumb names, else the session its command line resumes
// or continues.
var ompFinder = finder{
	matcher:    ompMatcher,
	breadcrumb: ompFromBreadcrumb,
	fromArgv:   (*transcripts).ompFromArgv,
	argvCwd:    func(argv []string) bool { return parseOmpArgs(argv) != (ompArgs{}) },
	fileTime:   ompFileTime,
}

// continueLimit is how many of the newest-created omp sessions --continue
// resolution checks for the agent's directory.
const continueLimit = 50

// ompMatcher recognizes sessions below the sessions root or the agent's
// --session-dir.
func ompMatcher(a agentFacts) func(string) (string, bool) {
	root := newSpelledDir(a.roots[wire.HarnessOmp])
	var dir *spelledDir
	if o, _ := ompArgsOf(a); o.sessionDir != "" {
		d := newSpelledDir(absFrom(a.cwd, o.sessionDir))
		dir = &d
	}
	return func(p string) (string, bool) {
		// <root>/<project>/<session>.jsonl (sub-agent sessions are a
		// level deeper), or <--session-dir>/<session>.jsonl.
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

// ompArgs are the session options of an omp command line.
type ompArgs struct {
	resume     string // -r/--resume value: session id prefix or file path
	resumed    bool   // -r/--resume, with or without a value (picker)
	cont       bool   // -c/--continue: the newest session of the directory
	sessionDir string // --session-dir: sessions stored there instead
}

func parseOmpArgs(argv []string) ompArgs {
	var o ompArgs
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		next := func() string {
			if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				return argv[i]
			}
			return ""
		}
		switch {
		case a == "-r" || a == "--resume":
			o.resumed, o.resume = true, next()
		case strings.HasPrefix(a, "--resume=") || strings.HasPrefix(a, "-r="):
			_, o.resume, _ = strings.Cut(a, "=")
			o.resumed = true
		case a == "-c" || a == "--continue":
			o.cont = true
		case a == "--session-dir":
			o.sessionDir = next()
		case strings.HasPrefix(a, "--session-dir="):
			o.sessionDir = strings.TrimPrefix(a, "--session-dir=")
		}
	}
	return o
}

// ompArgsOf returns the session options of the first of the agent's
// processes that has any (workers run without them), and its pid.
func ompArgsOf(a agentFacts) (ompArgs, int) {
	for _, pid := range a.pids {
		if o := parseOmpArgs(a.argv(pid)); o != (ompArgs{}) {
			return o, pid
		}
	}
	return ompArgs{}, 0
}

// ompFromBreadcrumb reads the record omp keeps of the session of the
// agent's terminal, <agent dir>/terminal-sessions/<terminal below /dev, /
// as ->: the cwd, the session file, then "fresh" while that session is not
// written yet. omp writes it whenever it starts, continues, resumes or
// switches sessions, unless it already says so: `omp -c` reads it and may
// leave it as it was. So it counts when written since the agent started,
// or for --continue when it names the agent's directory; an earlier omp in
// the same (reused) terminal left any other. ok with path "" means the
// agent's session is not written yet.
func ompFromBreadcrumb(a agentFacts, match func(string) (string, bool)) (path string, ok bool) {
	if a.tty == "" {
		return "", false
	}
	file := filepath.Join(filepath.Dir(a.roots[wire.HarnessOmp]), "terminal-sessions", strings.ReplaceAll(a.tty, "/", "-"))
	st, err := os.Stat(file)
	if err != nil {
		return "", false
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) < 2 {
		return "", false
	}
	cwd, session := lines[0], lines[1]
	if st.ModTime().Before(a.start.Add(-startSlack)) {
		if o, _ := ompArgsOf(a); !o.cont || !samePath(cwd, a.cwd) {
			return "", false
		}
	}
	p, ok := match(session)
	if !ok {
		return "", false
	}
	if isFile(p) {
		return p, true
	}
	return "", slices.Contains(lines[2:], "fresh")
}

// ompFromArgv returns the session the agent's command line resumes or
// continues.
func (t *transcripts) ompFromArgv(a agentFacts) (path string, fresh bool) {
	if o, pid := ompArgsOf(a); o.resumed || o.cont {
		return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string { return t.ompFromArgs(a, o) }), false
	}
	return "", true
}

// sessionFile is one omp session file.
type sessionFile struct {
	path, id string
	mtime    time.Time
}

// ompFiles lists the session files directly in --session-dir, or in the
// project directories under the sessions root.
func ompFiles(root, sessionDir string) []sessionFile {
	dirs := []string{sessionDir}
	if sessionDir == "" {
		ents, _ := os.ReadDir(root)
		dirs = dirs[:0]
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	var out []sessionFile
	for _, d := range dirs {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			_, id, ok := strings.Cut(strings.TrimSuffix(name, ".jsonl"), "_") // <timestamp>_<id>
			info, err := e.Info()
			if !ok || err != nil {
				continue
			}
			out = append(out, sessionFile{filepath.Join(d, name), id, info.ModTime()})
		}
	}
	return out
}

// ompFromArgs resolves `--resume <id prefix | path>` (the newest match) and
// `--continue` (the newest session of the agent's directory created before
// the agent started).
func (t *transcripts) ompFromArgs(a agentFacts, o ompArgs) string {
	root := a.roots[wire.HarnessOmp]
	sessionDir := ""
	if o.sessionDir != "" {
		sessionDir = absFrom(a.cwd, o.sessionDir)
	}
	if o.resume != "" {
		if strings.ContainsAny(o.resume, `/\`) || strings.HasSuffix(o.resume, ".jsonl") {
			if p := absFrom(a.cwd, o.resume); isFile(p) {
				return p
			}
			return ""
		}
		var best sessionFile
		for _, f := range ompFiles(root, sessionDir) {
			if strings.HasPrefix(f.id, o.resume) && (best.path == "" || f.mtime.After(best.mtime)) {
				best = f
			}
		}
		return best.path
	}
	if !o.cont || a.cwd == "" && sessionDir == "" {
		return ""
	}
	type created struct {
		path string
		at   time.Time
	}
	var before []created
	limit := a.start.Add(startSlack)
	for _, f := range ompFiles(root, sessionDir) {
		if at, ok := createdAt(ompFileTime, f.path, f.id); ok && at.Before(limit) {
			before = append(before, created{f.path, at})
		}
	}
	sort.Slice(before, func(i, j int) bool { return before[i].at.After(before[j].at) })
	for i, c := range before {
		if i == continueLimit {
			break
		}
		if sessionDir != "" {
			return c.path
		}
		if conv, ok := t.index(a.roots).Conversation(wire.HarnessOmp, c.path); ok && samePath(conv.Cwd, a.cwd) {
			return c.path
		}
	}
	return ""
}

// ompFileTime reads 2006-01-02T15-04-05-000Z_<id>.jsonl, UTC.
func ompFileTime(base string) (time.Time, bool) {
	if ts, _, ok := strings.Cut(base, "_"); ok {
		if at, err := time.Parse("2006-01-02T15-04-05-000Z", ts); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}
