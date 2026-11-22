package install

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/obutora/stagent/internal/harness"
	"github.com/obutora/stagent/internal/wire"
)

// Antigravity CLI: a group of stagent's, "ssh-term-stagent", in the hooks
// file it shares with the Antigravity app, ~/.gemini/config/hooks.json (no
// environment variable moves it). The file is an object of named groups,
// each mapping events to handlers ({"<group>": {"enabled"?: bool,
// "<Event>": [{"type": "command", "command": …, "timeout": <seconds>}]}};
// PreToolUse and PostToolUse wrap them in matcher groups, [{"matcher": …,
// "hooks": [handler…]}]). Other groups (Orca's "orca-status", the user's
// own) are left as they are: installing sets our group, removing deletes
// it, and a file stagent created and left empty is deleted.
//
// Each hook passes its event as an argument, `stagent hook antigravity
// <event>`, with Antigravity CLI's own event names. Antigravity CLI waits
// for its hooks (30 s by default) and reads a JSON object on their stdout;
// ours answer {} at once, which decides nothing. PreToolUse runs for every
// tool: the daemon watches the screen for the permission menu the call may
// bring (Antigravity CLI sends no hook for it). PostToolUse runs for its
// question tool only (ask_question), whose answer ends the question.
// PreInvocation runs before every model call: the first of a turn is the
// prompt submitted. PostInvocation is not used. A running Antigravity CLI
// reads the file again when it changes: there is nothing to restart. On
// unix the command is the guarded one of the other harnesses; on Windows
// Antigravity CLI runs a command with cmd /c, so the hook names the binary
// by its path with backslashes (cmd takes / for a switch), quoted only
// when cmd would split it.

var antigravityEvents = []string{"SessionStart", "PreInvocation", "PreToolUse", "PostToolUse", "Stop"}

// antigravityGroup is the name of stagent's group in the hooks file.
const antigravityGroup = "ssh-term-stagent"

// antigravityMatchers are the tool matchers of the events whose handlers
// Antigravity CLI groups by tool.
var antigravityMatchers = map[string]string{
	"PreToolUse":  "*",
	"PostToolUse": harness.QuestionToolOf(wire.HarnessAntigravity),
}

type antigravityIntegration struct{}

func (antigravityIntegration) plan(e *env) ([]*Change, error) {
	c, err := e.addChange(e.antigravityTarget())
	return []*Change{c}, err
}

func (antigravityIntegration) targets(e *env) []*target {
	return []*target{e.antigravityTarget()}
}

func (antigravityIntegration) report(e *env) HarnessReport {
	t := e.antigravityTarget()
	h, cur := e.harnessReport(wire.HarnessAntigravity, t)
	if cur != nil {
		d, err := parseJSONDoc(cur)
		if err != nil {
			h.Notes = append(h.Notes, t.path+" is not valid JSON")
		} else if on := d.root.get(antigravityGroup).get("enabled"); h.Integrated && on != nil && string(on.raw) == "false" {
			h.Notes = append(h.Notes, `the group "`+antigravityGroup+`" is disabled ("enabled": false): Antigravity CLI runs none of its hooks`)
		}
	}
	return h
}

func (e *env) antigravityHooks() string { return e.home(".gemini", "config", "hooks.json") }

