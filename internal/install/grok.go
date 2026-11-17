package install

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Grok Build: a hooks file of stagent's in its user hooks directory
// ($GROK_HOME/hooks, else ~/.grok/hooks), which Grok Build reads with every
// *.json there when it starts (or on /hooks → r). No file of the user's is
// edited (config.toml included): installing writes the file, removing
// deletes it.
//
// The file has Claude Code's form ({"hooks": {"<Event>": [{"matcher"?,
// "hooks": [{"type": "command", "command": …, "timeout": <seconds>}]}]}})
// and so do the payloads, with Grok Build's camelCase fields beside them.
// Each hook passes its event as an argument, `stagent hook grok <event>`.
// Grok Build runs its hooks one after another and waits for each (5 s by
// default, 600 s for Stop and PostToolUse): ours return at once, each with
// the same timeout. PreToolUse runs for every tool: Grok Build has no
// PermissionRequest, and the PreToolUse before its permission_prompt
// notification names the tool asked about. PostToolUse runs for its
// question tool only. StopCancelled is Grok Build's own: a turn the user
// cut short. On unix the command is the guarded one of the other
// harnesses; on Windows Grok Build runs a command under the shell it finds
// (pwsh, Windows PowerShell or Git Bash), so the hook names the binary with
// its arguments (args), which runs with no shell in any of them.
//
// Grok Build also runs the hooks of Claude Code's settings
// (~/.claude/settings.json); stagent's claude hook drops those calls
// (hook/grok.go). With a hook set up, Grok Build asks whether to trust a
// folder before the first session there (outside $HOME), and runs no hook
// until it is answered.

var grokEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Notification", "Stop", "StopCancelled"}

type grokIntegration struct{}

func (grokIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.grokTarget())
	if c != nil && e.findHarness(wire.HarnessGrok) != "" {
		e.note(noteGrokRestart, "Grok Build reads hooks when it starts; restart running grok sessions (or reload them in /hooks with r) to activate them.")
	}
	return []*Change{c}, err
}

// targets include the hooks file where it was written under an earlier
// GROK_HOME.
func (grokIntegration) targets(e *env) []*target {
	return e.withRecorded(e.grokTarget(), e.grokTargetAt)
}

func (grokIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessGrok, e.grokTarget())
	return h
}

func (e *env) grokHooks() string {
	return filepath.Join(harness.GrokHome(e.l.Home, e.getenv), "hooks", "ssh-term-stagent.json")
}

func (e *env) grokTarget() *target { return e.grokTargetAt(e.grokHooks()) }

func (e *env) grokTargetAt(path string) *target {
	source := e.grokHooksSource()
	return &target{
		id: wire.HarnessGrok, path: path, owned: true, mode: 0o600,
		identify: "whole file (every hook command contains \"" + binMarker + "\")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !isOurNestedHooks(cur) {
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(source), summary: "add `stagent hook grok` for " + strings.Join(grokEvents, ", ")}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !isOurNestedHooks(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if isOurNestedHooks(cur) {
				return "stagent hook entries for " + strings.Join(grokEvents, ", ")
			}
			return ""
		},
	}
}

// grokHooksSource is the hooks file: one command hook per event, in
// grokEvents' order.
func (e *env) grokHooksSource() string {
	var b strings.Builder
	b.WriteString("{\n  \"hooks\": {\n")
	for i, ev := range grokEvents {
		b.WriteString("    \"" + ev + "\": [\n      {\n")
		if ev == "PostToolUse" {
			b.WriteString("        \"matcher\": \"" + harness.QuestionToolOf(wire.HarnessGrok) + "\",\n")
		}
		b.WriteString("        \"hooks\": [\n          {\n")
		b.WriteString("            \"type\": \"command\",\n")
		if e.goos == "windows" {
			bin, _ := json.Marshal(e.l.Bin)
			args, _ := json.Marshal([]string{"hook", wire.HarnessGrok, ev})
			b.WriteString("            \"command\": " + string(bin) + ",\n")
			b.WriteString("            \"args\": " + string(args) + ",\n")
		} else {
			cmd, _ := json.Marshal(e.guardedHookCommand(wire.HarnessGrok, ev))
			b.WriteString("            \"command\": " + string(cmd) + ",\n")
		}
		b.WriteString("            \"timeout\": " + strconv.Itoa(hookTimeoutSec) + "\n")
		b.WriteString("          }\n        ]\n      }\n    ]")
		if i < len(grokEvents)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}
