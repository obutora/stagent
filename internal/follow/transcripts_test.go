package follow

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
)

func write(t *testing.T, path, content string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func noOpenFiles(int) []string { return nil }
func noClaims(string) bool     { return false }
func argvOf(argv ...string) func(int) []string {
	return func(int) []string { return argv }
}

// v7 is a UUIDv7 created at t (n tells ids of one millisecond apart).
func v7(at time.Time, n int) string {
	ms := at.UnixMilli()
	return fmt.Sprintf("%08x-%04x-7000-8000-%012x", ms>>16, ms&0xffff, n)
}

func TestClaudeTranscriptFollowsSessionFile(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(t.TempDir(), "claude-config")
	roots := transcript.DefaultRoots(home, func(k string) string {
		if k == "CLAUDE_CONFIG_DIR" {
			return config
		}
		return ""
	})
	start := time.Now().Add(-time.Minute)
	const id1, id2 = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
	session := func(id string, startedAt time.Time) {
		write(t, filepath.Join(config, "sessions", "4242.json"),
			fmt.Sprintf(`{"pid":4242,"sessionId":%q,"cwd":"/work","startedAt":%d}`, id, startedAt.UnixMilli()), time.Time{})
	}
	p1 := write(t, filepath.Join(config, "projects", "-work", id1+".jsonl"), `{"type":"user","message":{"content":"hi"}}`+"\n", time.Time{})
	tr := newTranscripts()
	a := agentFacts{harness: "claude", pids: []int{4242}, start: start, roots: roots, argv: argvOf("claude"), openFiles: noOpenFiles, claimed: noClaims}

	session(id1, start.Add(1500*time.Millisecond))
	if path, cwd := tr.resolve(a); path != p1 || cwd != "/work" {
		t.Fatalf("got %q %q, want %q /work", path, cwd, p1)
	}
	// /clear: the session file names a conversation without records yet.
	session(id2, start.Add(1500*time.Millisecond))
	if path, _ := tr.resolve(a); path != "" {
		t.Fatalf("new conversation without a file resolved to %q", path)
	}
	p2 := write(t, filepath.Join(config, "projects", "-work", id2+".jsonl"), `{"type":"user","message":{"content":"x"}}`+"\n", time.Time{})
	if path, _ := tr.resolve(a); path != p2 {
		t.Fatalf("after /clear got %q, want %q", path, p2)
	}
	// Left by an earlier process that had the same pid.
	session(id1, start.Add(-time.Hour))
	if path, _ := tr.resolve(a); path != "" {
		t.Fatalf("stale session file resolved to %q", path)
	}
}

// codexRollout writes a rollout created at created and last written at
// mtime.
func codexRollout(t *testing.T, codexHome, cwd, originator string, created, mtime time.Time, n int) string {
	t.Helper()
	id := v7(created, n)
	name := fmt.Sprintf("rollout-%s-%s.jsonl", created.Format("2006-01-02T15-04-05"), id)
	return write(t, filepath.Join(codexHome, "sessions", created.Format("2006/01/02"), name),
		fmt.Sprintf(`{"type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":%q,"source":"cli"}}`+"\n", id, cwd, originator), mtime)
}

func TestCodexResume(t *testing.T) {
	const id = "01a0d3a5-e07b-7871-95bd-c47c96166f5b"
	for _, c := range []struct {
		argv    []string
		id      string
		resumed bool
	}{
		{[]string{"codex"}, "", false},
		{[]string{"codex", "fix the resume bug"}, "", false},
		{[]string{"node", "/usr/bin/codex", "resume", id}, id, true},
		{[]string{"/x/vendor/x86_64-unknown-linux-musl/bin/codex", "-c", "model=o3", "resume", "-m", "o3", id, "go on"}, id, true},
		{[]string{"codex", "exec", "resume", id}, id, true},
		{[]string{"codex", "resume", "--last"}, "", true},
		{[]string{"codex", "resume"}, "", true},
		{[]string{"codex", "resume", "my-named-session"}, "", true},
	} {
		got, resumed := codexResume(c.argv)
		if got != c.id || resumed != c.resumed {
			t.Errorf("codexResume(%q) = %q, %v; want %q, %v", c.argv, got, resumed, c.id, c.resumed)
		}
	}
}

func TestCodexTranscriptOfResumedSession(t *testing.T) {
	codexHome := t.TempDir()
	roots := transcript.Roots{Codex: codexHome}
	start := time.Now().Add(-time.Minute)
	old := start.Add(-72 * time.Hour)
	resumed := codexRollout(t, codexHome, "/work", "codex-tui", old, old, 1)
	codexRollout(t, codexHome, "/work", "codex-tui", start.Add(time.Second), start.Add(2*time.Second), 2) // another agent's
	id := filepath.Base(resumed)[len("rollout-2006-01-02T15-04-05-") : len(filepath.Base(resumed))-len(".jsonl")]
	tr := newTranscripts()
	// The node launcher and its native child carry the same arguments; the
	// child's open files are known and hold no rollout yet.
	a := agentFacts{harness: "codex", pids: []int{7, 8}, start: start, cwd: "/work", roots: roots,
		argv:      argvOf("node", "/usr/bin/codex", "resume", id),
		openFiles: func(int) []string { return []string{filepath.Join(codexHome, "state_5.sqlite")} },
		claimed:   noClaims}
	if path, _ := tr.resolve(a); path != resumed {
		t.Fatalf("got %q, want the resumed rollout %q", path, resumed)
	}
}

func TestCodexTranscriptOfFreshSession(t *testing.T) {
	codexHome := t.TempDir()
	roots := transcript.Roots{Codex: codexHome}
	start := time.Now().Add(-time.Hour)
	codexRollout(t, codexHome, "/work", "codex-tui", start.Add(-time.Hour), start.Add(-time.Hour), 1) // before the agent
	codexRollout(t, codexHome, "/other", "codex-tui", start.Add(time.Minute), start.Add(20*time.Minute), 2)
	codexRollout(t, codexHome, "/work", "codex_exec", start.Add(time.Minute), start.Add(30*time.Minute), 3)
	tr := newTranscripts()
	a := agentFacts{harness: "codex", pids: []int{7}, start: start, cwd: "/work", roots: roots,
		argv: argvOf("codex"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := tr.resolve(a); path != "" {
		t.Fatalf("resolved %q before the agent wrote anything", path)
	}
	// An older agent in the same directory writes its (older) rollout after
	// this one started: a fresh session prefers what was created since.
	older := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(-2*time.Hour), start.Add(40*time.Minute), 4)
	if path, _ := tr.resolve(a); path != older {
		t.Fatalf("only candidate: got %q, want %q", path, older)
	}
	own := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(10*time.Minute), start.Add(10*time.Minute), 5)
	if path, _ := tr.resolve(a); path != own {
		t.Fatalf("got %q, want the rollout created since the start %q", path, own)
	}
	// Claimed by another agent: skipped.
	a.claimed = func(p string) bool { return p == own }
	if path, _ := tr.resolve(a); path != older {
		t.Fatalf("claimed: got %q, want %q", path, older)
	}
	// `codex resume --last` continues an older rollout: no preference for
	// new ones.
	a.claimed = noClaims
	a.argv = argvOf("codex", "resume", "--last")
	if path, _ := tr.resolve(a); path != older {
		t.Fatalf("resume --last: got %q, want the most recently written %q", path, older)
	}
}

func TestCodexTranscriptFromOpenFile(t *testing.T) {
	codexHome := t.TempDir()
	roots := transcript.Roots{Codex: codexHome}
	start := time.Now().Add(-time.Hour)
	open := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(time.Minute), start.Add(time.Minute), 1)
	codexRollout(t, codexHome, "/work", "codex-tui", start.Add(2*time.Minute), start.Add(2*time.Minute), 2)
	resumed := codexRollout(t, codexHome, "/work", "codex-tui", start.Add(-time.Hour), start.Add(-time.Hour), 3)
	id := filepath.Base(resumed)[len("rollout-2006-01-02T15-04-05-") : len(filepath.Base(resumed))-len(".jsonl")]
	tr := newTranscripts()
	// What it has open wins over what it resumed (a /new since).
	a := agentFacts{harness: "codex", pids: []int{7, 8}, start: start, cwd: "/work", roots: roots,
		argv: argvOf("codex", "resume", id), claimed: noClaims,
		openFiles: func(pid int) []string {
			if pid == 8 { // the native binary under the node launcher
				return []string{"/dev/null", filepath.Join(codexHome, "log", "codex-tui.log"), open}
			}
			return nil
		}}
	if path, _ := tr.resolve(a); path != open {
		t.Fatalf("got %q, want the open rollout %q", path, open)
	}
}

