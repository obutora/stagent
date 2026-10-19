package follow

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// newestLimit is how many of a harness's newest conversations the
	// cwd-matching fallback considers.
	newestLimit = 20
	// startSlack absorbs the imprecision of process start times (Linux
	// counts them from a boot time in whole seconds).
	startSlack = 2 * time.Second
)

// agentFacts is what transcript resolution knows about an agent.
type agentFacts struct {
	harness string
	pids    []int // see candidate.pids
	start   time.Time
	cwd     string
	// tty names the agent's terminal below /dev ("pts/3", "ttys003"), ""
	// without one.
	tty   string
	roots transcript.Roots // as the agent's environment configures them
	argv  func(pid int) []string
	// openFiles lists what a process has open (nil where the OS cannot
	// tell).
	openFiles func(pid int) []string
	// claimed reports whether another agent writes path: the cwd fallback
	// must not hand it to this one.
	claimed func(path string) bool
}

// transcripts finds the conversation file an agent process writes.
type transcripts struct {
	// indexes keep conversation metadata in memory (titles, cwd of the
	// newest conversations) per set of roots (rootsKey).
	indexes map[string]*transcript.Index
	// claudePaths caches where a Claude session's transcript lives
	// ("<projects dir>\x00<session id>" → path).
	claudePaths map[string]string
	// What command lines resolved to, per process: looking a session up
	// lists whole transcript directories. Entries not used during a scan
	// are dropped at its end (endScan).
	argvPaths, argvUsed map[instance]string
}

func newTranscripts() *transcripts {
	return &transcripts{
		indexes:     map[string]*transcript.Index{},
		claudePaths: map[string]string{},
		argvPaths:   map[instance]string{},
		argvUsed:    map[instance]string{},
	}
}

func (t *transcripts) index(r transcript.Roots) *transcript.Index {
	key := rootsKey(r)
	x := t.indexes[key]
	if x == nil {
		x = transcript.OpenIndex("", r) // no path: never persisted
		t.indexes[key] = x
	}
	return x
}

// rootsKey identifies a set of roots: the roots of every harness, in the
// order of the registry.
func rootsKey(r transcript.Roots) string {
	var b strings.Builder
	for _, h := range harness.All {
		b.WriteString(r[h.ID])
		b.WriteByte(0)
	}
	return b.String()
}

// endScan forgets the command-line resolutions of processes that were not
// looked at since the previous call.
func (t *transcripts) endScan() {
	t.argvPaths, t.argvUsed = t.argvUsed, map[instance]string{}
}

// finder is how the transcript of an agent of one harness is found. Each
// harness of harness.All has one in finders, in its own file.
type finder struct {
	// session resolves the transcript and the working directory from the
	// record the harness keeps per process (Claude Code). Harnesses
	// without one are found by the files they write and set matcher.
	session func(t *transcripts, a agentFacts) (path, cwd string)
	// matcher recognizes the transcripts of the harness among open files
	// and returns them spelled under the agent's roots, as the index
	// lists them: open files' paths have their symlinks resolved (Linux's
	// /proc/<pid>/fd), the roots may not, and claims compare the two.
	matcher func(a agentFacts) func(string) (string, bool)
	// breadcrumb, when set, returns the session a record of the agent's
	// terminal names; ok with path "" means it is not written yet.
	breadcrumb func(a agentFacts, match func(string) (string, bool)) (path string, ok bool)
	// fromArgv, when set, returns the session the agent's command line
	// resumes or continues; fresh reports one that starts a new
	// conversation.
	fromArgv func(t *transcripts, a agentFacts) (path string, fresh bool)
	// argvCwd, when set, reports a command line whose session options
	// need the process's directory (relative paths, --continue).
	argvCwd func(argv []string) bool
	// fileTime, when set, reads when a conversation started from its
	// file name.
	fileTime func(base string) (time.Time, bool)
}

