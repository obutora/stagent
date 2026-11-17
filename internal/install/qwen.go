package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Qwen Code: an extension of stagent's in its user extensions directory
// ($QWEN_HOME/extensions, else ~/.qwen/extensions), stagent/
// qwen-extension.json, whose manifest holds nothing but hooks. Qwen Code
// loads the hooks of every enabled extension along with those of its
// settings, also in a folder it does not trust, so the user's settings.json
// (JSON with comments, which stagent does not edit) is never touched:
// installing writes the directory, removing deletes it.
//
// The hooks have Claude Code's form ({"<Event>": [{"hooks": [{"type":
// "command", "command": …, "timeout": <seconds>}]}]}) and payload. Each
// passes its event as an argument, `stagent hook qwen <event>`. Qwen Code
// waits for a hook unless it is marked async; ours are not, as the order of
// the events matters and the hook returns at once. On Windows Qwen Code
// runs a command in cmd unless the hook names its shell: ours names
// powershell. PreToolUse and PostToolUse run for every tool: the daemon
// tells the idle_prompt Qwen Code sends before each tool call by the
// PreToolUse that comes with it.

var qwenEvents = []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "PermissionRequest", "PreToolUse", "PostToolUse"}

// qwenExtensionVersion is the manifest's version, which Qwen Code requires;
// it changes with what the extension does, not with stagent's version.
const qwenExtensionVersion = "1.0.0"

type qwenIntegration struct{}

func (qwenIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.qwenTarget())
	if c != nil && e.findHarness(wire.HarnessQwen) != "" {
		e.note(noteQwenRestart, "Qwen Code reads hooks when a session starts; restart running qwen sessions to activate them.")
	}
	return []*Change{c}, err
}

// targets include the extension where it was written under an earlier
// QWEN_HOME.
func (qwenIntegration) targets(e *env) []*target {
	return e.withRecorded(e.qwenTarget(), e.qwenTargetAt)
}

func (qwenIntegration) report(e *env) HarnessReport {
	h, _ := e.harnessReport(wire.HarnessQwen, e.qwenTarget())
	if qwenHooksDisabled(filepath.Join(harness.QwenHome(e.l.Home, e.getenv), "settings.json")) {
		h.Notes = append(h.Notes, "settings.json sets disableAllHooks: Qwen Code runs no hooks, stagent's included")
	}
	return h
}

func (e *env) qwenTarget() *target {
	return e.qwenTargetAt(filepath.Join(harness.QwenHome(e.l.Home, e.getenv), "extensions", "stagent", "qwen-extension.json"))
}

func (e *env) qwenTargetAt(path string) *target {
	source := e.qwenManifestSource()
	return &target{
		id: wire.HarnessQwen, path: path, owned: true, mode: 0o644,
		identify: "whole file (every hook command contains \"" + binMarker + "\")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !isOurNestedHooks(cur) {
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(source), summary: "install the qwen extension that runs `stagent hook qwen` for " + strings.Join(qwenEvents, ", ")}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !isOurNestedHooks(cur) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if isOurNestedHooks(cur) {
				return "stagent qwen extension (hooks for " + strings.Join(qwenEvents, ", ") + ")"
			}
			return ""
		},
	}
}

// qwenManifestSource is the extension's manifest: one command hook per
// event, in qwenEvents' order.
func (e *env) qwenManifestSource() string {
	var b strings.Builder
	b.WriteString("{\n  \"name\": \"stagent\",\n  \"version\": \"" + qwenExtensionVersion + "\",\n")
	b.WriteString("  \"description\": \"SSH Term: tells stagent what this Qwen Code session is doing (installed by stagent integrate; managed file)\",\n")
	b.WriteString("  \"hooks\": {\n")
	for i, ev := range qwenEvents {
		cmd, _ := json.Marshal(e.qwenHookCommand(ev))
		b.WriteString("    \"" + ev + "\": [\n      {\n        \"hooks\": [\n          {\n")
		b.WriteString("            \"type\": \"command\",\n")
		b.WriteString("            \"command\": " + string(cmd) + ",\n")
		if e.goos == "windows" {
			b.WriteString("            \"shell\": \"powershell\",\n")
		}
		b.WriteString("            \"timeout\": " + strconv.Itoa(hookTimeoutSec) + "\n")
		b.WriteString("          }\n        ]\n      }\n    ]")
		if i < len(qwenEvents)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("  }\n}\n")
	return b.String()
}

// qwenHookCommand is the command of event's hook: the guarded command of
// the other harnesses (bash), or the PowerShell one on Windows.
func (e *env) qwenHookCommand(event string) string {
	if e.goos == "windows" {
		return e.psHookCommand(wire.HarnessQwen, event)
	}
	return e.guardedHookCommand(wire.HarnessQwen, event)
}

// qwenDisableAllHooks finds "disableAllHooks": true in Qwen Code's
// settings, JSON with comments.
var qwenDisableAllHooks = regexp.MustCompile(`"disableAllHooks"\s*:\s*true\b`)

// qwenHooksDisabled reports whether the settings file at path turns every
// hook off.
func qwenHooksDisabled(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && qwenDisableAllHooks.Match(b)
}