// Open files come with their symlinks resolved (/proc/<pid>/fd); the roots
// may go through one. The transcript comes back spelled under the roots, as
// the index lists it and claims compare it.
func TestOpenFileUnderSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	resolved := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		if r == p {
			t.Fatalf("%s resolves to itself", p)
		}
		return r
	}
	roots := transcript.Roots{Codex: filepath.Join(link, "codex"), Omp: filepath.Join(link, "omp")}
	start := time.Now().Add(-time.Hour)
	rollout := codexRollout(t, roots.Codex, "/work", "codex-tui", start.Add(time.Minute), start.Add(time.Minute), 1)
	session := ompSession(t, roots.Omp, "-work", "/work", start.Add(time.Minute), start.Add(time.Minute), 2)
	sub := write(t, filepath.Join(roots.Omp, "-work", "x", "Helper.jsonl"), "{}\n", start.Add(3*time.Minute))
	tr := newTranscripts()
	codex := agentFacts{harness: "codex", pids: []int{7}, start: start, cwd: "/work", roots: roots,
		argv: argvOf("codex"), claimed: noClaims,
		openFiles: func(int) []string { return []string{resolved(rollout)} }}
	if path, _ := tr.resolve(codex); path != rollout {
		t.Fatalf("codex: got %q, want %q", path, rollout)
	}
	omp := agentFacts{harness: "omp", pids: []int{9}, start: start, cwd: "/work", roots: roots,
		argv: argvOf("bun", "/home/u/.bun/bin/omp"), claimed: noClaims,
		openFiles: func(int) []string { return []string{resolved(sub), resolved(session)} }}
	if path, _ := tr.resolve(omp); path != session {
		t.Fatalf("omp: got %q, want %q", path, session)
	}
}

