package transcript

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/harness"
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
	// Written by Pi 1.3.0's own SessionManager (v3).
	wire.HarnessPi: {
		{"user", "", "The test for parseConfig fails, fix it"}, // string content
		{"assistant", "", "Let me run the tests."},             // thinking dropped
		{"tool", "bash", "npm test -- config"},
		{"tool", "read", "src/config.ts"},
		{"tool", "read", "src/config.test.ts"},
		{"assistant", "", "The retries default is missing."},
		{"tool", "edit", "src/config.ts"},
		{"assistant", "", "Fixed: parseConfig now defaults retries to 3, and the test passes."},
		// custom_message, custom and bashExecution messages skipped
		{"user", "", "Also explain this screenshot\n\n[image]"},
		// role=system (prompt sections) skipped
		{"assistant", "", "The screenshot shows the failing test output."},
		// tool results, session_info, usage skipped
	},
	// Not a capture: Copilot CLI 1.0.98's records as its published schema
	// (schemas/session-events.schema.json) shapes them, with records
	// stagent does not read, one of a type it has never seen and one cut
	// short in the middle.
	wire.HarnessCopilot: {
		{"user", "", "The test for parseConfig fails, fix it"}, // not transformedContent
		{"assistant", "", "I'll look at the test first."},      // reasoningText dropped
		{"tool", "view", "/home/user/proj/config_test.go"},
		{"tool", "bash", "go test ./..."},
		{"tool", "task", "Look for other callers"},
		// tool runs, permissions, the sub-agents' messages (agentId,
		// parentToolCallId), the unknown and the broken record and the
		// skill's hidden prompt skipped
		{"assistant", "", "Fixed: parseConfig now retries on EAGAIN, and the test passes."},
		{"user", "", "Thanks! Commit it"},
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
	roots   Roots
	claude  string // listed Claude transcript
	codex   string
	omp     string
	pi      string
	copilot string
}

func setupTree(t *testing.T) tree {
	t.Helper()
	home := t.TempDir()
	claudeRoot := filepath.Join(home, ".claude", "projects")
	codexRoot := filepath.Join(home, ".codex")
	ompRoot := filepath.Join(home, ".omp", "agent", "sessions")
	piRoot := filepath.Join(home, ".pi", "agent", "sessions")
	copilotRoot := filepath.Join(home, ".copilot", "session-state")
	tr := tree{roots: Roots{wire.HarnessClaude: claudeRoot, wire.HarnessCodex: codexRoot, wire.HarnessOmp: ompRoot, wire.HarnessPi: piRoot, wire.HarnessCopilot: copilotRoot}}
	copyFile := func(src, dst string, mtime time.Time) string {
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, dst, b, mtime)
		return dst
	}
	base := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)

	tr.claude = copyFile(fixture("claude"), filepath.Join(claudeRoot, "-home-user-proj", "2a425a50-3f53-4e94-8acb-f2d54d242f33.jsonl"), base.Add(3*time.Hour))
	// claude -p run (automation) and a sub-agent transcript: never listed.
	writeFile(t, filepath.Join(claudeRoot, "-home-user-proj", "11111111-2222-4333-8444-555555555555.jsonl"),
		[]byte(`{"type":"user","entrypoint":"sdk-cli","cwd":"/home/user/proj","message":{"content":"x"}}`+"\n"), base.Add(5*time.Hour))
	writeFile(t, filepath.Join(claudeRoot, "-home-user-proj", "2a425a50-3f53-4e94-8acb-f2d54d242f33", "subagents", "agent-a1.jsonl"),
		[]byte(`{"type":"user","entrypoint":"cli","cwd":"/x"}`+"\n"), base.Add(6*time.Hour))

	tr.codex = copyFile(fixture("codex"), filepath.Join(codexRoot, "sessions", "2026", "09", "13", "rollout-2026-09-13T14-03-56-01a09926-8fc8-79c3-b589-f905ca3cd163.jsonl"), base.Add(2*time.Hour))
	writeFile(t, filepath.Join(codexRoot, "sessions", "2026", "09", "14", "rollout-2026-09-14T00-00-00-01a0aaaa-0000-7000-8000-000000000000.jsonl"),
		[]byte(`{"type":"session_meta","payload":{"id":"01a0aaaa-0000-7000-8000-000000000000","cwd":"/p","originator":"codex_exec","source":"exec"}}`+"\n"), base.Add(4*time.Hour))
	writeFile(t, filepath.Join(codexRoot, "session_index.jsonl"),
		[]byte(`{"id":"01a09926-8fc8-79c3-b589-f905ca3cd163","thread_name":"old name"}`+"\n"+
			`{"id":"01a09926-8fc8-79c3-b589-f905ca3cd163","thread_name":"Improve the landing page"}`+"\n"), base)

	tr.omp = copyFile(fixture("omp"), filepath.Join(ompRoot, "-proj", "2026-09-28T23-49-45-189Z_01a0ea6c-aa65-7290-9f3f-6beb480ab112.jsonl"), base.Add(1*time.Hour))
	writeFile(t, filepath.Join(ompRoot, "-proj", "2026-09-28T23-49-45-189Z_01a0ea6c-aa65-7290-9f3f-6beb480ab112", "sub.jsonl"),
		[]byte(`{"type":"session","id":"sub","cwd":"/p"}`+"\n"), base.Add(7*time.Hour))

	tr.pi = copyFile(fixture("pi"), filepath.Join(piRoot, "--home-user-proj--", "2026-10-19T15-17-10-242Z_01a154bc-ede2-7449-9df3-bd69c074f6dc.jsonl"), base.Add(30*time.Minute))

	copilotDir := filepath.Join(copilotRoot, "0193f2c4-5e1a-7c3b-9d2e-4f6a8b0c1d2e")
	tr.copilot = copyFile(fixture("copilot"), filepath.Join(copilotDir, "events.jsonl"), base.Add(15*time.Minute))
	writeFile(t, filepath.Join(copilotDir, "workspace.yaml"), []byte(copilotWorkspaceYAML), base.Add(15*time.Minute))
	// A session that never got a conversation: no events.jsonl.
	writeFile(t, filepath.Join(copilotRoot, "0193f2c4-0000-7000-8000-000000000000", "workspace.yaml"), []byte("cwd: /p\n"), base.Add(8*time.Hour))
	return tr
}

