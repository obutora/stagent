package transcript

import (
	"bytes"
	"encoding/json"
	"strings"
)

// pi: <agent dir>/sessions/--<cwd, / \ : as ->--/<timestamp>_<id>.jsonl,
// session format v3. Line 1 is the session header; every later line is an
// entry appended to the file. Its layout and message entries are omp's (omp
// is a fork of Pi), so ompSessionFormat lists them and parseOmp reads them:
// it keeps the user and assistant messages and leaves out what v3 added
// besides (role=system prompt sections, custom_message entries) with the
// other entry types.

var piFormat = ompSessionFormat(scanPi, titleOrPrompt)

var piSessionInfo = []byte(`{"type":"session_info"`)

// scanPi reads the header and the session's name, the last session_info
// entry (/name), from where the previous scan stopped: the file is
// append-only. Until it has a name the first prompt titles it, as in Pi's
// /resume.
func scanPi(path string, e *entry) {
	forLines(path, e.Scanned, 0, func(line []byte, complete bool) bool {
		if !complete {
			return true
		}
		switch {
		case e.ID == "" && bytes.HasPrefix(line, []byte(`{"type":"session",`)):
			var h struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Cwd  string `json:"cwd"`
			}
			if json.Unmarshal(line, &h) == nil && h.Type == "session" {
				e.ID, e.Cwd = h.ID, h.Cwd
			}
		case bytes.HasPrefix(line, piSessionInfo):
			var r struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(line, &r) == nil {
				e.Title = clip(strings.TrimSpace(r.Name), 1024)
			}
		case e.Prompt == "":
			for _, m := range parseOmp(line) {
				if m.Role == RoleUser {
					e.Prompt = clip(m.Text, 1024)
				}
			}
		}
		return true
	}, &e.Scanned)
	if e.ID == "" {
		e.ID = ompFileID(path)
	}
}
