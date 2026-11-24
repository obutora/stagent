package relay

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// docSource is a relay source in a checkout at /home/user/proj, as the
// transcript fixtures' conversations ran.
func docSource(harness string) wire.RelaySource {
	return wire.RelaySource{
		Host: wire.RelayHost{Label: "work-mac"}, Harness: harness, ConversationID: "conv-1",
		Title: "Fix it", State: wire.StateIdle, Cwd: "/home/user/proj", Repo: "/home/user/proj",
		Branch: "main", Head: "abc1234", Base: "abc1234", PreparedAt: time.Date(2026, 11, 24, 1, 2, 3, 0, time.UTC).UnixMilli(),
	}
}

func readDoc(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, DocumentFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Each harness's transcript makes a document that holds its tool outputs,
// the paths it touched (relative to the worktree's root when inside it)
// and its images, never its thinking.
func TestDocumentFromFixtures(t *testing.T) {
	cases := map[string]struct {
		output, thinking string
		touched          string // as listed ("": none)
		images           []string
	}{
		wire.HarnessClaude:      {"content 5", "thinking 1", "main.go", []string{"0001.png"}},
		wire.HarnessCodex:       {"output 1", "reasoning summary 1", "config.toml", []string{"0001.png"}},
		wire.HarnessOmp:         {"text 3", "thinking 1", "src/file2.ts", []string{"0001.webp"}},
		wire.HarnessPi:          {"FAIL src/config.test.ts", "Run the tests first.", "src/config.ts", []string{"0001.png"}},
		wire.HarnessCopilot:     {"package config", "The user wants the test fixed.", "", nil},
		wire.HarnessQwen:        {"hello-from-mock", "Weighing the hunks first.", "mock_write.txt", nil},
		wire.HarnessOpenCode:    {"Created file successfully: mock_write.txt", "Let me look.", "mock_write.txt", nil},
		wire.HarnessGrok:        {"exit: 0", "Reading the diff first.", "/home/u/proj/mock_write.txt", nil}, // outside the worktree: as written
		wire.HarnessAntigravity: {"HTTP/2 200", "Reading the headers.", "/tmp/outside.txt", nil},            // its image's file is not on this host
	}
	for h, c := range cases {
		t.Run(h, func(t *testing.T) {
			es, err := transcript.ReadEntries(filepath.Join("..", "transcript", "testdata", h+".jsonl"), h, 0)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			res, err := WriteDocument(dir, DocInput{Lang: wire.LangJa, Source: docSource(h), Entries: es, Tree: &TreeState{}})
			if err != nil {
				t.Fatal(err)
			}
			doc := readDoc(t, dir)
			if res.Bytes != int64(len(doc)) || res.Messages == 0 || !utf8.ValidString(doc) {
				t.Fatalf("result %+v for a document of %d bytes", res, len(doc))
			}
			if !strings.Contains(doc, c.output) {
				t.Errorf("tool output %q missing", c.output)
			}
			if strings.Contains(doc, c.thinking) {
				t.Errorf("thinking %q in the document", c.thinking)
			}
			if c.touched != "" && !strings.Contains(doc, "- `"+c.touched+"`\n") {
				t.Errorf("touched file %q not listed", c.touched)
			}
			if res.Images != len(c.images) {
				t.Errorf("%d images written, want %d", res.Images, len(c.images))
			}
			for _, name := range c.images {
				if _, err := os.Stat(filepath.Join(dir, ImagesDir, name)); err != nil {
					t.Errorf("image file: %v", err)
				}
				if !strings.Contains(doc, "]("+ImagesDir+"/"+name+")") {
					t.Errorf("no link to %s", name)
				}
			}
		})
	}
}

// A tool call's input and output are cut to 8 KiB: the first 6 KiB and
// the last 2 KiB, never inside a UTF-8 character.
func TestCutToolText(t *testing.T) {
	if s := strings.Repeat("x", wire.RelayToolTextMax); cutToolText(s) != s {
		t.Fatal("text of the limit cut")
	}
	for _, s := range []string{
		strings.Repeat("x", wire.RelayToolTextMax+1),
		"head" + strings.Repeat("あ", 4000) + "tail", // 3-byte runes across both cut points
		strings.Repeat("😀", 3000),                   // 4-byte runes
	} {
		got := cutToolText(s)
		if !utf8.ValidString(got) {
			t.Fatalf("cut inside a character: %q…", got[:20])
		}
		m := regexp.MustCompile(`\n\[… (\d+) bytes cut …\]\n`).FindStringSubmatchIndex(got)
		if m == nil {
			t.Fatalf("no cut marker in %d bytes", len(got))
		}
		head, tail := got[:m[0]], got[m[1]:]
		if !strings.HasPrefix(s, head) || !strings.HasSuffix(s, tail) {
			t.Fatal("head or tail not kept")
		}
		if len(head) > wire.RelayToolTextHead || len(head) < wire.RelayToolTextHead-3 {
			t.Errorf("head %d bytes", len(head))
		}
		if max := wire.RelayToolTextMax - wire.RelayToolTextHead; len(tail) > max || len(tail) < max-3 {
			t.Errorf("tail %d bytes", len(tail))
		}
		if n := got[m[2]:m[3]]; n != strconv.Itoa(len(s)-len(head)-len(tail)) {
			t.Errorf("marker says %s bytes cut, %d were", n, len(s)-len(head)-len(tail))
		}
	}
}

// Fences are longer than any run of backticks in what they hold, and a cut
// text is fenced too.
func TestDocumentFences(t *testing.T) {
	input := "{\"command\":\"printf '`````' ```\"}"
	output := strings.Repeat("o", wire.RelayToolTextMax) + "``````" // cut: the run stays in its tail
	es := []transcript.Entry{
		{Role: transcript.RoleUser, Text: "go", Time: 1},
		{Role: transcript.RoleTool, ToolName: "Bash", CallID: "c1", Input: input, Text: "printf", Time: 2},
		{Role: transcript.RoleToolOutput, CallID: "c1", Text: output, Time: 3},
	}
	dir := t.TempDir()
	if _, err := WriteDocument(dir, DocInput{Source: docSource(wire.HarnessClaude), Entries: es}); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, dir)
	for _, want := range []struct {
		inner string
		run   int
	}{{input, 5}, {strings.Repeat("o", 20), 6}} {
		i := strings.Index(doc, want.inner)
		if i < 0 {
			t.Fatalf("%q not in the document", want.inner)
		}
		before := strings.Split(doc[:i], "\n")
		fence := before[len(before)-2] // the line before the block's first
		if strings.Trim(fence, "`") != "" || len(fence) <= want.run {
			t.Errorf("fence %q before %q (a run of %d inside)", fence, want.inner, want.run)
		}
	}
}