// copilotWorkspaceYAML is a workspace.yaml of Copilot CLI 1.0.98 [INFERENCE:
// its fields are those the bundle compares (cli-main.js) and the runtime's
// WorkspaceMetadata names, not seen on disk].
const copilotWorkspaceYAML = `id: 0193f2c4-5e1a-7c3b-9d2e-4f6a8b0c1d2e
cwd: /home/user/proj
git_root: /home/user/proj
repository: user/proj
host_type: github
branch: main
name: "Fix parseConfig: retries"
user_named: false
summary: Fixed parseConfig
created_at: 2026-10-19T15:20:00.000Z
updated_at: 2026-10-19T15:21:02.000Z
`

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
		{"pi", "01a154bc-ede2-7449-9df3-bd69c074f6dc", "/home/user/proj", "parseConfig retries", tr.pi}, // the last session_info
		// workspace.yaml's name; the session without events.jsonl is not listed.
		{"copilot", "0193f2c4-5e1a-7c3b-9d2e-4f6a8b0c1d2e", "/home/user/proj", "Fix parseConfig: retries", tr.copilot},
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
	// Windows has no permission bits: Go reports 0666 for any writable file.
	if st, err := os.Stat(idxPath); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
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
		{"pi", "01a154bc-ede2-7449-9df3-bd69c074f6dc", tr.pi},
		{"copilot", "0193f2c4-5e1a-7c3b-9d2e-4f6a8b0c1d2e", tr.copilot},
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
		{tr.pi, "pi", true},
		{tr.copilot, "copilot", true},
		{filepath.Join(tr.roots[wire.HarnessCopilot], "..", "settings.json"), "", false},
		{filepath.Join(tr.roots[wire.HarnessClaude], "..", "settings.json"), "", false},
		{filepath.Join(tr.roots[wire.HarnessClaude], "..", "..", "x.jsonl"), "", false},
		{filepath.Join(tr.roots[wire.HarnessCodex], "history.jsonl"), "", false}, // not under sessions/
		{"relative/a.jsonl", "", false},
	}
	for _, c := range cases {
		h, ok := tr.roots.HarnessOf(c.path)
		if h != c.harness || ok != c.ok {
			t.Errorf("HarnessOf(%s) = %q %v, want %q %v", c.path, h, ok, c.harness, c.ok)
		}
	}
}

