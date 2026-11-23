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

// fakeAgy answers `agy -p /usage` with the fixture (or out), counting
// runs.
type fakeAgy struct {
	mu   sync.Mutex
	runs int
	out  []byte
	err  error
}

func (f *fakeAgy) run(context.Context, string, ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	if f.err != nil || f.out != nil {
		return f.out, f.err
	}
	return os.ReadFile(filepath.Join("..", "usage", "testdata", "agy-usage.json"))
}

func (f *fakeAgy) set(out []byte, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out, f.err = out, err
}

func (f *fakeAgy) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// startAgy runs a daemon whose agy is the fake. The test drives callAgy
// itself with its own schedule: the daemon's goroutine never finds agy
// due, no app being in front and the usage screen never opened.
func startAgy(t *testing.T, bin string) (*apiEnv, *fakeAgy, string) {
	t.Helper()
	f := &fakeAgy{}
	clk := &clock{t: time.Date(2026, 11, 23, 5, 0, 0, 0, time.UTC)}
	e := startDaemon(t, fastConfig(), Options{
		Now:            clk.now,
		PrivateAPITick: 5 * time.Millisecond,
		AgyBin:         func() string { return bin },
		Agy:            usage.AgyUsage{Run: f.run},
	})
	return &apiEnv{testEnv: e, clk: clk, logs: &logs{}}, f, filepath.Join(e.layout.Home, ".gemini", "antigravity-cli")
}

// agyAccount is usage.get's antigravity account of dir.
func agyAccount(t *testing.T, e *apiEnv, dir string) (wire.UsageLimits, bool) {
	t.Helper()
	c, _ := e.client()
	var r wire.UsageGetResult
	call(t, c, wire.MethodUsageGet, wire.UsageGetParams{}, &r)
	for _, a := range r.Limits {
		if a.Harness == wire.HarnessAntigravity {
			if a.ConfigDir != dir {
				t.Fatalf("antigravity account under %q, want %q", a.ConfigDir, dir)
			}
			return a, true
		}
	}
	return wire.UsageLimits{}, false
}

// agy runs on the private API's schedule — in front once per 15 minutes,
// on opening the usage screen after 5, never in the background — and its
// windows land under (antigravity, Antigravity CLI's home) in agy's
// order with their labels; a failure becomes the windows' error, the
// values kept, and waits before the next run; a success clears it.
func TestAgyUsageScheduledAndRecorded(t *testing.T) {
	e, f, dir := startAgy(t, "/fake/agy")
	ctx := context.Background()
	s := &agyState{}

	e.d.callAgy(ctx, s, false, false)
	if f.count() != 0 {
		t.Fatal("agy ran in the background")
	}
	e.d.callAgy(ctx, s, true, false)
	a, ok := agyAccount(t, e, dir)
	if f.count() != 1 || !ok {
		t.Fatalf("runs %d, account %v", f.count(), ok)
	}
	var ids []string
	for _, w := range a.Windows {
		ids = append(ids, w.Window+"="+w.Label)
		if w.Source != wire.UsageSourceAntigravityUsage || w.MeasuredAt != e.clk.now().UnixMilli() || w.Error != nil {
			t.Errorf("window %+v", w)
		}
	}
	want := "five_hour=,seven_day=,five_hour_3p=Claude and GPT models,seven_day_3p=Claude and GPT models,seven_day_geminiargonwtu=Gemini 4 Argon"
	if got := strings.Join(ids, ","); got != want {
		t.Fatalf("windows %s\nwant    %s", got, want)
	}
	if pct(a.Windows[0]) != 14.27 || pct(a.Windows[1]) != 9.42 {
		t.Fatalf("shares %v %v", pct(a.Windows[0]), pct(a.Windows[1]))
	}

	e.advance(15*time.Minute - time.Second)
	e.d.callAgy(ctx, s, true, false)
	if f.count() != 1 {
		t.Fatalf("ran again within 15 minutes: %d", f.count())
	}

	f.set(nil, errors.New("exit status 1"))
	e.advance(time.Second)
	e.d.callAgy(ctx, s, true, false)
	a, _ = agyAccount(t, e, dir)
	for _, w := range a.Windows {
		if w.Error == nil || w.Error.Code != wire.UsageErrCommand || w.UsedPercent == nil {
			t.Fatalf("after a failure %+v", w)
		}
	}
	e.d.callAgy(ctx, s, true, true)
	if f.count() != 2 {
		t.Fatalf("ran again during the back-off: %d", f.count())
	}

	f.set(nil, nil)
	e.advance(10 * time.Minute)
	e.d.callAgy(ctx, s, false, true)
	a, _ = agyAccount(t, e, dir)
	if f.count() != 3 || len(a.Windows) != 5 {
		t.Fatalf("runs %d, windows %+v", f.count(), a.Windows)
	}
	for _, w := range a.Windows {
		if w.Error != nil {
			t.Fatalf("error kept after a success: %+v", w)
		}
	}
}

// A failure before anything was measured (agy logged in with an API key,
// or not at all) is only logged: the account gets no windows.
func TestAgyUsageFailureBeforeMeasured(t *testing.T) {
	e, f, dir := startAgy(t, "/fake/agy")
	ctx := context.Background()
	s := &agyState{}
	f.set(nil, errors.New("exit status 2"))
	e.d.callAgy(ctx, s, true, false)
	f.set([]byte(`{"status":"ERROR"}`), nil)
	e.advance(time.Minute)
	e.d.callAgy(ctx, s, true, false)
	if a, ok := agyAccount(t, e, dir); ok || f.count() != 2 {
		t.Fatalf("runs %d, account %+v", f.count(), a)
	}
}

// Without agy nothing runs and nothing is recorded.
func TestAgyNotInstalled(t *testing.T) {
	e, f, dir := startAgy(t, "")
	e.d.callAgy(context.Background(), &agyState{}, true, true)
	if _, ok := agyAccount(t, e, dir); ok || f.count() != 0 {
		t.Fatalf("agy ran %d times", f.count())
	}
}

// STAGENT_HOME set (the test daemons are isolated): agy is never looked
// for.
func TestAgyNotLookedForWhenIsolated(t *testing.T) {
	e := startDaemon(t, fastConfig(), Options{})
	if e.d.opts.AgyBin != nil {
		t.Fatal("agy looked for under STAGENT_HOME")
	}
}
