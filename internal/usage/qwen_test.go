package usage

import (
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/qwen.jsonl is the conversation of the transcript reader's
// fixture: Qwen Code 0.27.4 against a fake model (nine requests of 100
// prompt and 20 output tokens), then lines written for the test, among
// them a request with cached and thought tokens.
const qwenSession = "a2730fef-0f6d-48da-9317-c9dc99d18525"

// Qwen Code: each model reply's usageMetadata is one request. The prompt
// count holds the cached tokens: they are taken out of the input and
// counted as cache reads; the thought tokens are output. The model is the
// reply's, the session and project the record's.
func TestQwenCountsUsageMetadata(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessQwen, "-home-user-proj/chats/"+qwenSession+".jsonl", fixture(t, "qwen.jsonl"))
	// A sidecar beside it is no conversation.
	tr.write(wire.HarnessQwen, "-home-user-proj/chats/"+qwenSession+".ledger.jsonl", `{"type":"assistant","uuid":"x","timestamp":"2026-11-16T16:33:00.000Z","usageMetadata":{"promptTokenCount":999}}`+"\n")
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})
	want := wire.UsageTokens{Input: 9*100 + (200 - 100), Output: 9*20 + 30 + 10, CacheRead: 100, Requests: 10}
	if got := harnessTotal(r, wire.HarnessQwen); got != want {
		t.Fatalf("qwen total = %+v, want %+v", got, want)
	}
	for _, d := range r.Days {
		if d.Harness == wire.HarnessQwen && (d.Model != "mock-model" || d.Project != "/home/user/proj") {
			t.Errorf("row = %+v", d)
		}
	}
	found := false
	for _, ss := range r.Sessions {
		found = found || ss.Harness == wire.HarnessQwen && ss.SessionID == qwenSession
	}
	if !found {
		t.Errorf("sessions = %+v", r.Sessions)
	}
}