func (e *env) antigravityTarget() *target {
	path := e.antigravityHooks()
	summary := "add the group \"" + antigravityGroup + "\" running `stagent hook antigravity` for " + strings.Join(antigravityEvents, ", ")
	return &target{
		id: wire.HarnessAntigravity, path: path, mode: 0o600,
		identify: `the top-level group "` + antigravityGroup + `" (every hook command contains "` + binMarker + `")`,
		add: func(cur []byte) (addResult, error) {
			d, err := parseJSONDoc(cur)
			if err != nil || d.root.kind != jObject {
				return addResult{}, unmanagedFile(path + " is not a JSON object; left unchanged")
			}
			want := e.antigravityGroupNode()
			if old := d.root.get(antigravityGroup); old != nil {
				if !isOurAntigravityGroup(old.src) {
					return addResult{}, unmanagedFile(path + `: its group "` + antigravityGroup + `" is not managed by stagent; left unchanged`)
				}
				if sameJSON(old.src, encodeNode(want)) {
					return addResult{after: cur}, nil
				}
			}
			d.root.set(antigravityGroup, want)
			return addResult{after: d.encode(), summary: summary}, nil
		},
		remove: func(cur []byte, rec *ConfigEntry) ([]byte, error) {
			d, err := parseJSONDoc(cur)
			if err != nil || d.root.kind != jObject {
				return nil, unmanagedFile(path + " is not a JSON object; left unchanged")
			}
			if g := d.root.get(antigravityGroup); g == nil || !isOurAntigravityGroup(g.src) {
				return cur, nil
			}
			d.root.del(antigravityGroup)
			if rec != nil && rec.Created && len(d.root.keys) == 0 {
				return nil, nil
			}
			return d.encode(), nil
		},
		present: func(cur []byte) string {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return ""
			}
			if g := d.root.get(antigravityGroup); g != nil && isOurAntigravityGroup(g.src) {
				return `stagent's group "` + antigravityGroup + `" (hooks for ` + strings.Join(antigravityEvents, ", ") + ")"
			}
			return ""
		},
	}
}

// antigravityGroupNode is stagent's group: one handler per event, in
// antigravityEvents' order, in a matcher group for the events Antigravity
// CLI groups by tool.
func (e *env) antigravityGroupNode() *jnode {
	g := jObj()
	for _, ev := range antigravityEvents {
		h := ourHookEntry(e.antigravityHookCommand(ev), hookTimeoutSec)
		if m, ok := antigravityMatchers[ev]; ok {
			g.set(ev, jArr(jObj("matcher", jStr(m), "hooks", jArr(h))))
		} else {
			g.set(ev, jArr(h))
		}
	}
	return g
}

// antigravityHookCommand is the command of event's hook: the guarded
// command of the other harnesses, or on Windows the binary's path for cmd.
func (e *env) antigravityHookCommand(event string) string {
	if e.goos != "windows" {
		return e.guardedHookCommand(wire.HarnessAntigravity, event)
	}
	bin := strings.ReplaceAll(e.l.Bin, "/", `\`)
	if strings.ContainsAny(bin, " \t&|<>^()%!,;=\"") {
		bin = `"` + bin + `"`
	}
	return bin + " hook " + wire.HarnessAntigravity + " " + event
}

// isOurAntigravityGroup reports whether src, the source of a group of the
// hooks file, is stagent's: an object whose every hook command, flat or in
// a matcher group, runs stagent's binary.
func isOurAntigravityGroup(src []byte) bool {
	var g map[string]json.RawMessage
	if json.Unmarshal(src, &g) != nil {
		return false
	}
	n := 0
	for key, raw := range g {
		if key == "enabled" {
			continue
		}
		var handlers []struct {
			Command *string `json:"command"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		}
		if json.Unmarshal(raw, &handlers) != nil {
			return false
		}
		for _, h := range handlers {
			if h.Command != nil {
				if !isOurCommand(*h.Command) {
					return false
				}
				n++
			}
			for _, hh := range h.Hooks {
				if !isOurCommand(hh.Command) {
					return false
				}
				n++
			}
		}
	}
	return n > 0
}

// encodeNode is n's JSON text.
func encodeNode(n *jnode) []byte {
	var buf bytes.Buffer
	(&jdoc{indent: "  ", eol: "\n"}).write(&buf, n, 0)
	return buf.Bytes()
}

// sameJSON reports whether a and b are the same JSON value, whatever their
// formatting.
func sameJSON(a, b []byte) bool {
	var va, vb any
	return json.Unmarshal(a, &va) == nil && json.Unmarshal(b, &vb) == nil && reflect.DeepEqual(va, vb)
}
