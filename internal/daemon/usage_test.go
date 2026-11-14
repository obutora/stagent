package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// usageFixture returns the lines of the usage package's Claude fixture.
func usageFixture(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "usage", "testdata", "claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.SplitAfter(strings.TrimSuffix(string(b), "\n"), "\n")
}

// claudeProjects makes a Claude projects directory holding one transcript
// with the given lines and returns the directory and the file.
func claudeProjects(t *testing.T, lines ...string) (dir, file string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "projects")
	file = filepath.Join(dir, "-home-user-proj", "4d896274-834f-4ba1-a3da-b4ec86da1a92.jsonl")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, file
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func usageTotal(r wire.UsageGetResult) wire.UsageTokens {
	var t wire.UsageTokens
	for _, d := range r.Days {
		t.Input += d.Input
		t.Output += d.Output
		t.Requests += d.Requests
	}
	return t
}

// usage.get returns what the background scan counted from the Claude
// transcripts, by the calendar days of the time zone it names.
func TestUsageGetCountsClaudeTranscripts(t *testing.T) {
	lines := usageFixture(t)
	dir, _ := claudeProjects(t, lines...)
	e := startDaemon(t, fastConfig(), Options{Roots: transcript.Roots{wire.HarnessClaude: dir}})
	c, _ := e.client()

	var r wire.UsageGetResult
	deadline := time.Now().Add(5 * time.Second)
	for {
		call(t, c, wire.MethodUsageGet, wire.UsageGetParams{TZ: "Asia/Tokyo"}, &r)
		if !r.Scanning || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r.Scanning || r.ScannedAt == 0 {
		t.Fatalf("scan did not finish: %+v", r)
	}
	if got := usageTotal(r); got.Requests != 2 || got.Output != 337+213 || got.Input != 5 {
		t.Errorf("usage = %+v, want the fixture's two requests", got)
	}
	if len(r.Days) != 1 || r.Days[0].Date != "2026-10-06" || r.Days[0].Harness != wire.HarnessClaude {
		t.Errorf("days = %+v", r.Days)
	}
	if want := []string{wire.HarnessClaude, wire.HarnessCodex, wire.HarnessOmp, wire.HarnessPi}; !slices.Equal(r.Counted, want) {
		t.Errorf("counted = %v, want %v", r.Counted, want)
	}
	err := c.Call(context.Background(), wire.MethodUsageGet, wire.UsageGetParams{TZ: "Nowhere/Else"}, &r)
	if errCode(err) != wire.ErrBadRequest {
		t.Errorf("usage.get with an unknown zone: %v, want %s", err, wire.ErrBadRequest)
	}
}

// While the first scan runs, the daemon answers everything else at once,
// and usage.get answers with what was counted so far and scanning: true.
// When the scan finishes, watchers get usage.changed.
func TestUsageScanDoesNotHoldOtherMethods(t *testing.T) {
	lines := usageFixture(t)
	dir, _ := claudeProjects(t, lines...)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	e := startDaemon(t, fastConfig(), Options{
		Roots: transcript.Roots{wire.HarnessClaude: dir},
		UsageBeforeFile: func(string) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		},
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the usage scan did not start")
	}

	c, ch := e.client()
	quick := func(method string, params, result any) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := c.Call(ctx, method, params, result); err != nil {
			t.Fatalf("%s during the usage scan: %v", method, err)
		}
	}
	quick(wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	quick(wire.MethodApprovalsList, nil, &wire.ApprovalsListResult{})
	quick(wire.MethodSessionsList, nil, nil)
	var r wire.UsageGetResult
	quick(wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	if !r.Scanning {
		t.Errorf("usage.get during the first scan: %+v, want scanning", r)
	}

	close(release)
	m := await(t, ch, "usage.changed", func(m *wire.Msg) bool { return m.Method == wire.NotifyUsageChanged })
	if !strings.Contains(string(m.Params), `"scanning":false`) {
		t.Errorf("usage.changed after the first scan: %s", m.Params)
	}
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	if r.Scanning || usageTotal(r).Requests != 2 {
		t.Errorf("usage.get after the scan: %+v", r)
	}
}

// New usage is announced with usage.changed, at most once per
// UsageChangedEvery.
func TestUsageChangedIsThrottled(t *testing.T) {
	lines := usageFixture(t)
	dir, file := claudeProjects(t, lines[:3]...)
	const every = 400 * time.Millisecond
	e := startDaemon(t, fastConfig(), Options{
		Roots:             transcript.Roots{wire.HarnessClaude: dir},
		UsageScanInterval: 20 * time.Millisecond,
		UsageChangedEvery: every,
	})
	c, ch := e.client()
	call(t, c, wire.MethodWatch, wire.WatchParams{}, &wire.WatchResult{})
	changed := func(m *wire.Msg) bool { return m.Method == wire.NotifyUsageChanged }

	// The first scan may have finished before the watch: wait for a
	// scan to have run, then grow the transcript twice in a row.
	var r wire.UsageGetResult
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
		if !r.Scanning || time.Now().After(deadline) {
			break
		}
	}
	drain := time.After(every + 100*time.Millisecond)
	for done := false; !done; {
		select {
		case <-ch:
		case <-drain:
			done = true
		}
	}

	appendFile(t, file, lines[3])
	await(t, ch, "usage.changed after an append", changed)
	at := time.Now()
	appendFile(t, file, lines[4]+lines[5])
	await(t, ch, "usage.changed after the second append", changed)
	if gap := time.Since(at); gap < every-50*time.Millisecond {
		t.Errorf("two usage.changed %v apart, want at least %v", gap, every)
	}
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	if got := usageTotal(r); got.Requests != 2 || got.Output != 337+213 {
		t.Errorf("usage after both appends: %+v", got)
	}
}
