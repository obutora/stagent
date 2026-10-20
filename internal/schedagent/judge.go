package schedagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/obutora/stagent/internal/wire"
)

// Outcome is how an agent run went, as the wrapper judged it from the
// agent's JSON output and exit code. The wrapper leaves it in the file
// the daemon names (--result); the daemon records it on the run.
type Outcome struct {
	// Result is wire.RunSucceeded, RunDenied or RunFailed.
	Result         string `json:"result"`
	Denials        int    `json:"denials,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Summary        string `json:"summary,omitempty"`
	// Unjudged: the output was not the agent's JSON; Result is the exit
	// code's alone.
	Unjudged bool `json:"unjudged,omitempty"`
	// Error says why it failed, when the output says.
	Error string `json:"error,omitempty"`
	// AuthFailed: the run failed because the agent could not log in
	// (claude asked for /login or was refused with HTTP 401).
	AuthFailed bool `json:"auth_failed,omitempty"`
}

// reader reads an agent's JSON output line by line, writes what a person
// reads of it to w and gathers the outcome.
type reader struct {
	harness string
	w       io.Writer
	// parsed: some line was the harness's JSON.
	parsed  bool
	failed  bool
	errMsg  string
	denials int
	conv    string
	summary string
	// codex: the last error event, and whether a turn completed.
	codexError    string
	turnCompleted bool
	// authFailed: the agent could not log in.
	authFailed bool
}

func newReader(harness string, w io.Writer) *reader {
	return &reader{harness: harness, w: w}
}

func (r *reader) say(format string, args ...any) {
	fmt.Fprintf(r.w, format+"\n", args...)
}

// line takes one line of the agent's standard output. A line that is not
// the harness's JSON is shown as it is.
func (r *reader) line(b []byte) {
	b = bytes.TrimRight(b, "\r\n")
	if len(bytes.TrimSpace(b)) == 0 {
		return
	}
	var ev map[string]json.RawMessage
	if json.Unmarshal(b, &ev) != nil {
		r.say("%s", b)
		return
	}
	known := false
	switch r.harness {
	case wire.HarnessClaude:
		known = r.claude(ev, b)
	case wire.HarnessCodex:
		known = r.codex(ev)
	case wire.HarnessOmp:
		known = r.omp(ev, b)
	}
	if !known {
		r.say("%s", b)
		return
	}
	r.parsed = true
}

// outcome judges the run that exited with exitCode.
func (r *reader) outcome(exitCode int) Outcome {
	o := Outcome{Denials: r.denials, ConversationID: r.conv, Summary: clip(r.summary, wire.ScheduleSummaryMax)}
	if r.harness == wire.HarnessCodex && !r.turnCompleted && r.codexError != "" {
		r.failed = true
		if r.errMsg == "" {
			r.errMsg = r.codexError
		}
	}
	switch {
	case !r.parsed:
		o.Unjudged = true
		o.Result = wire.RunSucceeded
		if exitCode != 0 {
			o.Result = wire.RunFailed
		}
	case exitCode != 0 || r.failed:
		o.Result, o.Error, o.AuthFailed = wire.RunFailed, r.errMsg, r.authFailed
	case r.denials > 0:
		o.Result = wire.RunDenied
	default:
		o.Result = wire.RunSucceeded
	}
	return o
}

func str(ev map[string]json.RawMessage, key string) string {
	var s string
	json.Unmarshal(ev[key], &s)
	return s
}

// clip cuts s to at most n bytes of whole runes.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// oneLine is s on one line, at most n runes, for the terminal.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// claude -p --output-format json: one result object at the end.

func (r *reader) claude(ev map[string]json.RawMessage, raw []byte) bool {
	if str(ev, "type") != "result" {
		return false
	}
	var res struct {
		Subtype           string `json:"subtype"`
		IsError           bool   `json:"is_error"`
		Result            string `json:"result"`
		SessionID         string `json:"session_id"`
		APIErrorStatus    int    `json:"api_error_status"`
		PermissionDenials []struct {
			ToolName  string          `json:"tool_name"`
			ToolInput json.RawMessage `json:"tool_input"`
		} `json:"permission_denials"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return false
	}
	r.conv, r.summary = res.SessionID, res.Result
	if res.Result != "" {
		r.say("%s", res.Result)
	}
	for _, d := range res.PermissionDenials {
		r.denials++
		r.say("[stagent] refused: %s %s", d.ToolName, oneLine(inputSummary(d.ToolInput), 200))
	}
	if res.IsError || res.Subtype != "" && res.Subtype != "success" {
		r.failed = true
		r.errMsg = firstNonEmpty(res.Result, res.Subtype)
		// "Not logged in · Please run /login", "Invalid API key · Please
		// run /login", a refused token.
		r.authFailed = res.IsError && (res.APIErrorStatus == 401 || strings.Contains(res.Result, "/login"))
	}
	return true
}

