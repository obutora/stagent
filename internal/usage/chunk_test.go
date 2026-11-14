package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// claudeRequests is a Claude transcript of n requests, each one line.
func claudeRequests(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, `{"message":{"model":"claude-opus-5-5","id":"msg_%d","type":"message","role":"assistant","usage":{"input_tokens":3,"cache_read_input_tokens":100,"output_tokens":10}},"requestId":"req_%d","type":"assistant","uuid":"u%d","timestamp":"2026-10-06T01:00:00.000Z","sessionId":"%s","cwd":"/home/user/proj"}`+"\n", i, i, i, sessA)
	}
	return b.String()
}

// A scan stopped in the middle of a long file keeps what it read: the
// counts of the lines before the stop are there, and the next scan goes
// on from where it stopped, counting each request once.
func TestScanStoppedKeepsProgress(t *testing.T) {
	const n = 3*4096 + 10
	c := newClaudeTree(t)
	c.write("-home-user-proj/"+sessA+".jsonl", claudeRequests(n), time.Now().Add(-time.Minute))
	s := c.open()
	stop := make(chan struct{})
	s.BeforeFile = func(string) { close(stop) }
	if _, err := s.Scan(stop); err != nil {
		t.Fatal(err)
	}
	got := total(report(t, s, wire.UsageGetParams{})).Requests
	if got == 0 || got >= n {
		t.Fatalf("after a stopped scan: %d requests, want some of %d", got, n)
	}

	s = c.open() // the progress was saved too
	if got := total(report(t, s, wire.UsageGetParams{})).Requests; got == 0 {
		t.Fatal("the stopped scan's progress was not saved")
	}
	scan(t, s)
	if got := total(report(t, s, wire.UsageGetParams{})).Requests; got != n {
		t.Fatalf("after the next scan: %d requests, want %d", got, n)
	}
}

// BenchmarkScanLongFile reads one long transcript from the start (a new
// scanner each time): the requests are merged every few thousand lines,
// so the memory a scan holds does not grow with the file.
func BenchmarkScanLongFile(b *testing.B) {
	root := filepath.Join(b.TempDir(), "projects")
	path := filepath.Join(root, "-home-user-proj", sessA+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(claudeRequests(100_000)), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Open("", transcript.Roots{wire.HarnessClaude: root}).Scan(nil); err != nil {
			b.Fatal(err)
		}
	}
}
