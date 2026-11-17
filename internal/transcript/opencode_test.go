package transcript

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// OpenCode's question and its answer are a question row (the question
// tool's call, its options) and an answer row (the answers its result
// records), linked by the call id; a question dismissed closes unanswered.
func TestOpenCodeAskRows(t *testing.T) {
	p, err := ReadPage(fixture(wire.HarnessOpenCode), wire.HarnessOpenCode, 0, maxLimit)
	if err != nil {
		t.Fatal(err)
	}
	var got []wire.Message
	for _, m := range p.Messages {
		if m.Ask != nil {
			m.Time = 0
			got = append(got, m)
		}
	}
	want := []wire.Message{
		question("question", "call_mwjh57ylf", "Which color do you prefer?",
			wire.QuestionItem{Header: "Color", Question: "Which color do you prefer?", Options: []string{"Red", "Green", "Blue"}}),
		answer("call_mwjh57ylf", "Green",
			wire.QuestionItem{Question: "Which color do you prefer?", Answers: []string{"Green"}}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ask rows\n got %+v\nwant %+v", got, want)
	}
	dismissed := []byte(`{"id":"msg_1","time":{"created":1794846299275,"completed":1794846300804},"type":"assistant","content":[{"type":"tool","id":"call_1","name":"question","state":{"status":"error","input":{"questions":[{"question":"Which?","header":"Q","options":[{"label":"A"}],"multiple":false}]},"error":{"type":"aborted","message":"The user dismissed this question"}}}],"finish":"error"}`)
	ms := parseOpenCode(dismissed)
	if len(ms) != 2 || ms[0].Ask == nil || ms[0].Ask.Kind != wire.AskQuestion || ms[1].Ask == nil || !ms[1].Ask.Declined || ms[1].Ask.ID != "call_1" {
		t.Fatalf("dismissed question: %+v", ms)
	}
}

// exportOf is a `session export` of session ses_1 titled title in /w, with
// messages.
func exportOf(title string, messages ...string) []byte {
	return []byte(`{"info":{"id":"ses_1","projectID":"p","agent":"build","title":"` + title + `","time":{"created":1794846187395,"updated":1794846187400},"location":{"directory":"/w"}},` +
		"\n  \"messages\": [\n" + strings.Join(messages, ",\n") + "\n  ]\n}\n")
}

const (
	ocUser      = `{"id":"msg_a","time":{"created":1},"text":"run it","files":[],"agents":[],"type":"user"}`
	ocTool      = `{"id":"msg_b","time":{"created":2,"completed":3},"type":"assistant","content":[{"type":"tool","id":"call_1","name":"shell","state":{"status":"completed","input":{"command":"ls"}}}],"finish":"tool-calls"}`
	ocStreaming = `{"id":"msg_c","time":{"created":4},"type":"assistant","content":[{"type":"text","text":"All d"}]}`
	ocText      = `{"id":"msg_c","time":{"created":4,"completed":5},"type":"assistant","content":[{"type":"text","text":"All done."}],"finish":"stop"}`
	ocIdle      = `{"id":"msg_d","time":{"created":6},"type":"idle","outcome":"succeeded"}`
)

// stagent keeps an OpenCode session as a file it only appends to: the
// session record, then the messages of each export it has not written yet,
// in order. A reply still being written (no completion time) waits, with
// everything after it, for a later export; nothing already written is
// rewritten, so a reader tailing the file reads each message once.
func TestSyncOpenCodeAppendsSettledMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "ses_1.jsonl")
	read := func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	session := `{"type":"session","id":"ses_1","title":"New session","directory":"/w","created":1794846187395}` + "\n"

	if err := SyncOpenCode(path, exportOf("New session", ocUser, ocTool, ocStreaming)); err != nil {
		t.Fatal(err)
	}
	first := read()
	if want := session + ocUser + "\n" + ocTool + "\n"; first != want {
		t.Fatalf("first sync:\n%s\nwant\n%s", first, want)
	}

	// Renamed, the reply done, the turn over.
	if err := SyncOpenCode(path, exportOf("Mock title", ocUser, ocTool, ocText, ocIdle)); err != nil {
		t.Fatal(err)
	}
	second := read()
	renamed := `{"type":"session","id":"ses_1","title":"Mock title","directory":"/w","created":1794846187395}` + "\n"
	if want := first + renamed + ocText + "\n" + ocIdle + "\n"; second != want {
		t.Fatalf("second sync:\n%s\nwant\n%s", second, want)
	}

	// The same export again changes nothing.
	if err := SyncOpenCode(path, exportOf("Mock title", ocUser, ocTool, ocText, ocIdle)); err != nil {
		t.Fatal(err)
	}
	if read() != second {
		t.Fatalf("unchanged export rewrote the file:\n%s", read())
	}

	p, err := ReadPage(path, wire.HarnessOpenCode, 0, maxLimit)
	if err != nil {
		t.Fatal(err)
	}
	want := []msg{{"user", "", "run it"}, {"tool", "shell", "ls"}, {"assistant", "", "All done."}}
	if got := strip(p.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("read back %v, want %v", got, want)
	}
}

// An export that is not of the file's session, or no export at all, is
// refused and leaves the file as it was.
func TestSyncOpenCodeRefusesOtherSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ses_2.jsonl")
	for _, export := range [][]byte{exportOf("x", ocUser), []byte("Error: Session not found\n"), nil} {
		if err := SyncOpenCode(path, export); err == nil {
			t.Errorf("export %q accepted", export)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file written: %v", err)
	}
}

// The session's title and directory are read from its last session
// record; its first prompt is the fallback title.
func TestOpenCodeTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "opencode")
	path := filepath.Join(root, "ses_1.jsonl")
	if err := SyncOpenCode(path, exportOf("", ocUser)); err != nil {
		t.Fatal(err)
	}
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), Roots{wire.HarnessOpenCode: root})
	got, err := x.List(nil, 10)
	if err != nil || len(got) != 1 || got[0].Title != "run it" || got[0].Cwd != "/w" || got[0].ID != "ses_1" {
		t.Fatalf("first prompt as title: %+v %v", got, err)
	}
	if err := SyncOpenCode(path, exportOf("Fix the parser", ocUser, ocTool)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Contains(b, []byte(`"title":"Fix the parser"`)) {
		t.Fatalf("no renamed record:\n%s", b)
	}
	if got, _ = x.List(nil, 10); len(got) != 1 || got[0].Title != "Fix the parser" {
		t.Fatalf("renamed: %+v", got)
	}
}
