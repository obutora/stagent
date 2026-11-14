package usage

import (
	"reflect"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

// claude-long.jsonl is claude.jsonl's last reply (prompt 47,685 tokens)
// followed by requests whose prompt (input + cache reads + cache writes) is
// exactly 100,000, 150,000 and 250,000 tokens; the last one streamed in two
// lines. codex-long.jsonl is codex.jsonl with a second request of 300,000
// input tokens (290,000 of them cached).

// The requests whose prompt was over a threshold of
// wire.UsageLongContextThresholds are also counted apart in the day's row,
// one entry per threshold (a request over 200K is in the 100K entry too);
// a prompt of exactly the threshold is not over it. The split survives a
// restart of the daemon (it is kept in the index).
func TestLongContextCountedApart(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessClaude, "-home-user-proj/"+sessA+".jsonl", fixture(t, "claude-long.jsonl"))
	tr.write(wire.HarnessCodex, codexRel, fixture(t, "codex-long.jsonl"))
	scan(t, tr.open())
	s := tr.open()
	r := report(t, s, wire.UsageGetParams{})

	over150 := wire.UsageTokens{Input: 10, Output: 300, CacheRead: 147990, CacheWrite5m: 2000, Requests: 1}
	over250 := wire.UsageTokens{Input: 10, Output: 400, CacheRead: 244990, CacheWrite1h: 5000, Requests: 1}
	var both wire.UsageTokens
	addTokens(&both, over150)
	addTokens(&both, over250)
	codex300 := wire.UsageTokens{Input: 10000, Output: 60, CacheRead: 290000, Requests: 1}
	want := map[string][]wire.UsageLongContext{
		wire.HarnessClaude: {{Over: 100000, UsageTokens: both}, {Over: 200000, UsageTokens: over250}},
		wire.HarnessCodex: {{Over: 100000, UsageTokens: codex300}, {Over: 200000, UsageTokens: codex300},
			{Over: 272000, UsageTokens: codex300}},
	}
	if len(r.Days) != 2 {
		t.Fatalf("days = %+v", r.Days)
	}
	for _, d := range r.Days {
		if !reflect.DeepEqual(d.LongContext, want[d.Harness]) {
			t.Errorf("%s long_context = %+v, want %+v", d.Harness, d.LongContext, want[d.Harness])
		}
	}
	// The day's own counts still hold every request.
	if got := harnessTotal(r, wire.HarnessClaude); got.Requests != 4 || got.Output != 213+50+300+400 {
		t.Errorf("claude day = %+v", got)
	}
	if got := harnessTotal(r, wire.HarnessCodex); got.Requests != 2 || got.Input != 2000+10000 {
		t.Errorf("codex day = %+v", got)
	}
}
