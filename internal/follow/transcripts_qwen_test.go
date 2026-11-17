package follow

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// qwenSession writes a Qwen Code conversation of cwd,
// <root>/<cwd as Qwen Code spells it>/chats/<id>.jsonl, last written at
// mtime, and its runtime sidecar naming pid when pid is not 0.
func qwenSession(t *testing.T, root, cwd string, n, pid int, started, mtime time.Time) string {
	t.Helper()
	id := fmt.Sprintf("a2730fef-0f6d-48da-9317-%012d", n)
	chats := filepath.Join(root, qwenProjectDir(cwd), "chats")
	if pid != 0 {
		write(t, filepath.Join(chats, id+".runtime.json"), fmt.Sprintf(`{"schema_version":1,"pid":%d,"session_id":%q,"work_dir":%q,"started_at":%.3f,"qwen_version":"0.27.4"}`,
			pid, id, cwd, float64(started.UnixMilli())/1000), mtime)
	}
	return write(t, filepath.Join(chats, id+".jsonl"),
		fmt.Sprintf(`{"uuid":"u1","parentUuid":null,"sessionId":%q,"timestamp":%q,"type":"user","cwd":%q,"version":"0.27.4","message":{"role":"user","parts":[{"text":"hi"}]}}`+"\n",
			id, started.UTC().Format(time.RFC3339Nano), cwd), mtime)
}

// Qwen Code is found by the runtime sidecar it writes for its process
// (its pid, since when it runs), else by the conversation its command line
// resumes, else as the newest conversation of its directory. A sidecar a
// process of the same pid left behind earlier does not count.
func TestQwenTranscript(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	roots := transcript.Roots{wire.HarnessQwen: root}
	start := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cwd := "/home/user/proj.v2"
	old := qwenSession(t, root, cwd, 1, 0, start.Add(-day), start.Add(-day))
	mine := qwenSession(t, root, cwd, 2, 4911, start, start.Add(time.Minute))
	newer := qwenSession(t, root, cwd, 3, 5000, start.Add(time.Minute), start.Add(2*time.Minute))
	a := agentFacts{harness: wire.HarnessQwen, pids: []int{4911}, start: start, cwd: cwd, roots: roots,
		argv: argvOf("node", "/usr/bin/qwen"), openFiles: noOpenFiles, claimed: noClaims}
	if path, _ := newTranscripts().resolve(a); path != mine {
		t.Fatalf("runtime sidecar: got %q, want %q", path, mine)
	}

	// The same pid, but a process that started a day later: the sidecar
	// is an older process's; the newest conversation of its directory.
	a.start = start.Add(day)
	newer = qwenSession(t, root, cwd, 3, 5000, start.Add(day), start.Add(day+time.Minute))
	if path, _ := newTranscripts().resolve(a); path != newer {
		t.Fatalf("stale sidecar: got %q, want %q", path, newer)
	}

	a.pids = []int{7}
	for _, argv := range [][]string{
		{"node", "/usr/bin/qwen", "--resume", idOfQwen(old)},
		{"node", "/usr/bin/qwen", "--approval-mode", "default", "-r", idOfQwen(old)},
		{"node", "/usr/bin/qwen", "--resume=" + idOfQwen(old)},
	} {
		a.argv = argvOf(argv...)
		if path, _ := newTranscripts().resolve(a); path != old {
			t.Errorf("%q: got %q, want %q", argv, path, old)
		}
	}
}

func idOfQwen(path string) string {
	return filepath.Base(path)[:len(filepath.Base(path))-len(".jsonl")]
}

// Qwen Code names a project's directory after its path, every character
// but [A-Za-z0-9] a dash; on Windows it lowercases the path first.
func TestQwenProjectDir(t *testing.T) {
	for in, want := range map[string]string{
		"/Volumes/SPStreamS05/orca/work/proj": "-Volumes-SPStreamS05-orca-work-proj",
		"/home/u/my_app.v2":                   "-home-u-my-app-v2",
		"/home/u/日本":                          "-home-u---",
	} {
		if runtime.GOOS == "windows" {
			want = strings.ToLower(want)
		}
		if got := qwenProjectDir(in); got != want {
			t.Errorf("qwenProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}