// Images become files in order, linked relatively; one too large, or a
// path that cannot be read, is named and not carried.
func TestDocumentImages(t *testing.T) {
	src := t.TempDir()
	onDisk := filepath.Join(src, "shot.jpg")
	jpeg := []byte("\xff\xd8\xff\xe0 jpeg")
	if err := os.WriteFile(onDisk, jpeg, 0o600); err != nil {
		t.Fatal(err)
	}
	png := []byte("\x89PNG\r\n\x1a\n")
	es := []transcript.Entry{
		{Role: transcript.RoleUser, Text: "look", Images: []transcript.Image{{MediaType: "image/png", Data: png}}},
		{Role: transcript.RoleTool, ToolName: "Read", CallID: "r", Input: `{"file_path":"x"}`},
		{Role: transcript.RoleToolOutput, CallID: "r", Images: []transcript.Image{
			{Path: onDisk},
			{MediaType: "image/png", Data: bytes.Repeat([]byte{1}, maxDocImage+1)},
			{Path: filepath.Join(src, "gone.png")},
		}},
	}
	dir := t.TempDir()
	res, err := WriteDocument(dir, DocInput{Source: docSource(wire.HarnessClaude), Entries: es})
	if err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, dir)
	if res.Images != 2 || res.ImageBytes != int64(len(png)+len(jpeg)) {
		t.Fatalf("result %+v", res)
	}
	for name, want := range map[string][]byte{"0001.png": png, "0002.jpg": jpeg} {
		if b, err := os.ReadFile(filepath.Join(dir, ImagesDir, name)); err != nil || !bytes.Equal(b, want) {
			t.Errorf("%s: %q %v", name, b, err)
		}
		if !strings.Contains(doc, "](images/"+name+")") {
			t.Errorf("no link to %s", name)
		}
	}
	for _, n := range []string{"0003", "0004"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, ImagesDir, n+".*")); len(matches) > 0 {
			t.Errorf("image %s carried: %v", n, matches)
		}
	}
	if !strings.Contains(doc, "image 3") || !strings.Contains(doc, "gone.png") {
		t.Error("images not carried are not named")
	}
	if runtime.GOOS != "windows" {
		for p, want := range map[string]os.FileMode{
			filepath.Join(dir, DocumentFile): 0o600, filepath.Join(dir, ImagesDir): 0o700,
			filepath.Join(dir, ImagesDir, "0001.png"): 0o600,
		} {
			if st, err := os.Stat(p); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s: %v %v, want %v", p, st.Mode(), err, want)
			}
		}
	}
}

