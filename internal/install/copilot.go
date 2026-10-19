package install

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Copilot CLI: a hooks file of stagent's in its user-level hooks directory
// ($COPILOT_HOME/hooks, else ~/.copilot/hooks), which Copilot CLI reads
// with every *.json there.
//
// The events are registered by their PascalCase names. Copilot CLI then
// sends the VS Code compatible payload: snake_case fields (session_id,
// transcript_path, hook_event_name, tool_name, tool_input with Claude Code's
// tool names) as Claude Code does, which the daemon already reads. The
// camelCase names (sessionStart, agentStop, …) send sessionId,
// transcriptPath and a millisecond timestamp instead. Both forms are
// documented in the hooks reference; the runtime of Copilot CLI 1.0.98
// lists the PascalCase aliases, Notification and PermissionRequest
// included. `stagent hook copilot` still reads the camelCase fields
// (hook/copilot.go), as the reference shows the notification payload with
// sessionId.
//
// Each hook has a bash command (macOS, Linux) and a powershell one
// (Windows), both skipping a missing binary, and passes the event name as
// an argument. Copilot CLI runs the PermissionRequest hook before its own
// permission checks and shows its menu only once the hook returned: the
// hook answers nothing and returns at once (the daemon learns of the menu
// from the permission_prompt notification and the holder's screen).

var copilotEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "Notification", "PermissionRequest"}

type copilotIntegration struct{}

func (copilotIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.copilotTarget())
	if c != nil && e.findHarness(wire.HarnessCopilot) != "" {
		e.note(noteCopilotRestart, "Copilot CLI reads hooks when a session starts; restart running copilot sessions to activate them.")
	}
	return []*Change{c}, err
}

// targets include the hooks file where it was written under an earlier
// COPILOT_HOME.
func (copilotIntegration) targets(e *env) []*target {
	return e.withRecorded(e.copilotTarget(), e.copilotTargetAt)
}

func (copilotIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessCopilot, e.copilotTarget())
	return h
}

func (e *env) copilotHooks() string {
	return filepath.Join(harness.CopilotHome(e.l.Home, e.getenv), "hooks", "ssh-term-stagent.json")
}

func (e *env) copilotTarget() *target { return e.copilotTargetAt(e.copilotHooks()) }

func (e *env) copilotTargetAt(path string) *target {
	source := e.copilotHooksSource()
	return &target{
		id: wire.HarnessCopilot, path: path, owned: true, mode: 0o600,
		identify: "whole file (every hook command contains \"" + binMarker + "\")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !isOurCopilotHooks(cur) {
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(source), summary: "add `stagent hook copilot` for " + strings.Join(copilotEvents, ", ")}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !isOurCopilotHooks(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if isOurCopilotHooks(cur) {
				return "stagent hook entries for " + strings.Join(copilotEvents, ", ")
			}
			return ""
		},
	}
}

// copilotHooksSource is the hooks file: version 1, one command hook per
// event, in copilotEvents' order.
func (e *env) copilotHooksSource() string {
	var b strings.Builder
	b.WriteString("{\n  \"version\": 1,\n  \"hooks\": {\n")
	for i, ev := range copilotEvents {
		bash, _ := json.Marshal(e.copilotBash(ev))
		ps, _ := json.Marshal(e.copilotPowerShell(ev))
		b.WriteString("    \"" + ev + "\": [\n      {\n")
		b.WriteString("        \"type\": \"command\",\n")
		b.WriteString("        \"bash\": " + string(bash) + ",\n")
		b.WriteString("        \"powershell\": " + string(ps) + ",\n")
		b.WriteString("        \"timeoutSec\": " + strconv.Itoa(hookTimeoutSec) + "\n")
		b.WriteString("      }\n    ]")
		if i < len(copilotEvents)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// copilotBash is the bash command of event's hook: the guarded command of
// the other harnesses, given the event.
func (e *env) copilotBash(event string) string {
	q := shellQuote(e.l.Bin)
	return "[ -x " + q + " ] && " + q + " hook " + wire.HarnessCopilot + " " + event + " || true"
}

// copilotPowerShell is the powershell command of event's hook.
func (e *env) copilotPowerShell(event string) string {
	q := psQuote(e.l.Bin)
	return "if (Test-Path -LiteralPath " + q + ") { & " + q + " hook " + wire.HarnessCopilot + " " + event + " }"
}

// isOurCopilotHooks reports whether b is a hooks file of stagent's: valid
// JSON whose hook commands all run stagent's binary.
func isOurCopilotHooks(b []byte) bool {
	var f struct {
		Hooks map[string][]struct {
			Bash       string `json:"bash"`
			PowerShell string `json:"powershell"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &f) != nil || len(f.Hooks) == 0 {
		return false
	}
	for _, entries := range f.Hooks {
		for _, h := range entries {
			if !isOurCommand(h.Bash) || !isOurCommand(h.PowerShell) {
				return false
			}
		}
	}
	return true
}
