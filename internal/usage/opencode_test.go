package usage

import (
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// testdata/opencode.jsonl is stagent's record of an OpenCode session, the
// transcript reader's fixture: OpenCode 2.3.0 against a fake model (seven
// replies of 100 input and 20 output tokens, one cut short with none),
// then lines written for the test: a reply with reasoning, cache and a
// cost, and a last reply of 100 and 5.
const openCodeSession = "ses_e1ad4a0c9ffe59ybAJc2LhlyvP"

// OpenCode: each finished reply's tokens are one request. Its input does
// not hold the cached tokens, which count as cache reads and writes; the
// reasoning is output; its cost is the agent's. The model is the reply's,
// the session and project the record's.
func TestOpenCodeCountsReplies(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessOpenCode, openCodeSession+".jsonl", fixture(t, "opencode.jsonl"))
	// The process sidecar beside it is no session.
	tr.write(wire.HarnessOpenCode, openCodeSession+".proc", `{"type":"assistant","id":"x","time":{"completed":1794846482500},"tokens":{"input":999}}`+"\n")
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})
	want := wire.UsageTokens{Input: 7*100 + 300 + 100, Output: 7*20 + 40 + 10 + 5, CacheRead: 200, CacheWrite5m: 50, Requests: 9, AgentCost: 0.0123}
	if got := harnessTotal(r, wire.HarnessOpenCode); got != want {
		t.Fatalf("opencode total = %+v, want %+v", got, want)
	}
	for _, d := range r.Days {
		if d.Harness == wire.HarnessOpenCode && d.Project != "/home/user/proj" {
			t.Errorf("row = %+v", d)
		}
	}
	found := false
	for _, ss := range r.Sessions {
		found = found || ss.Harness == wire.HarnessOpenCode && ss.SessionID == openCodeSession
	}
	if !found {
		t.Errorf("sessions = %+v", r.Sessions)
	}
}