func TestParseOmpArgs(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want ompArgs
	}{
		{[]string{"bun", "/home/u/.bun/bin/omp"}, ompArgs{}},
		{[]string{"bun", "/home/u/.bun/bin/omp", "-r", "01a0dda8"}, ompArgs{resume: "01a0dda8", resumed: true}},
		{[]string{"omp", "--resume=../s.jsonl", "--session-dir", "/tmp/s"}, ompArgs{resume: "../s.jsonl", resumed: true, sessionDir: "/tmp/s"}},
		{[]string{"omp", "-r", "--model", "x"}, ompArgs{resumed: true}},
		{[]string{"omp", "--resume"}, ompArgs{resumed: true}},
		{[]string{"omp", "-c", "--session-dir=s"}, ompArgs{cont: true, sessionDir: "s"}},
		{[]string{"omp", "--continue"}, ompArgs{cont: true}},
	} {
		if got := parseOmpArgs(c.argv); got != c.want {
			t.Errorf("parseOmpArgs(%q) = %+v, want %+v", c.argv, got, c.want)
		}
	}
}

// ompSession writes <root>/<project>/<created>_<id>.jsonl last written at
// mtime.
func ompSession(t *testing.T, root, project, cwd string, created, mtime time.Time, n int) string {
	t.Helper()
	id := v7(created, n)
	name := created.UTC().Format("2006-01-02T15-04-05.000Z")
	name = name[:19] + "-" + name[20:] + "_" + id + ".jsonl"
	return write(t, filepath.Join(root, project, name),
		`{"type":"title","v":1,"title":"t"}`+"\n"+
			fmt.Sprintf(`{"type":"session","version":3,"id":%q,"cwd":%q}`+"\n", id, cwd), mtime)
}