// inputSummary is the part of a tool's input a person recognizes it by.
func inputSummary(input json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return string(input)
	}
	for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
		if s := str(in, k); s != "" {
			return s
		}
	}
	return string(input)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// codex exec --json: JSON lines (thread.started, turn.*, item.*, error).

type codexItem struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	Message          string `json:"message"`
	Command          string `json:"command"`
	AggregatedOutput string `json:"aggregated_output"`
	ExitCode         *int   `json:"exit_code"`
	Status           string `json:"status"`
	Changes          []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	} `json:"changes"`
}

func (r *reader) codex(ev map[string]json.RawMessage) bool {
	switch str(ev, "type") {
	case "thread.started":
		r.conv = str(ev, "thread_id")
	case "turn.started":
	case "turn.completed":
		r.turnCompleted = true
	case "turn.failed":
		var e struct {
			Message string `json:"message"`
		}
		json.Unmarshal(ev["error"], &e)
		r.failed, r.errMsg = true, firstNonEmpty(e.Message, "turn failed")
		r.say("[stagent] turn failed: %s", e.Message)
	case "error":
		r.codexError = str(ev, "message")
		r.say("[stagent] %s", r.codexError)
	case "item.started":
		var it codexItem
		json.Unmarshal(ev["item"], &it)
		if it.Type == "command_execution" {
			r.say("$ %s", it.Command)
		}
	case "item.updated":
	case "item.completed":
		var it codexItem
		json.Unmarshal(ev["item"], &it)
		r.codexItem(it)
	default:
		return false
	}
	return true
}

func (r *reader) codexItem(it codexItem) {
	refused := it.Status == "declined" || it.Type == "file_change" && it.Status == "failed"
	if refused {
		r.denials++
	}
	switch it.Type {
	case "agent_message":
		r.summary = it.Text
		r.say("%s", it.Text)
	case "command_execution":
		if out := strings.TrimRight(it.AggregatedOutput, "\n"); out != "" {
			r.say("%s", clip(out, 4<<10))
		}
		switch {
		case refused:
			r.say("[stagent] refused: %s", it.Command)
		case it.ExitCode != nil && *it.ExitCode != 0:
			r.say("[stagent] exit %d: %s", *it.ExitCode, oneLine(it.Command, 200))
		}
	case "file_change":
		for _, c := range it.Changes {
			if refused {
				r.say("[stagent] refused: %s %s", c.Kind, c.Path)
			} else {
				r.say("[stagent] %s %s", c.Kind, c.Path)
			}
		}
	case "error":
		r.say("[stagent] %s", it.Message)
	default:
		if refused {
			r.say("[stagent] refused: %s", it.Type)
		}
	}
}

// ---------------------------------------------------------------------------
// omp -p --mode json: JSON lines (session, agent_*, turn_*, message_*,
// tool_execution_*).

func (r *reader) omp(ev map[string]json.RawMessage, raw []byte) bool {
	t := str(ev, "type")
	switch t {
	case "session":
		r.conv = str(ev, "id")
	case "tool_execution_start":
		what := firstNonEmpty(str(ev, "intent"), inputSummary(ev["args"]))
		r.say("> %s: %s", str(ev, "toolName"), oneLine(what, 200))
	case "tool_execution_end":
		var end struct {
			ToolName string `json:"toolName"`
			IsError  bool   `json:"isError"`
			Result   struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		json.Unmarshal(raw, &end)
		if !end.IsError {
			break
		}
		var text string
		for _, c := range end.Result.Content {
			text += c.Text
		}
		if strings.Contains(text, "requires approval") {
			r.denials++
			r.say("[stagent] refused: %s", end.ToolName)
		} else {
			r.say("[stagent] %s failed: %s", end.ToolName, oneLine(text, 200))
		}
	case "message_end":
		var m struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			StopReason   string `json:"stopReason"`
			ErrorMessage string `json:"errorMessage"`
		}
		json.Unmarshal(ev["message"], &m)
		if m.Role != "assistant" {
			break
		}
		var text []string
		for _, c := range m.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				text = append(text, c.Text)
			}
		}
		if len(text) > 0 {
			r.summary = strings.Join(text, "\n")
			r.say("%s", r.summary)
		}
		if m.StopReason == "error" || m.StopReason == "aborted" {
			r.failed, r.errMsg = true, firstNonEmpty(m.ErrorMessage, m.StopReason)
			r.say("[stagent] %s", r.errMsg)
		}
	default:
		if !strings.HasPrefix(t, "agent_") && !strings.HasPrefix(t, "turn_") && !strings.HasPrefix(t, "message_") &&
			!strings.HasPrefix(t, "tool_") && !strings.HasPrefix(t, "auto_") && !strings.HasSuffix(t, "_changed") {
			return false
		}
	}
	return true
}