var finders = map[string]finder{
	wire.HarnessClaude: claudeFinder,
	wire.HarnessCodex:  codexFinder,
	wire.HarnessOmp:    ompFinder,
}

// resolve returns the transcript an agent writes ("" before it has one)
// and, for a harness with a session record, the working directory it
// records.
//
// Harnesses found by their files (Codex, omp): the transcript the agent
// has open (they keep it open once they wrote to it), else the session its
// terminal's breadcrumb names, else the session its command line resumes,
// else the newest conversation in its directory that no other agent
// writes.
func (t *transcripts) resolve(a agentFacts) (path, cwd string) {
	f, ok := finders[a.harness]
	switch {
	case !ok:
		return "", ""
	case f.session != nil:
		return f.session(t, a)
	}
	match := f.matcher(a)
	if p := fromOpenFiles(a, match); p != "" {
		return p, ""
	}
	if f.breadcrumb != nil {
		if p, ok := f.breadcrumb(a, match); ok {
			return p, ""
		}
	}
	p, fresh := t.fromArgv(a)
	if p != "" {
		return p, ""
	}
	return t.newest(a, fresh), ""
}

// spelledDir is a directory as configured and with its symlinks resolved.
type spelledDir struct{ dir, real string }

func newSpelledDir(dir string) spelledDir {
	return spelledDir{filepath.Clean(dir), realDir(dir)}
}

