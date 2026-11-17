package transcript

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Qwen Code's question and its answer are a question row (the
// ask_user_question call, its options) and an answer row (the answers its
// result records), linked by the call id; a question turned down closes
// unanswered.
func TestQwenAskRows(t *testing.T) {
	p, err := ReadPage(fixture(wire.HarnessQwen), wire.HarnessQwen, 0, maxLimit)
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
		question("ask_user_question", "call_mwjhbe4y17", "Which color do you prefer?",
			wire.QuestionItem{Header: "Color", Question: "Which color do you prefer?", Options: []string{"Red", "Green", "Blue"}}),
		answer("call_mwjhbe4y17", "Blue",
			wire.QuestionItem{Question: "Which color do you prefer?", Answers: []string{"Blue"}}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ask rows\n got %+v\nwant %+v", got, want)
	}
	cancelled := []byte(`{"sessionId":"s","timestamp":"2026-11-16T16:29:56.001Z","type":"tool_result","message":{"role":"user","parts":[{"functionResponse":{"id":"call_1","name":"ask_user_question","response":{"error":"[Operation Cancelled] Reason: User did not allow tool call"}}}]},"toolCallResult":{"callId":"call_1","status":"cancelled"}}`)
	if ms := parseQwen(cancelled); len(ms) != 1 || ms[0].Ask == nil || !ms[0].Ask.Declined || ms[0].Ask.ID != "call_1" {
		t.Fatalf("cancelled question: %+v", ms)
	}
}

// A Qwen Code conversation is titled by its title sidecar (the first
// prompt, or the name given with /rename), else by its first prompt, and
// read again as it grows.
func TestQwenTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "projects")
	roots := Roots{wire.HarnessQwen: root}
	chats := filepath.Join(root, "-work", "chats")
	p := filepath.Join(chats, "0f6d48da-0000-4000-8000-000000000001.jsonl")
	mtime := time.Now().Add(-time.Hour)
	writeFile(t, p, []byte(`{"uuid":"a","parentUuid":null,"sessionId":"0f6d48da-0000-4000-8000-000000000001","timestamp":"2026-11-16T16:24:52.206Z","type":"user","cwd":"/work","version":"0.27.4","message":{"role":"user","parts":[{"text":"Add a\nREADME"}]}}`+"\n"), mtime)
	x := OpenIndex(filepath.Join(t.TempDir(), "index.json"), roots)
	got, err := x.List(nil, 10)
	if err != nil || len(got) != 1 || got[0].Title != "Add a README" || got[0].Cwd != "/work" || got[0].ID != "0f6d48da-0000-4000-8000-000000000001" {
		t.Fatalf("first prompt as title: %+v %v", got, err)
	}
	writeFile(t, filepath.Join(chats, "0f6d48da-0000-4000-8000-000000000001.title.json"), []byte(`{"version":1,"displayName":"Docs","source":"manual"}`), time.Now())
	appendTo(t, p, []byte(`{"uuid":"b","parentUuid":"a","sessionId":"0f6d48da-0000-4000-8000-000000000001","timestamp":"2026-11-16T16:25:00.000Z","type":"system","subtype":"ui_telemetry","cwd":"/work","version":"0.27.4"}`+"\n"))
	os.Chtimes(p, time.Now(), time.Now())
	if got, _ = x.List(nil, 10); len(got) != 1 || got[0].Title != "Docs" {
		t.Fatalf("renamed: %+v", got)
	}
}
