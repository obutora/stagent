package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/usage"
	"github.com/obutora/stagent/internal/wire"
)

// fakeOmp answers `omp usage` with the fixtures, counting runs.
type fakeOmp struct {
	mu      sync.Mutex
	live    int
	history int
	fail    bool
}

func (f *fakeOmp) run(_ context.Context, bin string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	history := strings.Contains(strings.Join(args, " "), "--history")
	if history {
		f.history++
	} else {
		f.live++
	}
	if f.fail && !history {
		return nil, errors.New("exit status 1")
	}
	name := "omp-usage.json"
	if history {
		name = "omp-usage-history.json"
	}
	return os.ReadFile(filepath.Join("..", "usage", "testdata", name))
}

func (f *fakeOmp) counts() (live, history int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.live, f.history
}

func (f *fakeOmp) setFail(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = v
}

func startOmp(t *testing.T) (*apiEnv, *fakeOmp) {
	t.Helper()
	f := &fakeOmp{}
	clk := &clock{t: time.UnixMilli(1794630500000)}
	lg := &logs{}
	e := startDaemon(t, fastConfig(), Options{
		Now:            clk.now,
		Logf:           lg.logf,
		PrivateAPITick: 5 * time.Millisecond,
		OmpBin:         func() string { return "/fake/omp" },
		Omp:            usage.OmpUsage{Run: f.run},
	})
	return &apiEnv{testEnv: e, clk: clk, logs: lg, claudeDir: filepath.Join(e.layout.Home, ".claude")}, f
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// omp's snapshots are read at once without the app; the Claude account
// gets the week and the Fable week (no five_hour: Anthropic sent none).
func TestOmpHistoryRecordedUnderClaudeAccount(t *testing.T) {
	e, f := startOmp(t)
	c, _ := e.client()
	waitFor(t, "omp windows", func() bool { return len(windows(t, c, wire.HarnessClaude, e.claudeDir)) == 2 })
	ws := windows(t, c, wire.HarnessClaude, e.claudeDir)
	if w := ws[wire.UsageWindowSevenDay]; pct(w) != 67 || w.Source != wire.UsageSourceOmpUsage || w.MeasuredAt != 1794630416288 {
		t.Fatalf("seven_day = %+v", w)
	}
	if w := ws["seven_day_fable"]; pct(w) != 28 {
		t.Fatalf("seven_day_fable = %+v", w)
	}
	if _, ok := ws[wire.UsageWindowFiveHour]; ok {
		t.Fatal("five_hour shown though omp sent none")
	}
	if live, _ := f.counts(); live != 0 {
		t.Fatalf("live runs without the app: %d", live)
	}
	// Read again only after OmpHistoryEvery.
	time.Sleep(50 * time.Millisecond)
	_, h := f.counts()
	e.advance(defaultOmpHistoryEvery)
	waitFor(t, "second history read", func() bool { _, n := f.counts(); return n == h+1 })
}

// The live command runs like the private API: in front once per 15
// minutes, on opening the usage screen after 5; a failure becomes the
// windows' error, the values kept; the next success clears it.
func TestOmpLiveGatedAndErrors(t *testing.T) {
	e, f := startOmp(t)
	c := e.app(false)
	time.Sleep(50 * time.Millisecond)
	if live, _ := f.counts(); live != 0 {
		t.Fatalf("live runs in the background: %d", live)
	}
	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	waitFor(t, "live run", func() bool { l, _ := f.counts(); return l == 1 })
	waitFor(t, "live value", func() bool {
		return pct(windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowSevenDay]) == 67
	})
	e.advance(15*time.Minute - time.Second)
	time.Sleep(50 * time.Millisecond)
	if l, _ := f.counts(); l != 1 {
		t.Fatalf("live runs within 15 minutes: %d", l)
	}

	f.setFail(true)
	e.advance(time.Second)
	waitFor(t, "error", func() bool {
		w := windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowSevenDay]
		return w.Error != nil && w.Error.Code == wire.UsageErrCommand && pct(w) == 67
	})

	f.setFail(false)
	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: false}, nil)
	e.advance(10 * time.Minute)
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{RefreshLimits: true}, &wire.UsageGetResult{})
	waitFor(t, "error cleared", func() bool {
		return windows(t, c, wire.HarnessClaude, e.claudeDir)[wire.UsageWindowSevenDay].Error == nil
	})
}

// Without omp nothing runs and nothing is recorded.
func TestOmpNotInstalled(t *testing.T) {
	f := &fakeOmp{}
	e := startDaemon(t, fastConfig(), Options{
		PrivateAPITick: 5 * time.Millisecond,
		OmpBin:         func() string { return "" },
		Omp:            usage.OmpUsage{Run: f.run},
	})
	c, _ := e.client()
	call(t, c, wire.MethodPresenceSet, wire.PresenceSetParams{Foreground: true}, nil)
	time.Sleep(50 * time.Millisecond)
	if l, h := f.counts(); l+h != 0 {
		t.Fatalf("omp ran: %d live, %d history", l, h)
	}
}