// below returns p, a path below the directory in either spelling and depth
// path elements deep (any depth when negative), spelled below its
// configured form.
func (d spelledDir) below(p string, depth int) (string, bool) {
	for _, base := range [...]string{d.dir, d.real} {
		rel, err := filepath.Rel(base, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		if depth >= 0 && strings.Count(rel, string(filepath.Separator))+1 != depth {
			continue
		}
		return filepath.Join(d.dir, rel), true
	}
	return "", false
}

// fromOpenFiles picks the newest matching file the agent's processes have
// open, spelled as match returns it.
func fromOpenFiles(a agentFacts, match func(string) (string, bool)) string {
	var best string
	var bestTime time.Time
	for _, pid := range a.pids {
		for _, p := range a.openFiles(pid) {
			p, ok := match(p)
			if !ok {
				continue
			}
			if st, err := os.Stat(p); err == nil && (best == "" || st.ModTime().After(bestTime)) {
				best, bestTime = p, st.ModTime()
			}
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// Command lines

// fromArgv returns the session the agent's command line resumes or
// continues. fresh reports a command line that starts a new conversation.
func (t *transcripts) fromArgv(a agentFacts) (path string, fresh bool) {
	if from := finders[a.harness].fromArgv; from != nil {
		return from(t, a)
	}
	return "", true
}

func (t *transcripts) cachedArgv(key instance, find func() string) string {
	p, ok := t.argvPaths[key]
	if !ok || p != "" && !isFile(p) {
		p = find()
	}
	t.argvUsed[key] = p
	return p
}

// absFrom resolves p against dir unless it is absolute.
func absFrom(dir, p string) string {
	if filepath.IsAbs(p) || dir == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// createdAt is when a conversation started: the time in its UUIDv7 id (what
// current Codex and omp use), else the time fileTime reads in its file
// name.
func createdAt(fileTime func(base string) (time.Time, bool), path, id string) (time.Time, bool) {
	if len(id) == 36 && id[14] == '7' {
		if ms, err := strconv.ParseUint(id[0:8]+id[9:13], 16, 64); err == nil {
			return time.UnixMilli(int64(ms)), true
		}
	}
	if fileTime == nil {
		return time.Time{}, false
	}
	return fileTime(filepath.Base(path))
}

// ---------------------------------------------------------------------------
// Fallback

// newest is the fallback where neither open files nor the command line tell
// the transcript: the harness's newest conversation in the agent's working
// directory written since the agent started and not claimed by another
// agent. A fresh agent (not resuming) prefers a conversation created since
// it started, which cannot be an older agent's.
func (t *transcripts) newest(a agentFacts, fresh bool) string {
	if a.cwd == "" {
		return ""
	}
	list, err := t.index(a.roots).List([]string{a.harness}, newestLimit)
	if err != nil {
		return ""
	}
	since := a.start.Add(-startSlack)
	updated := ""
	for _, c := range list { // newest first
		if time.UnixMilli(c.UpdatedAt).Before(since) || !samePath(c.Cwd, a.cwd) || a.claimed(c.Path) {
			continue
		}
		if !fresh {
			return c.Path
		}
		if at, ok := createdAt(finders[a.harness].fileTime, c.Path, c.ID); ok && !at.Before(since) {
			return c.Path
		}
		if updated == "" {
			updated = c.Path
		}
	}
	return updated
}

// claims tracks the transcripts the agents of one snapshot write, so the
// fallback does not hand one agent's transcript to another.
type claims struct {
	t       *transcripts
	s       *ptable.Snapshot
	owns    func(pid int) bool // see locator.owns
	rootsOf func(pid int) transcript.Roots
	// held maps transcripts to the processes of stagent's user, of the
	// harnesses found by their files, that have them open or name them on
	// their command line (collected on first use). Another user's process
	// is not asked: its environment, command line and directory would
	// point stagent at paths of that user's choosing.
	held     map[string][]int
	assigned map[string]bool // what earlier agents of the scan resolved to
}

func (t *transcripts) newClaims(s *ptable.Snapshot, owns func(pid int) bool, rootsOf func(pid int) transcript.Roots) *claims {
	return &claims{t: t, s: s, owns: owns, rootsOf: rootsOf, assigned: map[string]bool{}}
}

// claimedBy returns the claimed func of the agent made of the processes
// own.
func (c *claims) claimedBy(own []int) func(string) bool {
	return func(path string) bool {
		if c.assigned[path] {
			return true
		}
		if c.held == nil {
			c.held = c.collect()
		}
		for _, pid := range c.held[path] {
			if !slices.Contains(own, pid) {
				return true
			}
		}
		return false
	}
}

// assign records an agent's transcript.
func (c *claims) assign(path string) {
	if path != "" {
		c.assigned[path] = true
	}
}

func (c *claims) collect() map[string][]int {
	s := c.s
	held := map[string][]int{}
	for _, pid := range s.PIDs() {
		p := s.Get(pid)
		n := ptable.BaseName(p.Name)
		if harness.Name(n) == "" && !harness.Interpreter(n) {
			continue
		}
		h := harness.Of(p.Name, s.Argv(pid))
		f := finders[h]
		if f.matcher == nil || !c.owns(pid) {
			continue
		}
		a := agentFacts{
			harness: h, pids: []int{pid}, start: p.Start, roots: c.rootsOf(pid),
			argv: s.Argv, openFiles: s.OpenFiles,
		}
		if f.argvCwd != nil && f.argvCwd(s.Argv(pid)) {
			a.cwd = s.Cwd(pid) // relative paths, --continue
		}
		if path := fromOpenFiles(a, f.matcher(a)); path != "" {
			held[path] = append(held[path], pid)
		}
		if path, _ := c.t.fromArgv(a); path != "" {
			held[path] = append(held[path], pid)
		}
	}
	return held
}

// ---------------------------------------------------------------------------

// meta returns a transcript's title and modification time.
func (t *transcripts) meta(harness string, roots transcript.Roots, path string) (title string, mtime time.Time) {
	if c, ok := t.index(roots).Conversation(harness, path); ok {
		return c.Title, time.UnixMilli(c.UpdatedAt)
	}
	if st, err := os.Stat(path); err == nil {
		return "", st.ModTime()
	}
	return "", time.Time{}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// realDir resolves symlinks in dir, as the paths of open files are.
func realDir(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return filepath.Clean(dir)
}

// samePath compares cleaned paths, ignoring case on Windows where the
// harnesses and the process table may spell a directory differently.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
