// Package hook is `stagent hook <harness> [event] [json]`, the command
// harness hooks run. It forwards the hook payload to the daemon and prints
// nothing: approvals are answered on the program's own screen. It never
// breaks a harness: every path exits 0 and the daemon is never started from
// here. Waiting is bounded except for claude's PermissionRequest, which the
// daemon holds until its approval closes (the session's holder saw the
// prompt leave the screen, or claude stopped the hook, or the turn ended).
package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/ptable"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

const (
	// maxPayload bounds the hook payload read from stdin.
	maxPayload = 1 << 20
	// stdinWait bounds reading stdin; harnesses write the payload at once.
	stdinWait = 2 * time.Second
	// dialTimeout: an absent daemon must not slow the harness down.
	dialTimeout = 500 * time.Millisecond
	// callTimeout bounds every call but claude's PermissionRequest.
	callTimeout = 3 * time.Second

	eventPermissionRequest = "PermissionRequest"
	codexNotify            = "notify"
)

// procIO is the process environment, replaceable in tests.
type procIO struct {
	stdin     io.Reader
	stdinTTY  bool
	sessionID string
	// remoteControl: $CLAUDE_CODE_BRIDGE_SESSION_ID is set (claude is
	// connected to Remote Control).
	remoteControl bool
	// launch tells how the agent was started, for a hook outside any
	// stagent session (see wire.HookEventParams), given the payload.
	launch func(harness string, payload []byte) launchInfo
}

// launchInfo is what a hook outside a stagent session tells the daemon
// about the agent's start (wire.HookEventParams).
type launchInfo struct {
	shellWrapper     bool
	entrypoint       string
	parentTTY        *bool
	parentBatch      bool
	ssh              bool
	terminalAncestor *bool
	originator       string
	subagent         bool
}

// Main runs the hook command. It always returns 0.
func Main(args []string) int {
	return run(args, procIO{
		stdin:         os.Stdin,
		stdinTTY:      term.IsTerminal(int(os.Stdin.Fd())),
		sessionID:     os.Getenv(wire.EnvSessionID),
		remoteControl: os.Getenv(wire.EnvClaudeBridgeSessionID) != "",
		launch:        osLaunch,
	})
}

// osLaunch reads the launch facts from the environment, the harness that
// runs the hook (harnessPID; on Windows winAncestry) and, for Codex, the
// conversation's rollout file.
func osLaunch(harness string, payload []byte) launchInfo {
	li := launchInfo{
		shellWrapper: os.Getenv(wire.EnvShellWrapper) != "",
		entrypoint:   os.Getenv("CLAUDE_CODE_ENTRYPOINT"),
		ssh:          os.Getenv("SSH_CONNECTION") != "",
	}
	pid := 0
	if runtime.GOOS == "windows" {
		if s, err := ptable.Take(); err == nil {
			var terminal bool
			if pid, terminal = winAncestry(s, os.Getpid()); pid != 0 {
				li.terminalAncestor = &terminal
			}
		}
	} else {
		pid = harnessPID()
		if tty, known := ptable.ReadsTerminal(pid); known {
			li.parentTTY = &tty
		}
	}
	if pid != 0 {
		if argv, err := ptable.Argv(pid); err == nil {
			li.parentBatch = batchArgs(harness, argv)
		}
	}
	if harness == wire.HarnessCodex {
		li.originator, li.subagent = codexOrigin(payload)
	}
	return li
}

// hookShells are the shells harnesses run hook commands with. The
// installed codex command (`[ -x … ] && … hook codex || true`) keeps `sh -c`
// as the hook's parent, its stdin the payload pipe.
var hookShells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ash": true, "ksh": true, "busybox": true}

// harnessPID is the hook's nearest ancestor that is not one of hookShells.
func harnessPID() int {
	pid := os.Getppid()
	for range 3 {
		p, ok := ptable.Process(pid)
		if !ok || !hookShells[p.Name] || p.PPID <= 1 {
			break
		}
		pid = p.PPID
	}
	return pid
}

// batchArgs reports whether a harness command line (argv[0] first) asks for
// a non-interactive run, by the shell wrapper's rule: claude -p / --print;
// codex exec / e as the first argument; omp -p / --print or --mode other
// than text. omp runs under its JavaScript runtime, so every argument is
// looked at.
func batchArgs(harness string, argv []string) bool {
	if len(argv) < 2 {
		return false
	}
	args := argv[1:]
	switch harness {
	case wire.HarnessCodex:
		return args[0] == "exec" || args[0] == "e"
	case wire.HarnessClaude, wire.HarnessOmp:
	default:
		return false
	}
	omp := harness == wire.HarnessOmp
	for i, a := range args {
		switch {
		case a == "-p" || a == "--print":
			return true
		case omp && a == "--mode" && i+1 < len(args) && args[i+1] != "text":
			return true
		case omp && strings.HasPrefix(a, "--mode=") && a != "--mode=text":
			return true
		}
	}
	return false
}