func ompID(path string) string {
	b := filepath.Base(path)
	return b[len("2006-01-02T15-04-05-000Z_") : len(b)-len(".jsonl")]
}

func TestOmpTranscript(t *testing.T) {
	root := t.TempDir()
	roots := transcript.Roots{Omp: root}
	start := time.Now().Add(-time.Hour)
	top := ompSession(t, root, "-work", "/work", start.Add(time.Minute), start.Add(time.Minute), 1)
	sub := write(t, filepath.Join(root, "-work", "x", "Helper.jsonl"), "{}\n", start.Add(3*time.Minute))
	newer := ompSession(t, root, "-work", "/work", start.Add(2*time.Minute), start.Add(2*time.Minute), 2)
	tr := newTranscripts()
	a := agentFacts{harness: "omp", pids: []int{9}, start: start, cwd: "/work", roots: roots,
		argv: argvOf("bun", "/home/u/.bun/bin/omp"), claimed: noClaims,
		openFiles: func(int) []string { return []string{sub, top} }}
	if path, _ := tr.resolve(a); path != top {
		t.Fatalf("open files: got %q, want the session %q (not the sub-agent's)", path, top)
	}
	a.openFiles = noOpenFiles
	if path, _ := tr.resolve(a); path != newer {
		t.Fatalf("fallback: got %q, want %q", path, newer)
	}
	a.claimed = func(p string) bool { return p == newer }
	if path, _ := tr.resolve(a); path != top {
		t.Fatalf("fallback skipping a claimed session: got %q, want %q", path, top)
	}
}

func TestOmpTranscriptFromArgs(t *testing.T) {
	root := t.TempDir()
	roots := transcript.Roots{Omp: root}
	start := time.Now().Add(-time.Minute)
	day := 24 * time.Hour
	resumed := ompSession(t, root, "-work", "/work", start.Add(-3*day), start.Add(-3*day), 1)
	ompSession(t, root, "-work", "/work", start.Add(-5*day), start.Add(-5*day), 2)
	last := ompSession(t, root, "-work", "/work", start.Add(-day), start.Add(-day), 3)
	ompSession(t, root, "-other", "/other", start.Add(-time.Hour), start.Add(-time.Hour), 4)
	ompSession(t, root, "-work", "/work", start.Add(10*time.Second), start.Add(20*time.Second), 5) // another agent's, newer
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	inDir := ompSession(t, sessionDir, "", "/work", start.Add(-day), start.Add(-day), 6)

	fds := func(int) []string { return []string{"/dev/pts/3"} } // known, no session open yet
	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"id prefix", []string{"bun", "/b/omp", "-r", ompID(resumed)[:13]}, resumed},
		{"relative path", []string{"omp", "--resume=../proj/../-work/" + filepath.Base(resumed)}, resumed},
		{"continue", []string{"omp", "-c"}, last},
		{"session dir", []string{"omp", "--session-dir", sessionDir, "--resume", ompID(inDir)[:8]}, inDir},
		{"continue in session dir", []string{"omp", "--continue", "--session-dir=" + sessionDir}, inDir},
	} {
		cwd := "/work"
		if c.name == "relative path" {
			cwd = filepath.Join(root, "-other")
		}
		argv := map[int][]string{9: c.argv, 10: {"bun", "cli.js", "__omp_worker"}}
		a := agentFacts{harness: "omp", pids: []int{9, 10}, start: start, cwd: cwd, roots: roots,
			argv:      func(pid int) []string { return argv[pid] },
			openFiles: fds, claimed: noClaims}
		if path, _ := newTranscripts().resolve(a); path != c.want {
			t.Errorf("%s: got %q, want %q", c.name, path, c.want)
		}
	}
}
