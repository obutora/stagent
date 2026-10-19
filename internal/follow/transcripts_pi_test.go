package follow

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// piSession writes a Pi session of cwd, <root>/--<cwd>--/<created>_<id>.jsonl
// (or directly in root for a --session-dir), last written at mtime.
func piSession(t *testing.T, root, cwd string, flat bool, created, mtime time.Time, n int) string {
	t.Helper()
	id := v7(created, n)
	name := created.UTC().Format("2006-01-02T15-04-05.000Z")
	name = name[:19] + "-" + name[20:] + "_" + id + ".jsonl"
	dir := root
	if !flat {
		dir = filepath.Join(root, piProjectDir(cwd))
	}
	return write(t, filepath.Join(dir, name),
		fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":%q,"cwd":%q}`+"\n", id, created.UTC().Format(time.RFC3339Nano), cwd)+
			`{"type":"message","id":"a1","parentId":null,"timestamp":"2026-10-19T00:00:00.000Z","message":{"role":"user","content":"hi","timestamp":1}}`+"\n", mtime)
}

// Pi keeps its session in a directory named after the working directory.
func TestPiProjectDir(t *testing.T) {
	for cwd, want := range map[string]string{
		"/home/user/proj":     "--home-user-proj--",
		`C:\Users\u\proj`:     "--C--Users-u-proj--",
		"/":                   "----",
		"/srv/a:b/c":          "--srv-a-b-c--",
		`\\server\share\repo`: "---server-share-repo--", // only one leading separator goes
	} {
		if got := piProjectDir(cwd); got != want {
			t.Errorf("piProjectDir(%q) = %q, want %q", cwd, got, want)
		}
	}
}

// Pi opens its session file only to append to it, and its process title
// hides its arguments (macOS and Linux show argv as ["pi", "", ...]): the
// session it writes is the newest of its directory written since it
// started that no other agent writes, one created since it started first.
func TestPiTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	roots := transcript.Roots{wire.HarnessPi: root}
	// After the record below was created.
	start := time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	// Pi 1.3.0's record (transcript/testdata), resumed from the TUI's
	// /resume: created long before, written since the agent started.
	b, err := os.ReadFile(filepath.Join("..", "transcript", "testdata", "pi.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	resumed := write(t, filepath.Join(root, "--home-user-proj--", "2026-10-19T15-17-10-242Z_01a154bc-ede2-7449-9df3-bd69c074f6dc.jsonl"),
		string(b), start.Add(5*time.Minute))
	piSession(t, root, "/home/user/proj", false, start.Add(-day), start.Add(-day), 1)                         // not written since
	other := piSession(t, root, "/home/user/other", false, start.Add(time.Minute), start.Add(time.Minute), 2) // another directory's
	a := agentFacts{harness: wire.HarnessPi, pids: []int{9}, start: start, cwd: "/home/user/proj", roots: roots,
		argv: argvOf("pi", "", "", ""), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != resumed {
		t.Fatalf("resumed: got %q, want %q", path, resumed)
	}
	// The first answer of a new session creates its file.
	fresh := piSession(t, root, "/home/user/proj", false, start.Add(2*time.Minute), start.Add(2*time.Minute), 3)
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Fatalf("fresh: got %q, want %q", path, fresh)
	}
	a.claimed = func(p string) bool { return p == fresh }
	if path, _ := newTranscripts().resolve(a); path != resumed {
		t.Fatalf("skipping a claimed session: got %q, want %q", path, resumed)
	}
	// Its open file, seen while Pi appends to it, names it.
	a.claimed, a.openFiles = noClaims, func(int) []string { return []string{other} }
	if path, _ := newTranscripts().resolve(a); path != other {
		t.Fatalf("open file: got %q, want %q", path, other)
	}
}

// Where the command line is readable (Windows, or before Pi set its
// title), its session options name the session.
func TestPiTranscriptFromArgs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	roots := transcript.Roots{wire.HarnessPi: root}
	start := time.Now().Add(-time.Minute)
	day := 24 * time.Hour
	resumed := piSession(t, root, "/work", false, start.Add(-3*day), start.Add(-3*day), 1)
	piSession(t, root, "/work", false, start.Add(-5*day), start.Add(-day/2), 2) // the last written before the agent
	last := piSession(t, root, "/work", false, start.Add(-day), start.Add(-day), 3)
	piSession(t, root, "/work", false, start.Add(10*time.Second), start.Add(20*time.Second), 4) // another agent's, newer
	exact := piSession(t, root, "/work", false, start.Add(-2*day), start.Add(-2*day), 5)
	sessionDir := filepath.Join(t.TempDir(), "flat")
	inDir := piSession(t, sessionDir, "/work", true, start.Add(-day), start.Add(-day), 6)
	newest := piSession(t, root, "/work", false, start.Add(30*time.Second), start.Add(30*time.Second), 7) // a fresh agent's
	os.Chtimes(last, start.Add(-day/4), start.Add(-day/4))                                                // continued earlier today

	for _, c := range []struct {
		name string
		argv []string
		want string
	}{
		{"id prefix", []string{"node", `C:\npm\node_modules\@earendil-works\pi-coding-agent\dist\cli.js`, "--session", ompID(resumed)[:13]}, resumed},
		{"path", []string{"pi", "--session=" + filepath.Join("x", "..", filepath.Base(filepath.Dir(resumed)), filepath.Base(resumed))}, resumed},
		{"exact id", []string{"pi", "--session-id", ompID(exact)}, exact},
		{"new id", []string{"pi", "--session-id", "01a15500-0000-7000-8000-00000000ffff"}, newest},
		{"continue", []string{"pi", "-c"}, last},
		{"session dir", []string{"pi", "--session-dir", sessionDir, "--session", ompID(inDir)[:8]}, inDir},
		{"continue in session dir", []string{"pi", "--continue", "--session-dir=" + sessionDir}, inDir},
		{"fork starts a new session", []string{"pi", "--fork", ompID(resumed)[:8]}, newest},
	} {
		cwd := "/work"
		if c.name == "path" {
			cwd = root
		}
		a := agentFacts{harness: wire.HarnessPi, pids: []int{9}, start: start, cwd: cwd, roots: roots,
			argv: argvOf(c.argv...), openFiles: noOpenFiles, claimed: noClaims}
		if path, _ := newTranscripts().resolve(a); path != c.want {
			t.Errorf("%s: got %q, want %q", c.name, path, c.want)
		}
	}
}
