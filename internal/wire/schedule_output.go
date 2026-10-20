package wire

// schedule.output: the output of a schedule's run, read by the daemon from
// what the run's session left on disk (its scrollback), so it is there
// after the session's holder has exited (session.scrollback is the
// holder's). Coding agents may read it.
const MethodScheduleOutput = "schedule.output"

// ScheduleOutputParams names the run (ID: its schedule), which of its
// tries (SessionID: the try's session; omitted: its newest try) and the
// page of its output, as ScrollbackParams does.
type ScheduleOutputParams struct {
	ID        string `json:"id"`
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id,omitempty"`
	// Before is an exclusive byte offset into the run's output stream;
	// 0 means "up to the latest byte".
	Before   int64 `json:"before,omitempty"`
	MaxBytes int   `json:"max_bytes,omitempty"` // default 256 KiB, max 1 MiB
}

// ScheduleOutputResult is a page of a run's output as session.scrollback
// answers it. Gone: the run's output is not kept (any more): retention
// deleted it, or the run had no session; Data is then empty, and the
// run's summary is what is left.
type ScheduleOutputResult struct {
	ScrollbackResult
	Gone bool `json:"gone,omitempty"`
}
