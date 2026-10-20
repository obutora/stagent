package schedule

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/obutora/stagent/internal/wire"
)

// MaxRuns is how many runs (times, their retries in their lines) a
// schedule's record keeps, the newest ones.
const MaxRuns = 200

// RunLog is the record of every schedule's runs: Dir/<schedule id>.jsonl,
// one line per run (a time of the schedule, or run_now; its retries go on
// in its line, AddTry), oldest first. A run's line is written when it
// starts and rewritten when a try of it starts or ends; each write
// replaces the file (it holds MaxRuns lines at most). Its writer, the
// daemon, serializes the calls.
type RunLog struct {
	Dir string
}

func (rl RunLog) path(id string) string { return filepath.Join(rl.Dir, id+".jsonl") }

// read returns the runs of schedule id, oldest first. Lines that do not
// decode are dropped.
func (rl RunLog) read(id string) ([]wire.ScheduleRun, error) {
	b, err := os.ReadFile(rl.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []wire.ScheduleRun
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var r wire.ScheduleRun
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.RunID != "" {
			runs = append(runs, r)
		}
	}
	return runs, sc.Err()
}

func (rl RunLog) write(id string, runs []wire.ScheduleRun) error {
	var buf bytes.Buffer
	for _, r := range runs {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.MkdirAll(rl.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(rl.Dir, "."+id+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), rl.path(id))
}

// Add records a new run of schedule id, dropping the oldest beyond
// MaxRuns.
func (rl RunLog) Add(id string, r wire.ScheduleRun) error {
	runs, err := rl.read(id)
	if err != nil {
		return err
	}
	runs = append(runs, r)
	if len(runs) > MaxRuns {
		runs = runs[len(runs)-MaxRuns:]
	}
	return rl.write(id, runs)
}

// Update replaces the line of r.RunID; a run no longer kept is not
// added again.
func (rl RunLog) Update(id string, r wire.ScheduleRun) error {
	runs, err := rl.read(id)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(runs, func(x wire.ScheduleRun) bool { return x.RunID == r.RunID })
	if i < 0 {
		return nil
	}
	runs[i] = r
	return rl.write(id, runs)
}

// AddTry starts try as the newest of run r's line (a retry): r's newest
// try moves to its tries. It returns r as it is now; a run no longer kept
// is not added again.
func (rl RunLog) AddTry(id string, r wire.ScheduleRun, try wire.ScheduleTry) (wire.ScheduleRun, error) {
	r.Tries = append(slices.Clip(r.Tries), r.ScheduleTry)
	r.ScheduleTry = try
	return r, rl.Update(id, r)
}

// List returns the runs of schedule id, newest first.
func (rl RunLog) List(id string) ([]wire.ScheduleRun, error) {
	runs, err := rl.read(id)
	slices.Reverse(runs)
	return runs, err
}

// Last returns the newest run of schedule id (false: none).
func (rl RunLog) Last(id string) (wire.ScheduleRun, bool, error) {
	runs, err := rl.read(id)
	if err != nil || len(runs) == 0 {
		return wire.ScheduleRun{}, false, err
	}
	return runs[len(runs)-1], true, nil
}

// Running returns the runs of schedule id still marked running.
func (rl RunLog) Running(id string) ([]wire.ScheduleRun, error) {
	runs, err := rl.read(id)
	return slices.DeleteFunc(runs, func(r wire.ScheduleRun) bool { return r.Result != wire.RunRunning }), err
}

// Remove deletes the record of schedule id (none is fine).
func (rl RunLog) Remove(id string) error {
	err := os.Remove(rl.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