// The summary has the first request verbatim and the last turns; the
// unanswered section appears only with a question or approvals pending.
func TestDocumentSummaryAndNotAnswered(t *testing.T) {
	q := &wire.Ask{ID: "q1", Kind: wire.AskQuestion, Questions: []wire.QuestionItem{{Question: "Which color?", Options: []string{"Red", "Blue"}}}}
	es := []transcript.Entry{{Role: transcript.RoleUser, Text: "first request\n\n## with a heading", Time: 1}}
	for i := range 5 {
		n := strconv.Itoa(i)
		es = append(es,
			transcript.Entry{Role: transcript.RoleUser, Text: "turn " + n, Time: int64(10 + i)},
			transcript.Entry{Role: transcript.RoleTool, ToolName: "Bash", Text: "cmd " + n, Input: `{"command":"cmd"}`, Time: int64(10 + i)},
			transcript.Entry{Role: transcript.RoleAssistant, Text: "reply " + n, Time: int64(10 + i)},
		)
	}
	write := func(es []transcript.Entry, in DocInput) string {
		in.Source, in.Entries = docSource(wire.HarnessCodex), es
		dir := t.TempDir()
		if _, err := WriteDocument(dir, in); err != nil {
			t.Fatal(err)
		}
		return readDoc(t, dir)
	}

	doc := write(es, DocInput{Lang: wire.LangEn})
	// The first request in the summary and in the conversation; it is not
	// among the last 3 turns.
	if strings.Count(doc, "first request\n\n## with a heading\n") != 2 {
		t.Errorf("first request not verbatim in the summary")
	}
	// "turn 1" is in the conversation only; "turn 2"–"turn 4", their tool
	// calls and replies in the last turns too.
	for s, want := range map[string]int{"turn 1\n": 1, "cmd 1": 1, "reply 1\n": 1, "turn 2\n": 2, "cmd 3": 2, "reply 4\n": 2} {
		if got := strings.Count(doc, s); got != want {
			t.Errorf("%q %d times, want %d", s, got, want)
		}
	}
	if strings.Contains(doc, "Which color?") {
		t.Error("unanswered section without anything waiting")
	}

	waiting := append(append([]transcript.Entry{}, es...),
		transcript.Entry{Role: transcript.RoleTool, ToolName: "ask", Text: "Which color?", Ask: q, CallID: "q1"})
	doc = write(waiting, DocInput{Lang: wire.LangEn, Approvals: []wire.Approval{{ToolName: "Bash", Summary: "rm -rf build"}}})
	tail := doc[strings.LastIndex(doc, "\n## "):]
	for _, want := range []string{"Which color?", "Red", "Blue", "Bash", "rm -rf build"} {
		if !strings.Contains(tail, want) {
			t.Errorf("%q not in the unanswered section:\n%s", want, tail)
		}
	}
	answered := append(append([]transcript.Entry{}, waiting...),
		transcript.Entry{Role: transcript.RoleUser, Text: "Red", Ask: &wire.Ask{ID: "q1", Kind: wire.AskAnswer}})
	if doc := write(answered, DocInput{}); strings.Contains(doc, "Blue") {
		t.Error("an answered question is listed as waiting")
	}
}

// The memo's headings go below its section — ATX and setext ones, at most
// to level 6 — and what is in its code blocks stays as written; memo.md
// is not the document and keeps them.
func TestDocumentMemoHeadings(t *testing.T) {
	memo := strings.Join([]string{
		"# Goal",
		"Finish it.",
		"## Done",
		"Left",
		"----",
		"##### Deep",
		"#not a heading",
		"```sh",
		"# a shell comment",
		"## also code",
		"```",
		"~~~~",
		"# still code",
		"~~~~",
		"    # indented code",
	}, "\n")
	dir := t.TempDir()
	if _, err := WriteDocument(dir, DocInput{Source: docSource(wire.HarnessClaude), Memo: &MemoDoc{Text: memo}}); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, dir)
	i := strings.Index(doc, "## Hand-off memo\n")
	j := strings.Index(doc, "## Conversation")
	if i < 0 || j < i {
		t.Fatalf("no memo section:\n%s", doc)
	}
	section := doc[i:j]
	want := strings.Join([]string{
		"### Goal",
		"Finish it.",
		"#### Done",
		"#### Left",
		"###### Deep",
		"#not a heading",
		"```sh",
		"# a shell comment",
		"## also code",
		"```",
		"~~~~",
		"# still code",
		"~~~~",
		"    # indented code",
	}, "\n")
	if !strings.Contains(section, want) {
		t.Fatalf("memo section:\n%s\nwant within it:\n%s", section, want)
	}
}
