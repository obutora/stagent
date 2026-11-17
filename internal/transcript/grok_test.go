package transcript

import (
	"bufio"
	"os"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// Grok Build's ask_user_question becomes a question row (its tool_call,
// with the options) and an answer row (the call's completed update, with
// the options picked), linked by the call's id.
func TestGrokQuestionRows(t *testing.T) {
	f, err := os.Open(fixture(wire.HarnessGrok))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	asked := map[string]bool{}
	var rows []wire.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		for _, m := range parseLine(wire.HarnessGrok, sc.Bytes(), func(id string) bool { return asked[id] }) {
			if m.Ask != nil {
				if m.Ask.Kind == wire.AskQuestion {
					asked[m.Ask.ID] = true
				}
				rows = append(rows, m)
			}
		}
	}
	if len(rows) != 2 {
		t.Fatalf("question rows %+v", rows)
	}
	q, a := rows[0], rows[1]
	if q.Ask.ID != "call_mwjh8t8aj" || len(q.Ask.Questions) != 1 || q.Ask.Questions[0].Question != "Which color do you prefer?" ||
		len(q.Ask.Questions[0].Options) != 3 || q.Ask.Questions[0].Options[1] != "Green" {
		t.Errorf("question %+v", q.Ask)
	}
	if a.Ask.ID != q.Ask.ID || a.Ask.Kind != wire.AskAnswer || a.Role != RoleUser || a.Text != "Green" {
		t.Errorf("answer %+v %+v", a, a.Ask)
	}
}

// A question dismissed (its call failed) is closed without an answer; a
// failed call of another tool is no row.
func TestGrokQuestionDismissed(t *testing.T) {
	failed := []byte(`{"timestamp":1794846483,"method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"call_q","status":"failed","content":[{"type":"content","content":{"type":"text","text":"User cancelled"}}]},"_meta":{"agentTimestampMs":1794846483926}}}`)
	got := parseLine(wire.HarnessGrok, failed, func(id string) bool { return id == "call_q" })
	if len(got) != 1 || got[0].Ask == nil || !got[0].Ask.Declined || got[0].Time != 1794846483926 {
		t.Fatalf("dismissed question: %+v", got)
	}
	if got := parseLine(wire.HarnessGrok, failed, func(string) bool { return false }); len(got) != 0 {
		t.Fatalf("failed tool call: %+v", got)
	}
}
