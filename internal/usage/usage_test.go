package usage

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

const (
	sessA = "4d896274-834f-4ba1-a3da-b4ec86da1a92"
	sessB = "9c1e3a57-2b8d-4f60-a1d4-0e6f7b2c8d93"
)

// fixtureLines returns the lines of testdata/<name> (real transcript
// records, trimmed), each with its newline.
func fixtureLines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// claudeTree is a Claude projects directory with a scanner over it whose
// index lives next to it.
type claudeTree struct {
	t     *testing.T
	root  string
	index string
}

func newClaudeTree(t *testing.T) *claudeTree {
	dir := t.TempDir()
	return &claudeTree{t: t, root: filepath.Join(dir, "projects"), index: filepath.Join(dir, "usage-index.json")}
}

func (c *claudeTree) path(rel string) string { return filepath.Join(c.root, rel) }

// write creates (or replaces) rel with the given content and mtime.
func (c *claudeTree) write(rel, content string, mtime time.Time) {
	c.t.Helper()
	p := c.path(rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		c.t.Fatal(err)
	}
}

func (c *claudeTree) appendTo(rel, content string) {
	c.t.Helper()
	f, err := os.OpenFile(c.path(rel), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		c.t.Fatal(err)
	}
}

// open opens the scanner as a (re)started daemon does.
func (c *claudeTree) open() *Scanner {
	return Open(c.index, transcript.Roots{wire.HarnessClaude: c.root})
}

