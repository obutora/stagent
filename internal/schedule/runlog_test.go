package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obutora/stagent/internal/wire"
)

func run(n int) wire.ScheduleRun {
	return wire.ScheduleRun{RunID: fmt.Sprintf("run-%03d", n), ScheduledAt: int64(n), Trigger: wire.RunTriggerScheduled,
		ScheduleTry: wire.ScheduleTry{StartedAt: int64(n), Result: wire.RunSucceeded}}
}

// The 201st run drops the oldest line; the newest 200 stay.
func TestRunLogKeepsNewest200(t *testing.T) {
	rl := RunLog{Dir: t.TempDir()}
	for n := 1; n <= MaxRuns; n++ {
		if err := rl.Add("s1", run(n)); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := rl.List("s1")
	if err != nil || len(runs) != MaxRuns {
		t.Fatalf("after %d runs: %d listed, %v", MaxRuns, len(runs), err)
	}
	if err := rl.Add("s1", run(MaxRuns+1)); err != nil {
		t.Fatal(err)
	}
	runs, _ = rl.List("s1")
	if len(runs) != MaxRuns {
		t.Fatalf("after %d runs: %d listed", MaxRuns+1, len(runs))
	}
	if runs[0].RunID != "run-201" || runs[len(runs)-1].RunID != "run-002" {
		t.Errorf("newest %s, oldest %s; want run-201, run-002", runs[0].RunID, runs[len(runs)-1].RunID)
	}
	b, _ := os.ReadFile(filepath.Join(rl.Dir, "s1.jsonl"))
	if lines := strings.Count(string(b), "\n"); lines != MaxRuns {
		t.Errorf("file has %d lines, want %d", lines, MaxRuns)
	}
}

func TestRunLogUpdateAndRemove(t *testing.T) {
	rl := RunLog{Dir: t.TempDir()}
	r := run(1)
	r.Result = wire.RunRunning
	rl.Add("s1", r)
	rl.Add("s1", run(2))
	r.Result, r.ExitCode, r.EndedAt = wire.RunFailed, new(2), 9
	if err := rl.Update("s1", r); err != nil {
		t.Fatal(err)
	}
	runs, _ := rl.List("s1")
	if got := runs[1]; got.Result != wire.RunFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("updated run: %+v", got)
	}
	if err := rl.Remove("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rl.Dir, "s1.jsonl")); !os.IsNotExist(err) {
		t.Errorf("record file still there: %v", err)
	}
	if runs, err := rl.List("s1"); err != nil || len(runs) != 0 {
		t.Errorf("after remove: %v, %v", runs, err)
	}
	if err := rl.Remove("s1"); err != nil {
		t.Errorf("removing again: %v", err)
	}
}

// A line a later version wrote keeps the members this one does not know;
// the members of its newest try are known ones.
func TestRunLogKeepsUnknownMembers(t *testing.T) {
	rl := RunLog{Dir: t.TempDir()}
	line := `{"run_id":"r1","scheduled_at":1,"started_at":1,"trigger":"late","result":"running","summary":"ok","later":{"n":1,"of":2}}` + "\n"
	os.WriteFile(filepath.Join(rl.Dir, "s1.jsonl"), []byte(line), 0o600)
	runs, _ := rl.List("s1")
	r := runs[0]
	if len(r.Extra) != 1 || r.Extra["later"] == nil || r.Summary != "ok" {
		t.Errorf("read %+v (extra %v)", r, r.Extra)
	}
	r.Result = wire.RunSucceeded
	if err := rl.Update("s1", r); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(rl.Dir, "s1.jsonl"))
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["summary"] != "ok" || got["later"] == nil || got["trigger"] != "late" || got["result"] != "succeeded" {
		t.Errorf("rewritten line: %s", b)
	}
}

// A retry goes on in its time's line: the try before it moves to tries,
// the line stays one (and one of MaxRuns).
func TestRunLogAddTry(t *testing.T) {
	rl := RunLog{Dir: t.TempDir()}
	r := run(1)
	r.Retries = 1
	r.Result, r.SessionID, r.ExitCode, r.EndedAt = wire.RunFailed, "sess-1", new(1), 5
	rl.Add("s1", r)
	r, err := rl.AddTry("s1", r, wire.ScheduleTry{Retry: 1, StartedAt: 9, Result: wire.RunRunning, SessionID: "sess-2"})
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := rl.List("s1")
	if len(runs) != 1 {
		t.Fatalf("runs %+v", runs)
	}
	got := runs[0]
	if got.RunID != "run-001" || got.Retry != 1 || got.Result != wire.RunRunning || got.SessionID != "sess-2" || got.ExitCode != nil ||
		len(got.Tries) != 1 || got.Tries[0].SessionID != "sess-1" || got.Tries[0].Result != wire.RunFailed || got.Tries[0].EndedAt != 5 {
		t.Errorf("line after the retry started %+v", got)
	}
	if got.Extra != nil || r.RunID != got.RunID || len(r.Tries) != 1 {
		t.Errorf("extra %v, returned %+v", got.Extra, r)
	}
	b, _ := os.ReadFile(filepath.Join(rl.Dir, "s1.jsonl"))
	var flat map[string]any
	json.Unmarshal(b, &flat)
	if flat["session_id"] != "sess-2" || flat["retry"] != float64(1) || flat["tries"] == nil || flat["ScheduleTry"] != nil {
		t.Errorf("line %s", b)
	}
}
