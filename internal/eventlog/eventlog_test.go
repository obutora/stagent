package eventlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func appendN(t *testing.T, l *Log, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := l.Append(wire.Event{Kind: wire.EventNotification, Data: json.RawMessage(`{"title":"x"}`)}); err != nil {
			t.Fatal(err)
		}
	}
}

func seqs(evs []wire.Event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSeqPersistsAcrossReopen(t *testing.T) {
	base := filepath.Join(t.TempDir(), "events.log")
	l, err := Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 5)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l, err = Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Seq() != 5 {
		t.Fatalf("seq after reopen = %d, want 5", l.Seq())
	}
	ev, err := l.Append(wire.Event{Kind: wire.EventSessionStarted})
	if err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 6 || ev.Time == 0 {
		t.Fatalf("appended %+v, want seq 6 with a time", ev)
	}
	got, truncated := l.Since(3)
	if !equal(seqs(got), []int64{4, 5, 6}) || truncated {
		t.Fatalf("Since(3) = %v truncated=%v", seqs(got), truncated)
	}
}

func TestSinceReportsTruncation(t *testing.T) {
	l, err := Open(filepath.Join(t.TempDir(), "events.log"), Options{MaxEvents: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	appendN(t, l, 5) // ring keeps 3, 4, 5

	cases := []struct {
		since     int64
		want      []int64
		truncated bool
	}{
		{0, nil, false},              // 0 replays nothing
		{1, []int64{3, 4, 5}, true},  // 2 was dropped
		{2, []int64{3, 4, 5}, false}, // nothing between 2 and 3 is missing
		{4, []int64{5}, false},       // partial
		{5, nil, false},              // up to date
		{9, nil, true},               // client is ahead: the log was reset
	}
	for _, c := range cases {
		got, tr := l.Since(c.since)
		if !equal(seqs(got), c.want) || tr != c.truncated {
			t.Errorf("Since(%d) = %v truncated=%v, want %v truncated=%v", c.since, seqs(got), tr, c.want, c.truncated)
		}
	}
}

func TestRotationBoundsSegmentsAndKeepsLatest(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "events.log")
	opts := Options{MaxEvents: 20, SegmentBytes: 400}
	l, err := Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 200)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, e := range entries {
		info, _ := e.Info()
		if info.Size() > opts.SegmentBytes {
			t.Errorf("%s is %d bytes, over the segment size", e.Name(), info.Size())
		}
		total++
	}
	if total < 2 {
		t.Fatalf("expected rotated segments, got %d files", total)
	}
	// Each segment holds a handful of events; only enough segments to cover
	// MaxEvents (plus the partially filled active one) may remain.
	if total > 10 {
		t.Fatalf("retention kept %d segments for 20 events", total)
	}
	if _, err := os.Stat(base + ".1"); err != nil {
		t.Fatalf("rotated segment missing: %v", err)
	}

	l, err = Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Seq() != 200 {
		t.Fatalf("seq after reopen = %d, want 200", l.Seq())
	}
	got, truncated := l.Since(180)
	if len(got) != 20 || got[0].Seq != 181 || got[19].Seq != 200 || truncated {
		t.Fatalf("Since(180) = %v truncated=%v", seqs(got), truncated)
	}
	if _, truncated := l.Since(100); !truncated {
		t.Fatal("Since(100) should be truncated after retention")
	}
}

func TestAgeRetentionDropsOldSegmentsButKeepsSeq(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "events.log")
	c := &clock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	opts := Options{MaxAge: 7 * 24 * time.Hour, Now: c.now}
	l, err := Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 3)

	c.t = c.t.Add(8 * 24 * time.Hour)
	if got, _ := l.Since(1); len(got) != 0 {
		t.Fatalf("events older than MaxAge still served: %v", seqs(got))
	}
	appendN(t, l, 1) // the old active segment rotates away and is dropped
	if _, err := os.Stat(base + ".1"); !os.IsNotExist(err) {
		t.Fatalf("old segment kept: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l, err = Open(base, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Seq() != 4 {
		t.Fatalf("seq = %d, want 4", l.Seq())
	}
	got, truncated := l.Since(2)
	if !equal(seqs(got), []int64{4}) || !truncated {
		t.Fatalf("Since(2) = %v truncated=%v", seqs(got), truncated)
	}
}

func TestTornLastLineIsIgnored(t *testing.T) {
	base := filepath.Join(t.TempDir(), "events.log")
	l, err := Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	appendN(t, l, 2)
	l.Close()
	f, err := os.OpenFile(base, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":3,"time":1,"ki`)
	f.Close()

	l, err = Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if l.Seq() != 2 {
		t.Fatalf("seq = %d, want 2", l.Seq())
	}
	appendN(t, l, 1)
	l.Close()

	l, err = Open(base, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if got, _ := l.Since(1); !equal(seqs(got), []int64{2, 3}) {
		t.Fatalf("event after a torn line lost: %v", seqs(got))
	}
}
