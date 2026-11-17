package usage

import (
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/grok.jsonl is the session of the transcript reader's fixture:
// Grok Build 1.0.77 against a fake model, thirteen turns of one to three
// model calls of 100 input and 20 output tokens each (turn_completed),
// then lines written for the test.
const grokSession = "01a1e529-d6eb-7ca1-a972-905c1d336da5"

// Grok Build: each turn's usage is one request (Grok Build writes no
// usage per model call), of the model that spent it; the session and its directory come from the file's path (the
// directory percent-encoded).
func TestGrokCountsTurnUsage(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessGrok, "%2Fhome%2Fuser%2Fmy%20proj/"+grokSession+"/updates.jsonl", fixture(t, "grok.jsonl"))
	// The session's other files are no conversation.
	tr.write(wire.HarnessGrok, "%2Fhome%2Fuser%2Fmy%20proj/"+grokSession+"/events.jsonl", `{"type":"turn_completed","prompt_id":"x","usage":{"inputTokens":999}}`+"\n")
	// A session of a directory too long to name: its .cwd file.
	tr.write(wire.HarnessGrok, "bbbb-42f84f551da93751/.cwd", "/home/user/long\n")
	tr.write(wire.HarnessGrok, "bbbb-42f84f551da93751/01a1e600-0000-7000-8000-000000000000/updates.jsonl",
		`{"timestamp":1794846700,"method":"_x.ai/session/update","params":{"sessionId":"01a1e600-0000-7000-8000-000000000000","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":300,"outputTokens":40,"totalTokens":340,"cachedReadTokens":200,"cacheCreationTokens":0,"reasoningTokens":10,"modelUsage":{"grok-4":{"inputTokens":300,"outputTokens":40,"totalTokens":340,"cachedReadTokens":200,"reasoningTokens":10}}}},"_meta":{"agentTimestampMs":1794846700000}}}`+"\n")
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})
	want := wire.UsageTokens{Input: 22*100 + 100, Output: 22*20 + 50, CacheRead: 200, Requests: 14}
	if got := harnessTotal(r, wire.HarnessGrok); got != want {
		t.Fatalf("grok total = %+v, want %+v", got, want)
	}
	projects := map[string]string{}
	for _, d := range r.Days {
		if d.Harness == wire.HarnessGrok {
			projects[d.Model] = d.Project
		}
	}
	if projects["mock-model"] != "/home/user/my proj" || projects["grok-4"] != "/home/user/long" {
		t.Errorf("projects by model %v", projects)
	}
	found := false
	for _, ss := range r.Sessions {
		found = found || ss.Harness == wire.HarnessGrok && ss.SessionID == grokSession
	}
	if !found {
		t.Errorf("sessions = %+v", r.Sessions)
	}
}
