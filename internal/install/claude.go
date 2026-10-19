package install

import (
	"encoding/json"
	"os"

	"github.com/obutora/stagent/internal/wire"
)

// Claude Code: stagent's hooks in ~/.claude/settings.json.

var claudeEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "Notification", "PermissionRequest"}

// claudeApprovalTimeoutSec (7 days) is the timeout of claude's
// PermissionRequest hook: the daemon holds that hook until the prompt is
// answered, with no time limit of its own.
const claudeApprovalTimeoutSec = 7 * 24 * 60 * 60

// claudeHooks: Claude Code runs the hook command through Git Bash or cmd on
// Windows, where the quoted path works.
var claudeHooks = hookFile{
	harness:  wire.HarnessClaude,
	events:   claudeEvents,
	timeouts: map[string]int{"PermissionRequest": claudeApprovalTimeoutSec},
}

type claudeIntegration struct{}

func (claudeIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.claudeTarget())
	if c != nil {
		e.note(noteClaudeRestart, "Claude Code reads hooks when a session starts; restart running Claude sessions to activate them.")
		if e.goos == "windows" {
			e.note(noteClaudeWindowsHooks, "On Windows the hooks run stagent.exe directly (valid in Git Bash and cmd). If the binary is deleted without `stagent uninstall --level unhook`, Claude reports a non-blocking hook error until the entries are removed; `stagent doctor` lists them as orphans.")
		}
	}
	return []*Change{c}, err
}

func (claudeIntegration) targets(e *env) []*target { return []*target{e.claudeTarget()} }

// report adds the user's Remote Control setting to the claude item. Only
// ~/.claude/settings.json is read: managed and project settings are not
// guessed, and stagent never changes the setting.
func (claudeIntegration) report(e *env) HarnessReport {
	t := e.claudeTarget()
	h, cur := e.harnessReport(wire.HarnessClaude, t)
	if cur != nil {
		if _, err := parseJSONDoc(cur); err != nil {
			h.Notes = append(h.Notes, t.path+" is not valid JSON")
		}
	}
	h.RemoteControlAtStartup = json.RawMessage("null")
	var s struct {
		RemoteControlAtStartup json.RawMessage `json:"remoteControlAtStartup"`
	}
	if b, err := os.ReadFile(e.claudeSettings()); err == nil && json.Unmarshal(b, &s) == nil {
		if v := string(s.RemoteControlAtStartup); v == "true" || v == "false" {
			h.RemoteControlAtStartup = json.RawMessage(v)
		}
	}
	return h
}

func (e *env) claudeSettings() string { return e.home(".claude", "settings.json") }

func (e *env) claudeTarget() *target {
	return e.hooksTarget(wire.HarnessClaude, e.claudeSettings(), claudeHooks)
}
