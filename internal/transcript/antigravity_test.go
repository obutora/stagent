package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Antigravity CLI's ask_question becomes a question row (the call, with the
// options) and an answer row (its result's "A<n>:" lines, one per
// question), linked by the step the result is written at: the reply's
// step plus one plus the call's place among its calls. A question skipped
// has no answer; all skipped close the question unanswered; one no result
// follows still waits.
func TestAntigravityAskRows(t *testing.T) {
	p, err := ReadPage(fixture(wire.HarnessAntigravity), wire.HarnessAntigravity, 0, maxLimit)
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
		question("ask_question", "11", "Tea or coffee?\nCats or dogs?",
			wire.QuestionItem{Question: "Tea or coffee?", Options: []string{"Tea", "Coffee"}},
			wire.QuestionItem{Question: "Cats or dogs?", Multi: true, Options: []string{"Cats", "Dogs"}}),
		answer("11", "Coffee\nCats, Dogs",
			wire.QuestionItem{Answers: []string{"Coffee"}}, wire.QuestionItem{Answers: []string{"Cats, Dogs"}}),
		question("ask_question", "15", "Red or blue?\nUp or down?",
			wire.QuestionItem{Question: "Red or blue?", Options: []string{"Red", "Blue"}},
			wire.QuestionItem{Question: "Up or down?", Options: []string{"Up", "Down"}}),
		answer("15", "(no answer)\nDown", wire.QuestionItem{}, wire.QuestionItem{Answers: []string{"Down"}}),
		question("ask_question", "19", "Red or blue?", wire.QuestionItem{Question: "Red or blue?", Options: []string{"Red", "Blue"}}),
		declined("19"),
		question("ask_question", "33", "A or B?", wire.QuestionItem{Question: "A or B?", Options: []string{"A", "B"}}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ask rows\n got %+v\nwant %+v", got, want)
	}
}

// The second call of a reply is answered two steps after it; a typed
// answer of several lines stays one answer.
func TestAntigravityAskSecondCall(t *testing.T) {
	reply := []byte(`{"step_index": 7, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-22T01:21:12Z", "tool_calls": [{"name": "read_file", "args": {"Path": "/p/a.txt"}}, {"name": "ask_question", "args": {"questions": [{"question": "Why?", "options": ["Because"]}]}}]}`)
	ms := parseAntigravity(reply)
	if len(ms) != 2 || ms[0].ToolName != "read_file" || ms[0].Text != "/p/a.txt" || ms[1].Ask == nil || ms[1].Ask.ID != "9" {
		t.Fatalf("reply rows %+v", ms)
	}
	result := []byte(`{"step_index": 9, "source": "MODEL", "type": "GENERIC", "status": "DONE", "created_at": "2026-11-22T01:21:18Z", "content": "Created At: 2026-11-22T10:21:18+09:00\nCompleted At: 2026-11-22T10:21:38+09:00\nA1: first line\nsecond line"}`)
	ms = parseAntigravity(result)
	if len(ms) != 1 || ms[0].Ask == nil || ms[0].Ask.ID != "9" || ms[0].Text != "first line\nsecond line" || ms[0].Time != parseTime("2026-11-22T01:21:18Z") {
		t.Fatalf("answer rows %+v", ms)
	}
}

// A conversation's directory comes from history.jsonl: the line of its
// first prompt (recorded without the conversation's id) once it is there,
// then a line with its id. A conversation history.jsonl does not tell is
// not listed.
func TestAntigravityCwdFromHistory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "antigravity-cli")
	roots := Roots{wire.HarnessAntigravity: root}
	id := "0b6f3c1e-0000-4000-8000-000000000001"
	p := filepath.Join(root, "brain", id, ".system_generated", "logs", "transcript_full.jsonl")
	mtime := time.Now().Add(-time.Hour)
	writeFile(t, p, []byte(`{"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": "2026-11-22T01:09:01Z", "content": "<USER_REQUEST>\nAdd a\nREADME\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nx\n</ADDITIONAL_METADATA>"}`+"\n"), mtime)
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), roots)
	if got, err := x.List(nil, 10); err != nil || len(got) != 0 {
		t.Fatalf("listed without a cwd: %+v %v", got, err)
	}
	if c, ok := x.Conversation(wire.HarnessAntigravity, p); !ok || c.ID != id || c.Cwd != "" || c.Title != "Add a README" {
		t.Fatalf("conversation without a cwd: %+v %v", c, ok)
	}

	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, []byte(`{"display":"Add a\nREADME","timestamp":1795309741716,"workspace":"/work"}`+"\n"), time.Now())
	appendTo(t, p, []byte(`{"step_index": 1, "source": "MODEL", "type": "PLANNER_RESPONSE", "status": "DONE", "created_at": "2026-11-22T01:09:02Z", "content": "Done."}`+"\n"))
	os.Chtimes(p, time.Now(), time.Now())
	got, err := x.List(nil, 10)
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Cwd != "/work" || got[0].Title != "Add a README" {
		t.Fatalf("first prompt's line: %+v %v", got, err)
	}
	if cwd := AntigravityCwd(p); cwd != "/work" {
		t.Fatalf("AntigravityCwd = %q", cwd)
	}

	// Another conversation, whose prompt's line carries its id.
	id2 := "0b6f3c1e-0000-4000-8000-000000000002"
	p2 := filepath.Join(root, "brain", id2, ".system_generated", "logs", "transcript_full.jsonl")
	writeFile(t, p2, []byte(`{"step_index": 0, "source": "USER_EXPLICIT", "type": "USER_INPUT", "status": "DONE", "created_at": "2026-11-22T03:00:00Z", "content": "<USER_REQUEST>\nhi\n</USER_REQUEST>"}`+"\n"), time.Now())
	appendTo(t, history, []byte(`{"display":"/resume","timestamp":1795316400000,"workspace":"/elsewhere","type":"slash_command"}`+"\n"+
		`{"display":"hi again","timestamp":1795316460000,"workspace":"/work2","conversationId":"`+id2+`"}`+"\n"))
	later := time.Now().Add(time.Second)
	os.Chtimes(history, later, later)
	if cwd := AntigravityCwd(p2); cwd != "/work2" {
		t.Fatalf("AntigravityCwd by id = %q", cwd)
	}
}
