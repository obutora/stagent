package follow

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// grokSession writes a Grok Build session of cwd,
// <root>/<cwd percent-encoded>/<id>/updates.jsonl with its summary.json,
// last written at mtime.
func grokSession(t *testing.T, root, cwd string, created, mtime time.Time, n int) string {
	t.Helper()
	id := v7(created, n)
	dir := filepath.Join(root, strings.ReplaceAll(url.QueryEscape(cwd), "+", "%20"), id)
	write(t, filepath.Join(dir, "summary.json"), fmt.Sprintf(`{"info":{"id":%q,"cwd":%q}}`, id, cwd), mtime)
	return write(t, filepath.Join(dir, "updates.jsonl"),
		fmt.Sprintf(`{"timestamp":%d,"method":"session/update","params":{"sessionId":%q,"update":{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"hi"}}}}`+"\n", created.Unix(), id), mtime)
}

// Grok Build is found by a file of its session directory it has open (the
// lock it holds on one of them), else by the session its command line
// resumes, else as the newest session of its directory.
func TestGrokTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	roots := transcript.Roots{wire.HarnessGrok: root}
	start := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	old := grokSession(t, root, "/home/user/my proj", start.Add(-day), start.Add(-day), 1)
	other := grokSession(t, root, "/home/user/other", start.Add(time.Minute), start.Add(time.Minute), 2)
	fresh := grokSession(t, root, "/home/user/my proj", start.Add(2*time.Minute), start.Add(2*time.Minute), 3)
	a := agentFacts{harness: wire.HarnessGrok, pids: []int{9}, start: start, cwd: "/home/user/my proj", roots: roots,
		argv: argvOf("/home/user/.grok/bin/grok"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Fatalf("newest of its directory: got %q, want %q", path, fresh)
	}

	a.openFiles = func(int) []string {
		return []string{"/dev/null", filepath.Join(filepath.Dir(other), "chat_history.jsonl.lock")}
	}
	if path, _ := newTranscripts().resolve(a); path != other {
		t.Fatalf("open file: got %q, want %q", path, other)
	}
	a.openFiles = noOpenFiles

	id := filepath.Base(filepath.Dir(old))
	for _, argv := range [][]string{
		{"/home/user/.grok/bin/grok", "--resume", id},
		{"/home/user/.grok/bin/grok", "-m", "grok-4", "-r", id},
		{"/home/user/.grok/bin/grok", "--resume=" + id},
		{"/home/user/.grok/bin/grok", "-s", id},
	} {
		a.argv = argvOf(argv...)
		if path, _ := newTranscripts().resolve(a); path != old {
			t.Errorf("%q: got %q, want %q", argv, path, old)
		}
	}
}
