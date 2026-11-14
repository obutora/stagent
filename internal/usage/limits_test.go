package usage

import (
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// The newer measurement wins: after a restart the daemon records the
// newest rollout event again, hours old, with the share the private API
// measured since; the private API's value stays.
func TestLimitsKeepNewerMeasurementOfSameWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	home := "/home/u/.codex"
	now := time.Unix(1_794_600_000, 0)
	reset := now.Add(2 * time.Hour).UnixMilli()
	l := OpenLimits(path)
	if _, err := l.Record(wire.HarnessCodex, home, Measurement{Source: wire.UsageSourcePrivateAPI, At: now.Add(-10 * time.Minute),
		Windows: []Window{{ID: wire.UsageWindowFiveHour, UsedPercent: 30, ResetsAt: reset}}}); err != nil {
		t.Fatal(err)
	}

	l = OpenLimits(path) // the daemon restarted
	changed, err := l.Record(wire.HarnessCodex, home, Measurement{Source: wire.UsageSourceCodexRollout, At: now.Add(-3 * time.Hour),
		Windows: []Window{{ID: wire.UsageWindowFiveHour, UsedPercent: 30, ResetsAt: reset + 20_000}}})
	if err != nil {
		t.Fatal(err)
	}
	w := l.Report(now)[0].Windows[0]
	if changed || w.Source != wire.UsageSourcePrivateAPI || w.MeasuredAt != now.Add(-10*time.Minute).UnixMilli() || w.ResetsAt != reset {
		t.Fatalf("changed %v, five_hour = %+v", changed, w)
	}

	// A newer measurement of the same window still replaces it.
	if _, err := l.Record(wire.HarnessCodex, home, Measurement{Source: wire.UsageSourceCodexRollout, At: now,
		Windows: []Window{{ID: wire.UsageWindowFiveHour, UsedPercent: 31, ResetsAt: reset}}}); err != nil {
		t.Fatal(err)
	}
	if w := l.Report(now)[0].Windows[0]; w.Source != wire.UsageSourceCodexRollout || *w.UsedPercent != 31 {
		t.Fatalf("newer measurement not taken: %+v", w)
	}
}

// Records made at once, written outside the lock, leave the file as the
// last of them left the windows: an older snapshot never replaces a newer
// one.
func TestLimitsFileFollowsConcurrentRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage-limits.json")
	l := OpenLimits(path)
	now := time.Unix(1_794_600_000, 0)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			dir := "/home/u/.claude-" + strconv.Itoa(i%4)
			if _, err := l.Record(wire.HarnessClaude, dir, Measurement{Source: wire.UsageSourceStatusLine, At: now,
				Windows: []Window{{ID: wire.UsageWindowFiveHour, UsedPercent: float64(i), ResetsAt: now.Add(time.Hour).UnixMilli()}}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got, want := OpenLimits(path).Report(now), l.Report(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("file holds %+v, memory %+v", got, want)
	}
}