func scan(t *testing.T, s *Scanner) Stats {
	t.Helper()
	st, err := s.Scan(nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func report(t *testing.T, s *Scanner, p wire.UsageGetParams) wire.UsageGetResult {
	t.Helper()
	r, err := s.Report(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// total sums every row of r.
func total(r wire.UsageGetResult) wire.UsageTokens {
	var t wire.UsageTokens
	for _, d := range r.Days {
		addTokens(&t, d.UsageTokens)
	}
	return t
}

// Every harness of the registry has an entry in the readers table. Claude,
// codex, omp and pi are counted; Copilot CLI writes no input tokens per
// request, so it stays "not counted".
func TestReadersCoverRegistry(t *testing.T) {
	for _, id := range harness.Missing(readers) {
		t.Errorf("harness %q has no entry in readers", id)
	}
	want := []string{wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp, wire.HarnessPi}
	if got := Counted(); !reflect.DeepEqual(got, want) {
		t.Errorf("Counted() = %v, want %v", got, want)
	}
}

// A streamed response is written as several lines with the same
// message.id:requestId and a growing output: it counts once, with the
// largest values. The <synthetic> record of an aborted request is not a
// request. A resumed conversation copies earlier lines into its new file;
// they stay counted once, for the file they were first seen in.
func TestClaudeCountsEachRequestOnce(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	base := time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
	c.write("-home-user-proj/"+sessA+".jsonl", strings.Join(lines, ""), base)
	// The resumed session: the two streamed lines of the first request
	// copied over, then one new request.
	resumed := strings.ReplaceAll(lines[2]+lines[3], sessA, sessB) +
		strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(lines[5], sessA, sessB),
			"msg_011Cfk9qjkK6h5vgujFj9mA3", "msg_011CfkNEW"), "req_011Cfk9qjQEGrzhJUjzC2Wur", "req_011CfkNEW")
	c.write("-home-user-proj/"+sessB+".jsonl", resumed, base.Add(time.Hour))

	s := c.open()
	scan(t, s)
	r := report(t, s, wire.UsageGetParams{TZ: "UTC"})

	want := []wire.UsageDay{{
		Date: "2026-10-06", Harness: wire.HarnessClaude, Model: "claude-opus-5-5", Project: "/home/user/proj",
		UsageTokens: wire.UsageTokens{
			Input:        2 + 3 + 3,
			Output:       337 + 213 + 213,
			CacheRead:    27238 + 47306 + 47306,
			CacheWrite5m: 376 + 376,
			CacheWrite1h: 20068,
			Requests:     3,
		},
	}}
	if !reflect.DeepEqual(r.Days, want) {
		t.Errorf("days = %+v\nwant %+v", r.Days, want)
	}
	if r.Scanning {
		t.Error("scanning after a finished scan")
	}
	if len(r.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want 2", r.Sessions)
	}
	first := r.Sessions[0]
	if first.SessionID != sessA || first.Total != 2+337+27238+20068+3+213+47306+376 || first.Requests != 2 {
		t.Errorf("top session = %+v, want %s with both of its requests", first, sessA)
	}
	if first.Project != "/home/user/proj" || !reflect.DeepEqual(first.Models, []string{"claude-opus-5-5"}) ||
		first.First != time.Date(2026, 10, 6, 1, 56, 51, 394e6, time.UTC).UnixMilli() ||
		first.Last != time.Date(2026, 10, 6, 1, 56, 57, 72e6, time.UTC).UnixMilli() {
		t.Errorf("top session = %+v", first)
	}
	if second := r.Sessions[1]; second.SessionID != sessB || second.Requests != 1 {
		t.Errorf("second session = %+v, want %s with its one new request", second, sessB)
	}
}

// The unterminated last line of a file being written is not counted until
// its newline arrives; then it counts once (the streamed request grows
// from 10 to 337 output tokens, not 10 + 337).
func TestClaudeCountsUnterminatedLineOnceClosed(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	rel := "-home-user-proj/" + sessA + ".jsonl"
	c.write(rel, lines[0]+lines[1]+lines[2]+strings.TrimSuffix(lines[3], "\n"), time.Now())

	s := c.open()
	scan(t, s)
	if got := total(report(t, s, wire.UsageGetParams{})); got.Output != 10 || got.Requests != 1 {
		t.Fatalf("before the newline: %+v, want the first streamed line only", got)
	}
	c.appendTo(rel, "\n")
	scan(t, s)
	if got := total(report(t, s, wire.UsageGetParams{})); got.Output != 337 || got.Requests != 1 || got.Input != 2 {
		t.Fatalf("after the newline: %+v, want one request of 337 output tokens", got)
	}
}

// Days are calendar days of the time zone usage.get names: a request at
// 15:30 UTC is the next day in Asia/Tokyo and the same day in UTC. From
// and To pick days in that zone.
func TestDaysFollowTimeZone(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	c.write("-home-user-proj/"+sessA+".jsonl",
		strings.ReplaceAll(lines[5], "2026-10-06T01:56:57.072Z", "2026-10-05T15:30:00.000Z"), time.Now())
	s := c.open()
	scan(t, s)

	for _, tc := range []struct {
		p    wire.UsageGetParams
		want []string
	}{
		{wire.UsageGetParams{TZ: "UTC"}, []string{"2026-10-05"}},
		{wire.UsageGetParams{TZ: "Asia/Tokyo"}, []string{"2026-10-06"}},
		{wire.UsageGetParams{TZ: "Asia/Tokyo", From: "2026-10-06", To: "2026-10-06"}, []string{"2026-10-06"}},
		{wire.UsageGetParams{TZ: "Asia/Tokyo", To: "2026-10-05"}, nil},
		{wire.UsageGetParams{TZ: "UTC", From: "2026-10-06"}, nil},
	} {
		r := report(t, s, tc.p)
		var got []string
		for _, d := range r.Days {
			got = append(got, d.Date)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: days %v, want %v", tc.p, got, tc.want)
		}
		if len(tc.want) == 0 && len(r.Sessions) != 0 {
			t.Errorf("%+v: sessions %+v outside the days", tc.p, r.Sessions)
		}
	}
	if _, err := s.Report(wire.UsageGetParams{TZ: "Mars/Olympus"}); err == nil {
		t.Error("an unknown time zone was accepted")
	}
	if _, err := s.Report(wire.UsageGetParams{From: "10/05"}); err == nil {
		t.Error("a malformed day was accepted")
	}
}

// The index keeps what was counted across restarts: an unchanged file is
// not read again, an appended one only from where the last scan stopped,
// and one that shrank or was replaced is read again from the start.
func TestIndexReadsOnlyNewBytes(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	rel := "-home-user-proj/" + sessA + ".jsonl"
	head := strings.Join(lines[:4], "")
	c.write(rel, head, time.Now().Add(-time.Minute))

	s := c.open()
	if st := scan(t, s); st.Read != int64(len(head)) {
		t.Fatalf("first scan read %d bytes, want %d", st.Read, len(head))
	}
	want := total(report(t, s, wire.UsageGetParams{}))

	// Restart: nothing changed, nothing is read, the counts are kept.
	s = c.open()
	if r := report(t, s, wire.UsageGetParams{}); !r.Scanning || total(r) != want {
		t.Fatalf("after a restart before the scan: scanning %v, %+v; want scanning with %+v", r.Scanning, total(r), want)
	}
	if st := scan(t, s); st.Read != 0 {
		t.Fatalf("rescan of an unchanged tree read %d bytes", st.Read)
	}
	if got := total(report(t, s, wire.UsageGetParams{})); got != want {
		t.Fatalf("after a restart: %+v, want %+v", got, want)
	}

	// Appended: only the new bytes are read.
	c.appendTo(rel, lines[4]+lines[5])
	s = c.open()
	if st := scan(t, s); st.Read != int64(len(lines[4]+lines[5])) {
		t.Fatalf("scan after an append read %d bytes, want %d", st.Read, len(lines[4]+lines[5]))
	}
	if got := total(report(t, s, wire.UsageGetParams{})); got.Requests != 2 || got.Output != 337+213 {
		t.Fatalf("after the append: %+v", got)
	}

	// Shrunk (rewritten in place): read again, its old counts replaced.
	small := lines[5]
	if err := os.WriteFile(c.path(rel), []byte(small), 0o600); err != nil {
		t.Fatal(err)
	}
	if st := scan(t, s); st.Read != int64(len(small)) {
		t.Fatalf("scan after a shrink read %d bytes, want %d", st.Read, len(small))
	}
	if got := total(report(t, s, wire.UsageGetParams{})); got.Requests != 1 || got.Output != 213 {
		t.Fatalf("after the shrink: %+v, want only the remaining request", got)
	}

	// Replaced by another file (a new inode) that is longer: read again
	// from the start, not as an append.
	replacement := strings.Join(lines[:4], "")
	tmp := c.path(rel) + ".new"
	if err := os.WriteFile(tmp, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, c.path(rel)); err != nil {
		t.Fatal(err)
	}
	if !inodesSupported {
		t.Skip("no inode numbers on this OS")
	}
	if st := scan(t, s); st.Read != int64(len(replacement)) {
		t.Fatalf("scan after a replace read %d bytes, want %d", st.Read, len(replacement))
	}
	if got := total(report(t, s, wire.UsageGetParams{})); got.Requests != 1 || got.Output != 337 {
		t.Fatalf("after the replace: %+v", got)
	}
}

// Sub-agent transcripts (<project>/<session>/subagents/*.jsonl) spend
// tokens too and are counted.
func TestClaudeCountsSubagents(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	c.write("-home-user-proj/"+sessA+"/subagents/agent-a1b2.jsonl", lines[5], time.Now())
	s := c.open()
	scan(t, s)
	if got := total(report(t, s, wire.UsageGetParams{})); got.Requests != 1 || got.Output != 213 {
		t.Fatalf("sub-agent usage: %+v", got)
	}
}

// A corrupt or older index is dropped and everything is read again.
func TestIndexVersionMismatchRereads(t *testing.T) {
	lines := fixtureLines(t, "claude.jsonl")
	c := newClaudeTree(t)
	content := strings.Join(lines, "")
	c.write("-home-user-proj/"+sessA+".jsonl", content, time.Now())
	scan(t, c.open())

	b, err := os.ReadFile(c.index)
	if err != nil {
		t.Fatal(err)
	}
	old := bytes.Replace(b, []byte(`"version":`+strconv.Itoa(indexVersion)), []byte(`"version":0`), 1)
	if bytes.Equal(old, b) {
		t.Fatal("index has no version")
	}
	if err := os.WriteFile(c.index, old, 0o600); err != nil {
		t.Fatal(err)
	}
	s := c.open()
	if st := scan(t, s); st.Read != int64(len(content)) {
		t.Fatalf("scan with an older index read %d bytes, want %d", st.Read, len(content))
	}
	if got := total(report(t, s, wire.UsageGetParams{})); got.Requests != 2 {
		t.Fatalf("after re-reading: %+v", got)
	}
}

// ScannedAt never waits for the scanner's lock, which a scan holds while
// it merges a file: the daemon reads it under its own lock.
func TestScannedAtDoesNotWaitForScan(t *testing.T) {
	s := Open("", transcript.Roots{})
	s.Scan(nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	got := make(chan bool, 1)
	go func() {
		done, _ := s.ScannedAt()
		got <- done
	}()
	select {
	case done := <-got:
		if !done {
			t.Fatal("ScannedAt says no scan finished")
		}
	case <-time.After(time.Second):
		t.Fatal("ScannedAt waited for the scanner's lock")
	}
}
