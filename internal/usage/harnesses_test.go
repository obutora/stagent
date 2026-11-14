package usage

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// The fixtures codex.jsonl, omp.jsonl and pi.jsonl are files written by
// codex 0.160.1, omp 18.12.6 and pi 1.3.0 (a two-request conversation
// each; codex and pi talked to a local stand-in for the model API; the
// conversation text and paths replaced). codex.jsonl has an added
// info:null token_count line (rate_limits only, as a signed-in codex writes
// it); codex-total.jsonl is codex.jsonl's token_count lines without
// last_token_usage, as older codex wrote them, with a total that goes back.

const (
	codexSession = "01a1d645-7e70-72c1-92fd-79fc3817d828"
	ompSession   = "01a1d642-32a5-724b-ab6e-873a35276054"
	piSession    = "01a1d646-d352-72da-9eb7-3ec675401d59"
	codexRel     = "sessions/2026/11/14/rollout-2026-11-14T03-57-21-" + codexSession + ".jsonl"
)

// tree is a home with the transcript directory of every counted harness and
// a scanner over them whose index lives next to them.
type tree struct {
	t     *testing.T
	index string
	roots transcript.Roots
}

func newTree(t *testing.T) *tree {
	dir := t.TempDir()
	return &tree{t: t, index: filepath.Join(dir, "usage-index.json"), roots: transcript.Roots{
		wire.HarnessClaude: filepath.Join(dir, ".claude", "projects"),
		wire.HarnessCodex:  filepath.Join(dir, ".codex"),
		wire.HarnessOmp:    filepath.Join(dir, ".omp", "agent", "sessions"),
		wire.HarnessPi:     filepath.Join(dir, ".pi", "agent", "sessions"),
	}}
}

func (tr *tree) path(h, rel string) string { return filepath.Join(tr.roots[h], rel) }

func (tr *tree) write(h, rel, content string) {
	tr.t.Helper()
	p := tr.path(h, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		tr.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) appendTo(h, rel, content string) {
	tr.t.Helper()
	f, err := os.OpenFile(tr.path(h, rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		tr.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) open() *Scanner { return Open(tr.index, tr.roots) }

// harnessTotal sums the rows of harness h in r.
func harnessTotal(r wire.UsageGetResult, h string) wire.UsageTokens {
	var t wire.UsageTokens
	for _, d := range r.Days {
		if d.Harness == h {
			addTokens(&t, d.UsageTokens)
		}
	}
	return t
}

func fixture(t *testing.T, name string) string {
	return strings.Join(fixtureLines(t, name), "")
}

// sameCost: costs are kept to the millionth of a dollar.
func sameCost(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Codex: each token_count's last_token_usage is one request. The cached
// input is part of input_tokens: it is taken out of the input and counted
// as cache reads. The info:null line (rate_limits only) is no request. The
// model is the turn_context's, the session and project the session_meta's.
func TestCodexCountsLastTokenUsage(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessCodex, codexRel, fixture(t, "codex.jsonl"))
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})

	// last_token_usage: {2000 in, 0 cached, 30 out}, {4000 in, 1500 cached, 60 out}.
	want := wire.UsageTokens{Input: 2000 + 4000 - 1500, Output: 30 + 60, CacheRead: 1500, Requests: 2}
	if got := harnessTotal(r, wire.HarnessCodex); got != want {
		t.Fatalf("codex total = %+v, want %+v", got, want)
	}
	if len(r.Days) != 1 || r.Days[0].Model != "gpt-5.5-codex" || r.Days[0].Project != "/home/user/proj" ||
		r.Days[0].Date != "2026-11-13" {
		t.Fatalf("days = %+v", r.Days)
	}
	if len(r.Sessions) != 1 || r.Sessions[0].SessionID != codexSession || r.Sessions[0].Harness != wire.HarnessCodex {
		t.Fatalf("sessions = %+v", r.Sessions)
	}
}

// Older codex wrote only total_token_usage: each request is the difference
// from the previous total. When the total goes back (compaction, a resumed
// process), it becomes the new baseline. The baseline survives a restart
// of the daemon between two scans of the same file.
func TestCodexTotalOnlyTakesDifferences(t *testing.T) {
	lines := fixtureLines(t, "codex-total.jsonl")
	tr := newTree(t)
	// session_meta, turn_context, totals 1000/200/50 and 2500/900/120.
	tr.write(wire.HarnessCodex, codexRel, strings.Join(lines[:4], ""))
	scan(t, tr.open())
	// totals 800/100/20 (went back) and 1300/400/70, read by a restarted
	// daemon.
	tr.appendTo(wire.HarnessCodex, codexRel, strings.Join(lines[4:], ""))
	s := tr.open()
	scan(t, s)

	// Differences: (1000,200,50), (1500,700,70), new baseline, (500,300,50).
	in, cached, out := int64(1000+1500+500), int64(200+700+300), int64(50+70+50)
	want := wire.UsageTokens{Input: in - cached, Output: out, CacheRead: cached, Requests: 3}
	if got := harnessTotal(report(t, s, wire.UsageGetParams{}), wire.HarnessCodex); got != want {
		t.Fatalf("codex total = %+v, want %+v", got, want)
	}
}

// omp and pi: each assistant message's usage is one request, with the
// cost the agent wrote (cost.total). Cache writes the agent marked as
// 1-hour (omp: cttl.ephemeral1h, pi: cacheWrite1h) are counted as such.
func TestOmpPiCountTokensAndCost(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessOmp, "-home-user-proj-/2026-11-13T18-53-44-997Z_"+ompSession+".jsonl", fixture(t, "omp.jsonl"))
	tr.write(wire.HarnessPi, "--home-user-proj--/2026-11-13T18-58-48-274Z_"+piSession+".jsonl", fixture(t, "pi.jsonl"))
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})

	for _, c := range []struct {
		harness, session, model string
		want                    wire.UsageTokens
	}{
		{wire.HarnessOmp, ompSession, "claude-opus-5-5", wire.UsageTokens{
			Input: 4 + 2, Output: 75 + 3, CacheRead: 0 + 12822, CacheWrite1h: 12822 + 86, Requests: 2,
			AgentCost: 0.104092 + 0.0033204}},
		{wire.HarnessPi, piSession, "claude-sonnet-4-5", wire.UsageTokens{
			Input: 4 + 5, Output: 20 + 40, CacheRead: 0 + 5000, CacheWrite1h: 1000 + 2000, Requests: 2,
			AgentCost: 0.006312 + 0.014115}},
	} {
		got := harnessTotal(r, c.harness)
		gotCost, wantCost := got.AgentCost, c.want.AgentCost
		got.AgentCost, c.want.AgentCost = 0, 0
		if got != c.want || !sameCost(gotCost, wantCost) {
			t.Errorf("%s total = %+v cost %v, want %+v cost %v", c.harness, got, gotCost, c.want, wantCost)
		}
		found := false
		for _, d := range r.Days {
			if d.Harness == c.harness {
				found = true
				if d.Model != c.model || d.Project != "/home/user/proj" {
					t.Errorf("%s row = %+v", c.harness, d)
				}
			}
		}
		if !found {
			t.Errorf("%s has no row", c.harness)
		}
		var sess *wire.UsageSession
		for i := range r.Sessions {
			if r.Sessions[i].Harness == c.harness {
				sess = &r.Sessions[i]
			}
		}
		if sess == nil || sess.SessionID != c.session || !sameCost(sess.AgentCost, wantCost) {
			t.Errorf("%s session = %+v", c.harness, sess)
		}
	}
}

