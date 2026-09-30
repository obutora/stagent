package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// msg is the comparable part of a wire.Message.
type msg struct{ role, tool, text string }

func strip(ms []wire.Message) []msg {
	out := make([]msg, len(ms))
	for i, m := range ms {
		out[i] = msg{m.Role, m.ToolName, m.Text}
	}
	return out
}

// The fixtures are real transcripts of each harness with every text value
// redacted to a placeholder, keeping record shapes and ordering.
var fixtureWant = map[string][]msg{
	wire.HarnessClaude: {
		{"user", "", "/model"}, // slash command, not its caveat / stdout records
		{"user", "", "content 4"},
		{"assistant", "", "text 1"}, // thinking blocks dropped
		{"tool", "Bash", "command 1"},
		{"tool", "Bash", "command 2"},
		{"tool", "Agent", "description 3"},
		{"tool", "Agent", "description 4"},
		{"user", "", "content 9"}, // isMeta prompt skipped before this
		{"user", "", "content 10"},
		{"assistant", "", "text 4"},
		{"user", "", "content 12"},
		{"assistant", "", "text 5"},
		{"assistant", "", "text 6"},
		// task-notification (automation) skipped
		{"system", "", "Interrupted by user"},
		// meta text and the sidechain record skipped
	},
	wire.HarnessCodex: {
		// developer instructions and <environment_context> skipped
		{"user", "", "text 7"}, // event_msg duplicates ignored
		{"assistant", "", "text 10"},
		{"tool", "exec", "ls 1"}, // command pulled out of code-mode JS
		{"tool", "exec", "ls 2"},
		{"assistant", "", "text 11"},
		{"tool", "exec_command", "cmd 6"},
		{"tool", "apply_patch", "Update /home/user/proj/config.toml"},
		{"assistant", "", "text 12"},
		{"tool", "wait", ""},
		{"user", "", "text 13"},
		{"user", "", "text 14"},
	},
	wire.HarnessOmp: {
		{"user", "", "text 1"},
		{"user", "", "text 2"},
		{"tool", "bash", "command 1"},
		{"tool", "bash", "command 2"},
		{"tool", "bash", "command 3"},
		{"tool", "bash", "command 4"},
		{"tool", "read", "src/file1.ts"},
		{"tool", "todo", "task 1"},
		{"tool", "write", "src/file2.ts"},
		{"tool", "write", "src/file3.ts"},
		{"tool", "bash", "command 5"},
		{"tool", "wait", "i 10"}, // no descriptive argument: falls back to the intent
		{"tool", "read", "src/file4.ts"},
		{"tool", "todo", "i 12"},
		{"tool", "todo", "i 13"},
		{"assistant", "", "text 7"},
		{"user", "", "text 8"},
		{"tool", "write", "src/file5.ts"},
		{"tool", "write", "src/file6.ts"},
		{"tool", "write", "src/file7.ts"},
		{"assistant", "", "text 9"},
		// tool results, custom (system) messages and titles skipped
	},
}

func fixture(h string) string { return filepath.Join("testdata", h+".jsonl") }

func TestParseFixtures(t *testing.T) {
	for h, want := range fixtureWant {
		t.Run(h, func(t *testing.T) {
			p, err := ReadPage(fixture(h), h, 0, maxLimit)
			if err != nil {
				t.Fatal(err)
			}
			if got := strip(p.Messages); !reflect.DeepEqual(got, want) {
				t.Fatalf("messages:\n got %v\nwant %v", got, want)
			}
			if p.Cursor != 0 {
				t.Fatalf("cursor = %d, want 0 (whole file read)", p.Cursor)
			}
			for _, m := range p.Messages {
				if m.Time == 0 {
					t.Fatalf("message without time: %+v", m)
				}
			}
		})
	}
}

