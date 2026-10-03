package paths

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// WrapperRunRecord is the content of Layout.WrapperRun, written at every
// start through a shell wrapper and read by `stagent doctor`.
type WrapperRunRecord struct {
	// At is when the program was started, unix ms.
	At int64 `json:"at"`
	// SurvivesLogout says whether `stagent run`, where it ended up after
	// leaving the login session (logind.Escape, Linux) or swapping its
	// bootstrap port (bootstrap.Swap, macOS), outlives the user's logout;
	// nil when unknown.
	SurvivesLogout *bool `json:"survives_logout"`
	// BootstrapError is why the swap failed on macOS; "" otherwise.
	BootstrapError string `json:"bootstrap_error,omitempty"`
}

// WriteWrapperRun stores rec. Best effort: errors are ignored, it never
// holds up the program.
func (l *Layout) WriteWrapperRun(rec WrapperRunRecord) {
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if os.WriteFile(l.WrapperRun, b, 0o600) == nil {
		return
	}
	if os.MkdirAll(filepath.Dir(l.WrapperRun), 0o700) == nil {
		os.WriteFile(l.WrapperRun, b, 0o600)
	}
}

// ReadWrapperRun returns the last record; ok is false when there is none
// or it is unreadable. A file of stagent before 0.4.0 holds only the time.
func (l *Layout) ReadWrapperRun() (rec WrapperRunRecord, ok bool) {
	b, err := os.ReadFile(l.WrapperRun)
	if err != nil {
		return rec, false
	}
	b = bytes.TrimSpace(b)
	if ms, err := strconv.ParseInt(string(b), 10, 64); err == nil {
		return WrapperRunRecord{At: ms}, true
	}
	if json.Unmarshal(b, &rec) != nil || rec.At == 0 {
		return WrapperRunRecord{}, false
	}
	return rec, true
}