// omp rewrites its title line (line 1) in place; when the new title does
// not fit the padding the whole file is rewritten and every line moves.
// The requests stay counted once.
func TestOmpTitleRewriteCountsOnce(t *testing.T) {
	lines := fixtureLines(t, "omp.jsonl")
	rel := "-home-user-proj-/2026-11-13T18-53-44-997Z_" + ompSession + ".jsonl"
	tr := newTree(t)
	tr.write(wire.HarnessOmp, rel, strings.Join(lines, ""))
	s := tr.open()
	scan(t, s)
	before := harnessTotal(report(t, s, wire.UsageGetParams{}), wire.HarnessOmp)

	title := `{"type":"title","v":1,"title":"` + strings.Repeat("A long title ", 30) + `","updatedAt":"2026-11-13T18:54:00.000Z"}` + "\n"
	f, err := os.OpenFile(tr.path(wire.HarnessOmp, rel), os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(title + strings.Join(lines[1:], "")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	future := time.Now().Add(time.Minute)
	os.Chtimes(tr.path(wire.HarnessOmp, rel), future, future)
	scan(t, s)
	if after := harnessTotal(report(t, s, wire.UsageGetParams{}), wire.HarnessOmp); after != before || after.Requests != 2 {
		t.Fatalf("after the title rewrite: %+v, before %+v", after, before)
	}
}

// The overall total is the sum of every harness.
func TestTotalIsSumOfHarnesses(t *testing.T) {
	tr := newTree(t)
	tr.write(wire.HarnessClaude, "-home-user-proj/"+sessA+".jsonl", fixture(t, "claude.jsonl"))
	tr.write(wire.HarnessCodex, codexRel, fixture(t, "codex.jsonl"))
	tr.write(wire.HarnessOmp, "p/2026-11-13T18-53-44-997Z_"+ompSession+".jsonl", fixture(t, "omp.jsonl"))
	tr.write(wire.HarnessPi, "p/2026-11-13T18-58-48-274Z_"+piSession+".jsonl", fixture(t, "pi.jsonl"))
	s := tr.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{})
	var sum wire.UsageTokens
	for _, h := range Counted() {
		h := harnessTotal(r, h)
		if h.Requests == 0 {
			t.Errorf("a harness has no requests: %+v", r.Days)
		}
		addTokens(&sum, h)
	}
	all := total(r)
	if all.Requests != 2+2+2+2 || all.Total() != sum.Total() {
		t.Fatalf("total %+v, sum of harnesses %+v", all, sum)
	}
}
