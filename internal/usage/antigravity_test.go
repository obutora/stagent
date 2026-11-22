package usage

import (
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/antigravity.jsonl is the transcript reader's fixture (records
// of Antigravity CLI 1.5.14's shapes: fifteen replies, the first of 8644
// input and 90 output tokens with nothing cached, the second of 2401 input,
// 6587 cache read and 58 output, the others of 2000, 7000 and 50), then
// lines written for the test: a tool result naming the usage keys and a
// reply that spent nothing.
const agyConversation = "7c2e0a51-3f0e-4d39-9c43-2f1f0e0b6a11"

// agyRel is where the fixture's transcript lies below the Antigravity CLI
// home.
const agyRel = "brain/" + agyConversation + "/.system_generated/logs/transcript_full.jsonl"

// Antigravity CLI: each reply's usage is one request, its input without
// the cache reads counted beside it, of the reply's model. The session is
// the conversation of the path, the project the directory history.jsonl
// records for its first prompt. The truncated transcript.jsonl and the
// chunks beside the transcript are not counted again.
func TestAntigravityCountsReplyUsage(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessAntigravity, agyRel, fixture(t, "antigravity.jsonl"))
	tr.write(wire.HarnessAntigravity, "brain/"+agyConversation+"/.system_generated/logs/transcript.jsonl", fixture(t, "antigravity.jsonl"))
	tr.write(wire.HarnessAntigravity, "brain/"+agyConversation+"/.system_generated/logs/chunks/transcript_full_0.jsonl", fixture(t, "antigravity.jsonl"))
	tr.write(wire.HarnessAntigravity, "history.jsonl",
		`{"display":"Run `+"`curl -sI https://example.com`"+` and tell me the status line.","timestamp":1795309741716,"workspace":"/home/user/proj"}`+"\n")
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})
	want := wire.UsageTokens{Input: 8644 + 2401 + 13*2000, Output: 90 + 58 + 13*50, CacheRead: 6587 + 13*7000, Requests: 15}
	if got := harnessTotal(r, wire.HarnessAntigravity); got != want {
		t.Fatalf("antigravity total = %+v, want %+v", got, want)
	}
	for _, d := range r.Days {
		if d.Harness == wire.HarnessAntigravity && (d.Model != "gemini-4-neon-high" || d.Project != "/home/user/proj") {
			t.Errorf("row = %+v", d)
		}
	}
	found := false
	for _, ss := range r.Sessions {
		found = found || ss.Harness == wire.HarnessAntigravity && ss.SessionID == agyConversation
	}
	if !found {
		t.Errorf("sessions = %+v", r.Sessions)
	}
}
