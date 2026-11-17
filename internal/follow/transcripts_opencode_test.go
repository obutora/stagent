package follow

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// openCodeSession writes stagent's record of OpenCode session id in cwd,
// last written at mtime, and its process sidecar naming pid (started then)
// when pid is not 0.
func openCodeSession(t *testing.T, root, id, cwd string, pid int, started, mtime time.Time) string {
	t.Helper()
	if pid != 0 {
		write(t, filepath.Join(root, id+".proc"), fmt.Sprintf(`{"pid":%d,"started":%d}`, pid, started.UnixMilli()), mtime)
	}
	return write(t, filepath.Join(root, id+".jsonl"),
		fmt.Sprintf(`{"type":"session","id":%q,"title":"","directory":%q,"created":%d}`+"\n"+
			`{"id":"msg_a","time":{"created":%d},"text":"hi","type":"user"}`+"\n", id, cwd, started.UnixMilli(), started.UnixMilli()), mtime)
}

// OpenCode keeps no file open: its session is found by the process
// sidecar of stagent's record (the terminal UI that last reported it,
// since when it runs), else by the session its command line resumes, else
// as the newest session of its directory.
func TestOpenCodeTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	roots := transcript.Roots{wire.HarnessOpenCode: root}
	start := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cwd := "/home/user/proj"
	old := openCodeSession(t, root, "ses_old", cwd, 0, start.Add(-day), start.Add(-day))
	mine := openCodeSession(t, root, "ses_mine", cwd, 4911, start, start.Add(time.Minute))
	openCodeSession(t, root, "ses_newer", cwd, 5000, start.Add(time.Minute), start.Add(2*time.Minute))
	a := agentFacts{harness: wire.HarnessOpenCode, pids: []int{4911, 4912}, start: start, cwd: cwd, roots: roots,
		argv: argvOf("/usr/bin/opencode", "--standalone"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != mine {
		t.Fatalf("process sidecar: got %q, want %q", path, mine)
	}

	// The same pid, a process that started a day later: the sidecar is an
	// older process's.
	a.start = start.Add(day)
	newest := openCodeSession(t, root, "ses_newest", cwd, 0, start.Add(day), start.Add(day+time.Minute))
	if path, _ := newTranscripts().resolve(a); path != newest {
		t.Fatalf("stale sidecar: got %q, want %q", path, newest)
	}

	a.pids = []int{7}
	for _, argv := range [][]string{
		{"/usr/bin/opencode", "--standalone", "--session", "ses_old"},
		{"/usr/bin/opencode", "-s", "ses_old"},
		{"/usr/bin/opencode", "--session=ses_old"},
	} {
		a.argv = argvOf(argv...)
		if path, _ := newTranscripts().resolve(a); path != old {
			t.Errorf("%q: got %q, want %q", argv, path, old)
		}
	}
}
