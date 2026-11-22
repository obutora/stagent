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

// agyConversation writes an Antigravity CLI conversation of cwd: its
// transcript_full.jsonl last written at mtime, and a prompt of it in
// history.jsonl (where its cwd is read from).
func agyConversation(t *testing.T, home, id, cwd string, mtime time.Time) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(home, "history.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, `{"display":"hi","timestamp":%d,"workspace":%q,"conversationId":%q}`+"\n", mtime.UnixMilli(), cwd, id)
	f.Close()
	return write(t, filepath.Join(home, "brain", id, ".system_generated", "logs", "transcript_full.jsonl"),
		fmt.Sprintf(`{"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": %q, "content": "<USER_REQUEST>\nhi\n</USER_REQUEST>"}`+"\n", mtime.UTC().Format(time.RFC3339)), mtime)
}

// Antigravity CLI is found by the presence lock (or the database) of the
// conversation it shows, which it holds open, else by the conversation its
// command line resumes, else as the newest conversation of its directory.
func TestAntigravityTranscript(t *testing.T) {
	home := filepath.Join(t.TempDir(), "antigravity-cli")
	roots := transcript.Roots{wire.HarnessAntigravity: home}
	start := time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	oldID, otherID := "215bbd83-2b01-4c76-a48b-35a1781e64b4", "306ebb4e-83c6-48af-9e13-360d85fc1548"
	old := agyConversation(t, home, oldID, "/home/user/my proj", start.Add(-day))
	other := agyConversation(t, home, otherID, "/home/user/other", start.Add(time.Minute))
	fresh := agyConversation(t, home, "44c0bdd4-8aa4-4a77-a330-d614c70337f3", "/home/user/my proj", start.Add(2*time.Minute))
	a := agentFacts{harness: wire.HarnessAntigravity, pids: []int{9}, start: start, cwd: "/home/user/my proj", roots: roots,
		argv: argvOf("agy"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Fatalf("newest of its directory: got %q, want %q", path, fresh)
	}

	for _, open := range []string{
		filepath.Join(home, "presence", otherID+".lock"),
		filepath.Join(home, "conversations", otherID+".db-wal"),
	} {
		a.openFiles = func(int) []string {
			return []string{"/dev/null", filepath.Join(home, "history.jsonl"), open}
		}
		if path, _ := newTranscripts().resolve(a); path != other {
			t.Fatalf("open %s: got %q, want %q", open, path, other)
		}
	}
	// The lock of a conversation without a transcript yet names none.
	a.openFiles = func(int) []string {
		return []string{filepath.Join(home, "presence", "0b6f3c1e-0000-4000-8000-000000000009.lock")}
	}
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Fatalf("lock without a transcript: got %q, want %q", path, fresh)
	}
	a.openFiles = noOpenFiles

	for _, argv := range [][]string{
		{"agy", "--conversation", oldID},
		{"agy", "--settings", `{"agentMode":""}`, "--conversation", oldID},
		{"/home/user/.local/bin/agy", "--conversation=" + oldID},
	} {
		a.argv = argvOf(argv...)
		if path, _ := newTranscripts().resolve(a); path != old {
			t.Errorf("%q: got %q, want %q", argv, path, old)
		}
	}
	// --continue resumes the most recent conversation: the newest of its
	// directory, whenever it was created.
	a.argv = argvOf("agy", "-c")
	if path, _ := newTranscripts().resolve(a); path != fresh {
		t.Errorf("-c: got %q, want %q", path, fresh)
	}
}