func run(args []string, pio procIO) (code int) {
	defer func() {
		if recover() != nil {
			code = 0
		}
	}()
	if len(args) == 0 {
		return 0
	}
	harness, rest := args[0], args[1:]
	event := ""
	var payload []byte
	switch {
	case harness == wire.HarnessCodex && len(rest) > 0 && rest[0] == codexNotify:
		// Codex's notify program gets the JSON as its last argument.
		event = codexNotify
		if len(rest) > 1 {
			payload = []byte(rest[len(rest)-1])
		}
	default:
		if len(rest) > 0 {
			event = rest[0]
		}
		if len(rest) > 1 && json.Valid([]byte(rest[len(rest)-1])) {
			payload = []byte(rest[len(rest)-1])
		} else if !pio.stdinTTY && pio.stdin != nil {
			payload = readPayload(pio.stdin)
		}
	}
	payload = normalizePayload(payload)
	if event == "" {
		event = eventName(payload)
	}

	l, err := paths.Resolve()
	if err != nil {
		return 0
	}
	conn, err := daemonclient.Dial(l, dialTimeout)
	if err != nil {
		return 0
	}
	client := rpc.NewClient(conn, nil)
	defer client.Close()

	// claude shows its prompt while the hook runs, so the daemon may hold
	// it until the prompt is answered (it answers at once outside stagent
	// sessions). Codex shows its prompt only after the hook returned.
	ctx := context.Background()
	if harness != wire.HarnessClaude || event != eventPermissionRequest {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
		defer cancel()
	}
	params := wire.HookEventParams{
		Harness:       harness,
		Event:         event,
		SessionID:     pio.sessionID,
		Payload:       payload,
		RemoteControl: pio.remoteControl,
	}
	if pio.sessionID == "" && pio.launch != nil {
		li := pio.launch(harness, payload)
		params.ShellWrapper, params.Entrypoint = li.shellWrapper, li.entrypoint
		params.ParentTTY, params.ParentBatch = li.parentTTY, li.parentBatch
		params.SSH, params.TerminalAncestor = li.ssh, li.terminalAncestor
		params.Originator, params.Subagent = li.originator, li.subagent
	}
	client.Call(ctx, wire.MethodHookEvent, params, nil)
	return 0
}

// lockedBuf collects what the stdin reader saw, up to maxPayload+1 bytes.
type lockedBuf struct {
	mu sync.Mutex
	b  []byte
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := maxPayload + 1 - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (l *lockedBuf) bytes() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte(nil), l.b...)
}

// readPayload reads one JSON value from r, returning as soon as it is
// complete (stdin need not be closed) or after stdinWait with whatever
// arrived. Oversized or truncated payloads come back as they are and are
// reduced by normalizePayload.
func readPayload(r io.Reader) []byte {
	var seen lockedBuf
	type result struct {
		raw json.RawMessage
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var raw json.RawMessage
		err := json.NewDecoder(io.TeeReader(io.LimitReader(r, maxPayload+1), &seen)).Decode(&raw)
		ch <- result{raw, err}
	}()
	select {
	case res := <-ch:
		if res.err == nil {
			return res.raw
		}
	case <-time.After(stdinWait):
	}
	return seen.bytes()
}

// Fields kept from a payload too large (or too broken) to forward whole.
var salvageFields = []string{
	"hook_event_name", "session_id", "transcript_path", "cwd", "tool_name",
	"notification_type", "permission_mode", "type", "thread-id",
}

var salvageInputFields = []string{"command", "file_path", "path", "url", "pattern", "description"}

// normalizePayload returns payload when it is valid JSON of at most
// maxPayload bytes; otherwise a small object with the fields stagent needs,
// scraped from the bytes that were read (nil when nothing is left).
func normalizePayload(payload []byte) []byte {
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return nil
	}
	if len(payload) <= maxPayload && json.Valid(payload) {
		return payload
	}
	obj := map[string]any{}
	for _, f := range salvageFields {
		if v, ok := scrapeString(payload, f); ok {
			obj[f] = v
		}
	}
	in := map[string]string{}
	for _, f := range salvageInputFields {
		if v, ok := scrapeString(payload, f); ok {
			in[f] = v
		}
	}
	if len(in) > 0 {
		obj["tool_input"] = in
	}
	if len(obj) == 0 {
		return nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return b
}

// maxScraped bounds a scraped string value.
const maxScraped = 4096

// scrapeString finds the first "field": "value" pair in raw (possibly
// truncated) JSON text.
func scrapeString(raw []byte, field string) (string, bool) {
	key := []byte(`"` + field + `"`)
	for from := 0; ; {
		i := bytes.Index(raw[from:], key)
		if i < 0 {
			return "", false
		}
		j := skipSpace(raw, from+i+len(key))
		from += i + len(key)
		if j >= len(raw) || raw[j] != ':' {
			continue // the name appeared as a value
		}
		j = skipSpace(raw, j+1)
		if j >= len(raw) || raw[j] != '"' {
			return "", false
		}
		for k := j + 1; k < len(raw) && k-j <= maxScraped; k++ {
			switch raw[k] {
			case '\\':
				k++
			case '"':
				var s string
				if json.Unmarshal(raw[j:k+1], &s) != nil {
					return "", false
				}
				return s, true
			}
		}
		return "", false
	}
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// eventName reads hook_event_name from a payload.
func eventName(payload []byte) string {
	var p struct {
		Name string `json:"hook_event_name"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return ""
	}
	return p.Name
}
