package follow

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// copilotSession writes a Copilot CLI session of cwd,
// <root>/<id>/events.jsonl with its workspace.yaml, last written at mtime.
func copilotSession(t *testing.T, root, cwd string, created, mtime time.Time, n int) string {
	t.Helper()
	id := v7(created, n)
	dir := filepath.Join(root, id)
	write(t, filepath.Join(dir, "workspace.yaml"), "id: "+id+"\ncwd: "+cwd+"\n", mtime)
	return write(t, filepath.Join(dir, "events.jsonl"),
		fmt.Sprintf(`{"type":"session.start","id":"e1","parentId":null,"timestamp":%q,"data":{"sessionId":%q,"context":{"cwd":%q}}}`+"\n", created.UTC().Format(time.RFC3339Nano), id, cwd)+
			`{"type":"user.message","id":"e2","parentId":"e1","timestamp":"2026-10-19T00:00:00.000Z","data":{"content":"hi"}}`+"\n", mtime)
}

// Copilot CLI is found by a file of its session directory it has open
// (its event log, or any other file there), else by the session its
// command line resumes, else as the newest session of its directory.
func TestCopilotTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	roots := transcript.Roots{wire.HarnessCopilot: root}
	start := time.Date(2026, 10, 19, 16, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	old := copilotSession(t, root, "/home/user/proj", start.Add(-day), start.Add(-day), 1)
	other := copilotSession(t, root, "/home/user/other", start.Add(time.Minute), start.Add(time.Minute), 2)
	fresh := copilotSession(t, root, "/home/user/proj", start.Add(2*time.Minute), start.Add(2*time.Minute), 3)
	a := agentFacts{harness: wire.HarnessCopilot, pids: []int{9, 10}, start: start, cwd: "/home/user/proj", roots: roots,
		argv: argvOf("copilot"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Fatalf("newest of its directory: got %q, want %q", path, fresh)
	}

	// The native binary (pid 10, under its node launcher) has a file of
	// another session's directory open.
	a.openFiles = func(pid int) []string {
		if pid == 10 {
			return []string{"/dev/null", filepath.Join(filepath.Dir(other), "session.db-wal")}
		}
		return nil
	}
	if path, _ := newTranscripts().resolve(a); path != other {
		t.Fatalf("open file: got %q, want %q", path, other)
	}
	a.openFiles = noOpenFiles

	for _, argv := range [][]string{
		{"copilot", "--resume=" + idOf(old)},
		{"copilot", "--model", "gpt-5", "--resume", idOf(old)},
		{"copilot", "-r", idOf(old)},
		{"copilot", "--session-id", idOf(old)},
		{"copilot", "--session-id=" + idOf(old)},
	} {
		a.argv = argvOf(argv...)
		if path, _ := newTranscripts().resolve(a); path != old {
			t.Errorf("%q: got %q, want %q", argv, path, old)
		}
	}
}

func idOf(events string) string { return filepath.Base(filepath.Dir(events)) }
