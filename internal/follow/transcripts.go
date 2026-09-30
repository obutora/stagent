package follow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// newestLimit is how many of a harness's newest conversations the
	// cwd-matching fallback considers.
	newestLimit = 20
	// continueLimit is how many of the newest-created omp sessions
	// --continue resolution checks for the agent's directory.
	continueLimit = 50
	// startSlack absorbs the imprecision of process start times (Linux
	// counts them from a boot time in whole seconds).
	startSlack = 2 * time.Second
	// claudeStaleSlack: a session file written this long before its pid's
	// process started was left by an earlier process with the same pid.
	claudeStaleSlack = 5 * time.Second
)

// agentFacts is what transcript resolution knows about an agent.
type agentFacts struct {
	harness string
	pids    []int // see candidate.pids
	start   time.Time
	cwd     string
	roots   transcript.Roots // as the agent's environment configures them
	argv    func(pid int) []string
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
	// newest conversations) per set of roots.
	indexes map[transcript.Roots]*transcript.Index
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
		indexes:     map[transcript.Roots]*transcript.Index{},
		claudePaths: map[string]string{},
		argvPaths:   map[instance]string{},
		argvUsed:    map[instance]string{},
	}
}

func (t *transcripts) index(r transcript.Roots) *transcript.Index {
	x := t.indexes[r]
	if x == nil {
		x = transcript.OpenIndex("", r) // no path: never persisted
		t.indexes[r] = x
	}
	return x
}

// endScan forgets the command-line resolutions of processes that were not
// looked at since the previous call.
func (t *transcripts) endScan() {
	t.argvPaths, t.argvUsed = t.argvUsed, map[instance]string{}
}

// resolve returns the transcript an agent writes ("" before it has one)
// and, for Claude, the working directory its session file records.
//
// Codex and omp: the transcript the agent has open (they keep it open once
// they wrote to it), else the session its command line resumes, else the
// newest conversation in its directory that no other agent writes.
func (t *transcripts) resolve(a agentFacts) (path, cwd string) {
	if a.harness == wire.HarnessClaude {
		return t.claude(a)
	}
	match := t.matcher(a)
	if match == nil {
		return "", ""
	}
	if p := fromOpenFiles(a, match); p != "" {
		return p, ""
	}
	p, fresh := t.fromArgv(a)
	if p != "" {
		return p, ""
	}
	return t.newest(a, fresh), ""
}

// matcher recognizes the transcripts of the agent's harness among open
// files; nil for harnesses without file-based resolution.
func (t *transcripts) matcher(a agentFacts) func(string) bool {
	switch a.harness {
	case wire.HarnessCodex:
		sessions := realDir(filepath.Join(a.roots.Codex, "sessions"))
		return func(p string) bool {
			name := filepath.Base(p)
			return within(sessions, p) && strings.HasPrefix(name, "rollout-") && strings.HasSuffix(name, ".jsonl")
		}
	case wire.HarnessOmp:
		root := realDir(a.roots.Omp)
		dir := ""
		if o, _ := ompArgsOf(a); o.sessionDir != "" {
			dir = realDir(absFrom(a.cwd, o.sessionDir))
		}
		return func(p string) bool {
			// <root>/<project>/<session>.jsonl (sub-agent sessions are a
			// level deeper), or <--session-dir>/<session>.jsonl.
			return strings.HasSuffix(p, ".jsonl") &&
				(samePath(filepath.Dir(filepath.Dir(p)), root) || dir != "" && samePath(filepath.Dir(p), dir))
		}
	}
	return nil
}

// claudeSession is <config dir>/sessions/<pid>.json, which Claude Code
// keeps per running process and rewrites when /clear or a resume switches
// conversations.
type claudeSession struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // unix ms
}

func (t *transcripts) claude(a agentFacts) (path, cwd string) {
	dir := filepath.Join(filepath.Dir(a.roots.Claude), "sessions")
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
		return t.claudePath(a.roots.Claude, cs.SessionID), cs.Cwd
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

// fromOpenFiles picks the newest matching file the agent's processes have
// open.
func fromOpenFiles(a agentFacts, match func(string) bool) string {
	var best string
	var bestTime time.Time
	for _, pid := range a.pids {
		for _, p := range a.openFiles(pid) {
			if !match(p) {
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

// fromArgv returns the session the agent's command line resumes or
// continues. fresh reports a command line that starts a new conversation.
func (t *transcripts) fromArgv(a agentFacts) (path string, fresh bool) {
	switch a.harness {
	case wire.HarnessCodex:
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
	case wire.HarnessOmp:
		if o, pid := ompArgsOf(a); o.resumed || o.cont {
			return t.cachedArgv(instance{pid, a.start.UnixNano()}, func() string { return t.ompFromArgs(a, o) }), false
		}
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
		for _, f := range ompFiles(a.roots.Omp, sessionDir) {
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
	for _, f := range ompFiles(a.roots.Omp, sessionDir) {
		if at, ok := createdAt(wire.HarnessOmp, f.path, f.id); ok && at.Before(limit) {
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

// absFrom resolves p against dir unless it is absolute.
func absFrom(dir, p string) string {
	if filepath.IsAbs(p) || dir == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}

// createdAt is when a conversation started: the time in its UUIDv7 id (what
// current Codex and omp use), else the time in its file name.
func createdAt(harness, path, id string) (time.Time, bool) {
	if len(id) == 36 && id[14] == '7' {
		if ms, err := strconv.ParseUint(id[0:8]+id[9:13], 16, 64); err == nil {
			return time.UnixMilli(int64(ms)), true
		}
	}
	base := filepath.Base(path)
	switch harness {
	case wire.HarnessCodex: // rollout-2006-01-02T15-04-05-<id>.jsonl, local time
		if len(base) > 27 {
			if at, err := time.ParseInLocation("2006-01-02T15-04-05", base[8:27], time.Local); err == nil {
				return at, true
			}
		}
	case wire.HarnessOmp: // 2006-01-02T15-04-05-000Z_<id>.jsonl, UTC
		if ts, _, ok := strings.Cut(base, "_"); ok {
			if at, err := time.Parse("2006-01-02T15-04-05-000Z", ts); err == nil {
				return at, true
			}
		}
	}
	return time.Time{}, false
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
		if at, ok := createdAt(a.harness, c.Path, c.ID); ok && !at.Before(since) {
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
	rootsOf func(pid int) transcript.Roots
	// held maps transcripts to the Codex and omp processes that have them
	// open or name them on their command line (collected on first use).
	held     map[string][]int
	assigned map[string]bool // what earlier agents of the scan resolved to
}

func (t *transcripts) newClaims(s *ptable.Snapshot, rootsOf func(pid int) transcript.Roots) *claims {
	return &claims{t: t, s: s, rootsOf: rootsOf, assigned: map[string]bool{}}
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
		n := baseName(p.Name)
		if harnessName(n) == "" && !interpreters[n] {
			continue
		}
		h := harnessOf(p.Name, s.Argv(pid))
		if h != wire.HarnessCodex && h != wire.HarnessOmp {
			continue
		}
		a := agentFacts{
			harness: h, pids: []int{pid}, start: p.Start, roots: c.rootsOf(pid),
			argv: s.Argv, openFiles: s.OpenFiles,
		}
		if h == wire.HarnessOmp && parseOmpArgs(s.Argv(pid)) != (ompArgs{}) {
			a.cwd = s.Cwd(pid) // relative paths, --continue
		}
		if path := fromOpenFiles(a, c.t.matcher(a)); path != "" {
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

// within reports whether p lies below dir.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
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