func TestBackwardPagingCoversEverythingOnce(t *testing.T) {
	for h, want := range fixtureWant {
		t.Run(h, func(t *testing.T) {
			var pages [][]msg
			before := int64(0)
			for i := 0; ; i++ {
				if i > 50 {
					t.Fatal("paging does not terminate")
				}
				p, err := ReadPage(fixture(h), h, before, 3)
				if err != nil {
					t.Fatal(err)
				}
				if len(p.Messages) < 3 && p.Cursor != 0 {
					t.Fatalf("short page %v with cursor %d", strip(p.Messages), p.Cursor)
				}
				pages = append(pages, strip(p.Messages))
				if p.Cursor == 0 {
					break
				}
				if before != 0 && p.Cursor >= before {
					t.Fatalf("cursor did not move back: %d → %d", before, p.Cursor)
				}
				before = p.Cursor
			}
			var all []msg
			for i := len(pages) - 1; i >= 0; i-- {
				all = append(all, pages[i]...)
			}
			if !reflect.DeepEqual(all, want) {
				t.Fatalf("pages joined:\n got %v\nwant %v", all, want)
			}
		})
	}
}

func TestTailReadsOnlyCompleteNewRecords(t *testing.T) {
	src, err := os.ReadFile(fixture(wire.HarnessOmp))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(src, []byte("\n"))
	lines = lines[:len(lines)-1] // empty element after the final newline
	half := len(lines) / 2
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, bytes.Join(lines[:half], nil), 0o600); err != nil {
		t.Fatal(err)
	}
	head, err := ReadPage(p, wire.HarnessOmp, 0, maxLimit)
	if err != nil {
		t.Fatal(err)
	}
	off, err := EndOffset(p)
	if err != nil || off != head.End {
		t.Fatalf("EndOffset = %d, %v; page end %d", off, err, head.End)
	}

	rest := bytes.Join(lines[half:], nil)
	cut := len(rest) - 25 // inside the last record
	appendTo(t, p, rest[:cut])
	got1, off, err := ReadFrom(p, wire.HarnessOmp, off)
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(src)-len(lines[len(lines)-1])) {
		t.Fatalf("offset %d stops inside a record (want %d)", off, len(src)-len(lines[len(lines)-1]))
	}
	got2, off2, err := ReadFrom(p, wire.HarnessOmp, off)
	if err != nil || len(got2) != 0 || off2 != off {
		t.Fatalf("re-read without new data: %v %d %v", strip(got2), off2, err)
	}
	appendTo(t, p, rest[cut:])
	got3, off, err := ReadFrom(p, wire.HarnessOmp, off)
	if err != nil {
		t.Fatal(err)
	}
	if off != int64(len(src)) {
		t.Fatalf("final offset %d, want %d", off, len(src))
	}
	all := append(append(strip(head.Messages), strip(got1)...), strip(got3)...)
	if !reflect.DeepEqual(all, fixtureWant[wire.HarnessOmp]) {
		t.Fatalf("head+tail:\n got %v\nwant %v", all, fixtureWant[wire.HarnessOmp])
	}
}

func TestPagingSkipsOversizedRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.jsonl")
	user := func(text string) string {
		b, _ := json.Marshal(map[string]any{"type": "message", "message": map[string]any{
			"role": "user", "attribution": "user", "timestamp": 1,
			"content": []any{map[string]any{"type": "text", "text": text}}}})
		return string(b) + "\n"
	}
	body := user("first") + user(strings.Repeat("x", maxLine+10)) + user("last")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	pg, err := ReadPage(p, wire.HarnessOmp, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []msg{{"user", "", "first"}, {"user", "", "last"}}
	if got := strip(pg.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func appendTo(t *testing.T, p string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func TestSummarize(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"command":"ls -la\n  /tmp","description":"List"}`, "ls -la /tmp"},
		{`{"file_path":"/a/b.go","old_string":"x"}`, "/a/b.go"},
		{`"{\"cmd\":\"git status\",\"workdir\":\"/r\"}"`, "git status"}, // Codex arguments string
		{`{"command":["bash","-lc","make test"]}`, "bash -lc make test"},
		{`{"command":"*** Begin Patch\n*** Add File: a.txt\n+x\n*** Delete File: b.txt\n*** End Patch"}`, "Add a.txt, Delete b.txt"},
		{`{"questions":[1]}`, ""},
		{`not json`, ""},
	}
	for _, c := range cases {
		if got := Summarize(json.RawMessage(c.in)); got != c.want {
			t.Errorf("Summarize(%s) = %q, want %q", c.in, got, c.want)
		}
	}
	long := Summarize(json.RawMessage(`{"command":"` + strings.Repeat("あ", 300) + `"}`))
	if len(long) > maxSummary || !strings.HasSuffix(long, "…") {
		t.Errorf("long summary not clipped on a rune boundary: %d bytes", len(long))
	}
}

// ---------------------------------------------------------------------------
// Conversation index

type tree struct {
	roots  Roots
	claude string // listed Claude transcript
	codex  string
	omp    string
}

func setupTree(t *testing.T) tree {
	t.Helper()
	home := t.TempDir()
	r := Roots{
		Claude: filepath.Join(home, ".claude", "projects"),
		Codex:  filepath.Join(home, ".codex"),
		Omp:    filepath.Join(home, ".omp", "agent", "sessions"),
	}
	tr := tree{roots: r}
	copyFile := func(src, dst string, mtime time.Time) string {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dst, b, mtime)
		return dst
	}
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	tr.claude = copyFile(fixture("claude"), filepath.Join(r.Claude, "-home-user-proj", "2a425a50-3f53-4e94-8acb-f2d54d242f33.jsonl"), base.Add(3*time.Hour))
	// claude -p run (automation) and a sub-agent transcript: never listed.
	writeFile(t, filepath.Join(r.Claude, "-home-user-proj", "11111111-2222-4333-8444-555555555555.jsonl"),
		[]byte(`{"type":"user","entrypoint":"sdk-cli","cwd":"/home/user/proj","message":{"content":"x"}}`+"\n"), base.Add(5*time.Hour))
	writeFile(t, filepath.Join(r.Claude, "-home-user-proj", "2a425a50-3f53-4e94-8acb-f2d54d242f33", "subagents", "agent-a1.jsonl"),
		[]byte(`{"type":"user","entrypoint":"cli","cwd":"/x"}`+"\n"), base.Add(6*time.Hour))

	tr.codex = copyFile(fixture("codex"), filepath.Join(r.Codex, "sessions", "2026", "09", "13", "rollout-2026-09-13T14-03-56-01a09926-8fc8-79c3-b589-f905ca3cd163.jsonl"), base.Add(2*time.Hour))
	writeFile(t, filepath.Join(r.Codex, "sessions", "2026", "09", "14", "rollout-2026-09-14T00-00-00-01a0aaaa-0000-7000-8000-000000000000.jsonl"),
		[]byte(`{"type":"session_meta","payload":{"id":"01a0aaaa-0000-7000-8000-000000000000","cwd":"/p","originator":"codex_exec","source":"exec"}}`+"\n"), base.Add(4*time.Hour))
	writeFile(t, filepath.Join(r.Codex, "session_index.jsonl"),
		[]byte(`{"id":"01a09926-8fc8-79c3-b589-f905ca3cd163","thread_name":"old name"}`+"\n"+
			`{"id":"01a09926-8fc8-79c3-b589-f905ca3cd163","thread_name":"Improve the landing page"}`+"\n"), base)

	tr.omp = copyFile(fixture("omp"), filepath.Join(r.Omp, "-proj", "2026-09-28T23-49-45-189Z_01a0ea6c-aa65-7290-9f3f-6beb480ab112.jsonl"), base.Add(1*time.Hour))
	writeFile(t, filepath.Join(r.Omp, "-proj", "2026-09-28T23-49-45-189Z_01a0ea6c-aa65-7290-9f3f-6beb480ab112", "sub.jsonl"),
		[]byte(`{"type":"session","id":"sub","cwd":"/p"}`+"\n"), base.Add(7*time.Hour))
	return tr
}

func writeFile(t *testing.T, p string, b []byte, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestIndexListsInteractiveConversationsNewestFirst(t *testing.T) {
	tr := setupTree(t)
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), tr.roots)
	got, err := x.List(nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	type conv struct{ harness, id, cwd, title, path string }
	var gotC []conv
	for _, c := range got {
		gotC = append(gotC, conv{c.Harness, c.ID, c.Cwd, c.Title, c.Path})
	}
	want := []conv{
		{"claude", "2a425a50-3f53-4e94-8acb-f2d54d242f33", "/home/user/proj", "My renamed session", tr.claude},
		{"codex", "01a09926-8fc8-79c3-b589-f905ca3cd163", "/home/user/proj", "Improve the landing page", tr.codex},
		{"omp", "01a0ea6c-aa65-7290-9f3f-6beb480ab112", "/home/user/proj", "title 1", tr.omp},
	}
	if !reflect.DeepEqual(gotC, want) {
		t.Fatalf("List:\n got %v\nwant %v", gotC, want)
	}
	if got[0].UpdatedAt <= got[1].UpdatedAt {
		t.Fatalf("not newest first: %d, %d", got[0].UpdatedAt, got[1].UpdatedAt)
	}

	only, err := x.List([]string{wire.HarnessCodex}, 10)
	if err != nil || len(only) != 1 || only[0].Harness != wire.HarnessCodex {
		t.Fatalf("harness filter: %+v %v", only, err)
	}
}

func TestIndexRereadsOnlyChangedFiles(t *testing.T) {
	tr := setupTree(t)
	idxPath := filepath.Join(t.TempDir(), "index.json")
	x := OpenIndex(idxPath, tr.roots)
	if _, err := x.List(nil, 10); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(idxPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("index file: %v %v", st, err)
	}

	// Same size and mtime: a reopened index trusts the cache and does not
	// read the file (the rewritten title stays unseen).
	st, _ := os.Stat(tr.omp)
	b, _ := os.ReadFile(tr.omp)
	b = bytes.Replace(b, []byte(`"title":"title 1"`), []byte(`"title":"TITLE X"`), 1)
	writeFile(t, tr.omp, b, st.ModTime())
	x = OpenIndex(idxPath, tr.roots)
	got, _ := x.List([]string{wire.HarnessOmp}, 10)
	if len(got) != 1 || got[0].Title != "title 1" {
		t.Fatalf("unchanged file was re-read: %+v", got)
	}

	// A grown Claude transcript is rescanned from where the scan stopped.
	appendTo(t, tr.claude, []byte(`{"type":"custom-title","customTitle":"Renamed again","sessionId":"x"}`+"\n"))
	later := time.Now()
	os.Chtimes(tr.claude, later, later)
	got, _ = x.List([]string{wire.HarnessClaude}, 10)
	if len(got) != 1 || got[0].Title != "Renamed again" {
		t.Fatalf("grown file: %+v", got)
	}
	// Deleted files leave the cache.
	os.Remove(tr.claude)
	got, _ = x.List([]string{wire.HarnessClaude}, 10)
	if len(got) != 0 {
		t.Fatalf("deleted file still listed: %+v", got)
	}
}

func TestIndexFind(t *testing.T) {
	tr := setupTree(t)
	x := OpenIndex("", tr.roots) // nothing cached: falls back to the file names
	for _, c := range []struct{ harness, id, want string }{
		{"claude", "2a425a50-3f53-4e94-8acb-f2d54d242f33", tr.claude},
		{"codex", "01a09926-8fc8-79c3-b589-f905ca3cd163", tr.codex},
		{"omp", "01a0ea6c-aa65-7290-9f3f-6beb480ab112", tr.omp},
	} {
		if p, ok := x.Find(c.harness, c.id); !ok || p != c.want {
			t.Errorf("Find(%s, %s) = %q %v, want %q", c.harness, c.id, p, ok, c.want)
		}
	}
	if _, ok := x.Find("claude", "00000000-0000-4000-8000-000000000000"); ok {
		t.Error("found a missing conversation")
	}
}

func TestHarnessOfConfinesPaths(t *testing.T) {
	tr := setupTree(t)
	cases := []struct {
		path, harness string
		ok            bool
	}{
		{tr.claude, "claude", true},
		{tr.codex, "codex", true},
		{tr.omp, "omp", true},
		{filepath.Join(tr.roots.Claude, "..", "settings.json"), "", false},
		{filepath.Join(tr.roots.Claude, "..", "..", "x.jsonl"), "", false},
		{filepath.Join(tr.roots.Codex, "history.jsonl"), "", false}, // not under sessions/
		{"relative/a.jsonl", "", false},
	}
	for _, c := range cases {
		h, ok := tr.roots.HarnessOf(c.path)
		if h != c.harness || ok != c.ok {
			t.Errorf("HarnessOf(%s) = %q %v, want %q %v", c.path, h, ok, c.harness, c.ok)
		}
	}
}