// Every harness of the registry has a transcript format.
func TestFormatsCoverRegistry(t *testing.T) {
	for _, id := range harness.Missing(formats) {
		t.Errorf("harness %q has no entry in formats", id)
	}
	for id, f := range formats {
		if f.dir == nil || f.enumerate == nil || f.named == nil || f.scan == nil || f.titler == nil || f.parse == nil {
			t.Errorf("formats[%q] leaves a function unset: %+v", id, f)
		}
	}
}

// A Pi session is append-only: a grown file is read from where the last
// scan stopped, a later session_info renames it, and one without any is
// titled by its first prompt.
func TestPiTitle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	roots := Roots{wire.HarnessPi: dir}
	p := filepath.Join(dir, "--work--", "2026-10-19T15-17-10-242Z_01a154bc-0000-7000-8000-000000000001.jsonl")
	mtime := time.Now().Add(-time.Hour)
	writeFile(t, p, []byte(`{"type":"session","version":3,"id":"01a154bc-0000-7000-8000-000000000001","timestamp":"2026-10-19T15:17:10.242Z","cwd":"/work"}`+"\n"+
		`{"type":"message","id":"a","parentId":null,"timestamp":"2026-10-19T15:17:11.000Z","message":{"role":"user","content":[{"type":"text","text":"Add a\nREADME"}],"timestamp":1}}`+"\n"), mtime)
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), roots)
	got, err := x.List(nil, 10)
	if err != nil || len(got) != 1 || got[0].Title != "Add a README" || got[0].Cwd != "/work" {
		t.Fatalf("first prompt as title: %+v %v", got, err)
	}
	appendTo(t, p, []byte(`{"type":"session_info","id":"b","parentId":"a","timestamp":"2026-10-19T15:18:00.000Z","name":"Docs"}`+"\n"))
	os.Chtimes(p, time.Now(), time.Now())
	if got, _ = x.List(nil, 10); len(got) != 1 || got[0].Title != "Docs" {
		t.Fatalf("renamed: %+v", got)
	}
}

// A Copilot CLI session is titled by its workspace.yaml: its name (Copilot
// CLI names it, the user may rename it), else its summary, else the first
// prompt. Without workspace.yaml the cwd is session.start's and the id the
// directory's.
func TestCopilotTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "session-state")
	roots := Roots{wire.HarnessCopilot: root}
	dir := filepath.Join(root, "0193f2c4-0000-7000-8000-000000000001")
	p := filepath.Join(dir, "events.jsonl")
	mtime := time.Now().Add(-time.Hour)
	writeFile(t, p, []byte(`{"type":"session.start","id":"a","parentId":null,"timestamp":"2026-10-19T15:20:00.000Z","data":{"sessionId":"0193f2c4-0000-7000-8000-000000000001","context":{"cwd":"/work"}}}`+"\n"+
		`{"type":"user.message","id":"b","parentId":"a","timestamp":"2026-10-19T15:20:05.000Z","data":{"content":"Add a\nREADME"}}`+"\n"), mtime)
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), roots)
	got, err := x.List(nil, 10)
	if err != nil || len(got) != 1 || got[0].Title != "Add a README" || got[0].Cwd != "/work" || got[0].ID != "0193f2c4-0000-7000-8000-000000000001" {
		t.Fatalf("first prompt as title: %+v %v", got, err)
	}

	writeFile(t, filepath.Join(dir, "workspace.yaml"), []byte("cwd: /work/sub\nsummary: 'Write the README'\n"), time.Now())
	appendTo(t, p, []byte(`{"type":"assistant.message","id":"c","parentId":"b","timestamp":"2026-10-19T15:20:06.000Z","data":{"messageId":"m","content":"Done."}}`+"\n"))
	os.Chtimes(p, time.Now(), time.Now())
	if got, _ = x.List(nil, 10); len(got) != 1 || got[0].Title != "Write the README" || got[0].Cwd != "/work/sub" {
		t.Fatalf("summary: %+v", got)
	}

	writeFile(t, filepath.Join(dir, "workspace.yaml"), []byte("cwd: /work/sub\nname: Docs\nuser_named: true\nsummary: Write the README\n"), time.Now())
	appendTo(t, p, []byte(`{"type":"session.shutdown","id":"d","parentId":"c","timestamp":"2026-10-19T15:21:00.000Z","data":{"shutdownType":"routine"}}`+"\n"))
	later := time.Now().Add(time.Second)
	os.Chtimes(p, later, later)
	if got, _ = x.List(nil, 10); len(got) != 1 || got[0].Title != "Docs" {
		t.Fatalf("renamed: %+v", got)
	}
}
