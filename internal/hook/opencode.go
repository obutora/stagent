package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/obutora/stagent/internal/fsutil"
	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/transcript"
	"github.com/obutora/stagent/internal/wire"
)

// openCodeHook: stagent's plugin for OpenCode (install/opencode.go) passes
// the event as an argument and Claude Code's payload, plus exec_path, the
// OpenCode binary its server runs. The plugin runs in OpenCode's server,
// the child of the terminal UI (or of `opencode run`): how OpenCode was
// started is read from the server's parent, and a run without the terminal
// UI reports nothing. The payload gets the session's transcript, stagent's
// record of it (transcript/opencode.go), which the pseudo-event Transcript
// brings up to date; that one never reaches the daemon.
var openCodeHook = hookHarness{
	payload: openCodePayload,
	local:   openCodeLocal,
	launch: func(li *launchInfo, _ []byte) {
		p, ok := openCodeParent()
		if !ok {
			return
		}
		tty := p.tty
		li.parentTTY = &tty
		li.parentBatch = harness.BatchArgs(wire.HarnessOpenCode, p.argv)
	},
}

// eventOpenCodeTranscript is the pseudo-event that reads the session again.
const eventOpenCodeTranscript = "Transcript"

// openCodeSessionID is what an OpenCode session id looks like (ses_…): a
// file name of its own.
var openCodeSessionID = regexp.MustCompile(`^ses_[A-Za-z0-9]+$`)

// openCodeProc is the process that started the hook's OpenCode server.
type openCodeProc struct {
	pid   int
	start time.Time
	argv  []string
	tty   bool // it reads a terminal
}

// openCodeParent returns the parent of the hook's OpenCode server.
var openCodeParent = func() (openCodeProc, bool) {
	server, ok := ptable.Process(harnessPID())
	if !ok {
		return openCodeProc{}, false
	}
	p, ok := ptable.Process(server.PPID)
	if !ok {
		return openCodeProc{}, false
	}
	argv, _ := ptable.Argv(p.PID)
	tty, _ := ptable.ReadsTerminal(p.PID)
	return openCodeProc{pid: p.PID, start: p.Start, argv: argv, tty: tty}, true
}

type openCodePayloadFields struct {
	SessionID string `json:"session_id"`
	ExecPath  string `json:"exec_path"`
}

// openCodeTranscript is stagent's record of the session the payload names,
// "" when it names none.
func openCodeTranscript(payload []byte) (path, id string) {
	var pl openCodePayloadFields
	if json.Unmarshal(payload, &pl) != nil || !openCodeSessionID.MatchString(pl.SessionID) {
		return "", ""
	}
	l, err := paths.Resolve()
	if err != nil {
		return "", ""
	}
	root := transcript.DefaultRoots(l.Home, os.Getenv)[wire.HarnessOpenCode]
	return filepath.Join(root, pl.SessionID+".jsonl"), pl.SessionID
}

// openCodePayload adds the session's transcript to the payload.
func openCodePayload(payload []byte) []byte {
	path, _ := openCodeTranscript(payload)
	if path == "" {
		return payload
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	m["transcript_path"], _ = json.Marshal(path)
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return b
}

// openCodeLocal drops the events of a run without the terminal UI and
// handles Transcript.
func openCodeLocal(event string, payload []byte) bool {
	p, ok := openCodeParent()
	if ok && harness.BatchArgs(wire.HarnessOpenCode, p.argv) {
		return true
	}
	if event != eventOpenCodeTranscript {
		return false
	}
	syncOpenCode(payload, p, ok)
	return true
}

// openCodeExportTimeout bounds reading a session.
const openCodeExportTimeout = 30 * time.Second

// syncOpenCode reads the session the payload names with `opencode session
// export --standalone` (a server of its own: the background service is
// never started) and brings stagent's record up to it; beside it, it
// records the OpenCode process that reports the session (follow).
func syncOpenCode(payload []byte, proc openCodeProc, procOK bool) {
	path, id := openCodeTranscript(payload)
	if path == "" {
		return
	}
	var pl openCodePayloadFields
	json.Unmarshal(payload, &pl)
	ctx, cancel := context.WithTimeout(context.Background(), openCodeExportTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, openCodeBinary(pl.ExecPath), "session", "export", "--standalone", id)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil || transcript.SyncOpenCode(path, out) != nil {
		return
	}
	if procOK && proc.pid > 0 {
		sidecar := fmt.Sprintf(`{"pid":%d,"started":%d}`, proc.pid, proc.start.UnixMilli())
		if b, err := os.ReadFile(transcript.OpenCodeProcPath(path)); err != nil || string(b) != sidecar {
			fsutil.WriteFileAtomic(transcript.OpenCodeProcPath(path), []byte(sidecar))
		}
	}
}

// openCodeBinary is the OpenCode binary to read a session with: the one
// the payload names when it is an absolute path to an OpenCode binary,
// else opencode on PATH.
func openCodeBinary(execPath string) string {
	if filepath.IsAbs(execPath) && ptable.BaseName(execPath) == "opencode" {
		return execPath
	}
	return "opencode"
}
