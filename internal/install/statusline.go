package install

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// Claude Code's statusLine relay (`integrate --statusline`): the statusLine
// of Claude's settings.json becomes `stagent statusline claude`, which
// sends the limit windows of its input to the daemon and runs the user's
// own statusLine, recorded in the manifest (ConfigEntry.Original). Every
// other key of the user's statusLine (padding, refreshInterval…) stays.

const statusLineID = "statusline"

// Values of StatusLineFile.State.
const (
	statusLineRelay    = "relay"    // our relay is the statusLine
	statusLineReplaced = "replaced" // the manifest records our relay, but the statusLine is another now
	statusLineOther    = "other"    // a statusLine of the user's, never relayed
	statusLineNone     = "none"     // no statusLine
)

// StatusLineReport is doctor's view of the statusLine relay.
type StatusLineReport struct {
	// Installed: some settings.json has our relay as its statusLine.
	Installed bool `json:"installed"`
	// Replaced: one we relayed holds another statusLine now, so the limit
	// windows are not measured there until `integrate --statusline`
	// relays the new one.
	Replaced bool `json:"replaced"`
	// UserStatusLine: the user has a statusLine of their own, the one the
	// relay runs or the one `integrate --statusline` would keep.
	UserStatusLine bool             `json:"user_status_line"`
	Files          []StatusLineFile `json:"files"`
}

type StatusLineFile struct {
	Path  string `json:"path"`
	State string `json:"state"` // statusLine* values
}

// claudeConfigDirs are the Claude configuration directories whose
// settings.json gets the relay: ~/.claude, and $CLAUDE_CONFIG_DIR when set.
func (e *env) claudeConfigDirs() []string {
	dirs := []string{e.home(".claude")}
	if d := e.getenv("CLAUDE_CONFIG_DIR"); d != "" && filepath.Clean(d) != dirs[0] {
		dirs = append(dirs, filepath.Clean(d))
	}
	return dirs
}

// statusLineTargets are the settings.json files of claudeConfigDirs, then
// any other the manifest records a relay in (integrated under another
// CLAUDE_CONFIG_DIR), for removal and doctor.
func (e *env) statusLineTargets() []*target {
	var ts []*target
	for _, d := range e.claudeConfigDirs() {
		ts = append(ts, e.statusLineTarget(filepath.Join(d, "settings.json")))
	}
	for _, rec := range e.m.Configs {
		if rec.ID == statusLineID && !e.hasTarget(ts, rec.Path) {
			ts = append(ts, e.statusLineTarget(rec.Path))
		}
	}
	return ts
}

func (e *env) hasTarget(ts []*target, path string) bool {
	for _, t := range ts {
		if t.path == path {
			return true
		}
	}
	return false
}

// statusLineAddChanges plans the relay in every settings.json of
// claudeConfigDirs.
func (e *env) statusLineAddChanges(one func(id string) func(*Change, error)) {
	for _, d := range e.claudeConfigDirs() {
		one(statusLineID)(e.addChange(e.statusLineTarget(filepath.Join(d, "settings.json"))))
	}
}

// statusLineCommand is the statusLine command of the relay. On unix a
// deleted binary shows nothing, quietly. On Windows Claude Code runs it
// through Git Bash, or PowerShell without Git Bash: the path (forward
// slashes) is bare, valid in both, unless it holds a character one of
// them treats specially; then it is quoted, for Git Bash.
func (e *env) statusLineCommand() string {
	if e.goos == "windows" {
		bin := strings.ReplaceAll(e.l.Bin, `\`, "/")
		if strings.ContainsAny(bin, " \t'\"`$&|;,(){}[]@#<>%^") {
			return `"` + bin + `" statusline claude`
		}
		return bin + " statusline claude"
	}
	q := shellQuote(e.l.Bin)
	return "[ ! -x " + q + " ] || exec " + q + " statusline claude"
}

// isOurStatusLine reports whether n (settings.json's statusLine) is the
// relay.
func isOurStatusLine(n *jnode) bool {
	cmd, _ := n.get("command").str()
	return isOurCommand(cmd) && strings.Contains(cmd, " statusline claude")
}

func (e *env) statusLineTarget(path string) *target {
	return &target{
		id: statusLineID, path: path, identify: "statusLine running `stagent statusline claude`", mode: 0o600,
		add: func(cur []byte) (addResult, error) {
			d, err := parseJSONDoc(cur)
			if err != nil || d.root.kind != jObject {
				return addResult{}, unmanagedFile(path + " is not a valid JSON object; left unchanged")
			}
			cmd := e.statusLineCommand()
			sl := d.root.get("statusLine")
			if isOurStatusLine(sl) {
				if c, _ := sl.get("command").str(); c == cmd {
					return addResult{after: cur}, nil
				}
				sl.set("command", jStr(cmd))
				return addResult{after: d.encode(), summary: "point the statusLine relay at " + e.l.Bin}, nil
			}
			res := addResult{replaced: true, summary: "show Claude's statusLine through `stagent statusline claude`, which measures the limit windows"}
			relay := jObj("type", jStr("command"), "command", jStr(cmd))
			if sl != nil {
				res.original = string(sl.src)
				if c := originalCommand(res.original); c != "" {
					res.summary += " and runs the current one (" + c + ")"
				}
				if sl.kind == jObject {
					sl.set("type", jStr("command"))
					sl.set("command", jStr(cmd))
					relay = sl
				}
			}
			d.root.set("statusLine", relay)
			res.after = d.encode()
			return res, nil
		},
		remove: func(cur []byte, rec *ConfigEntry) ([]byte, error) {
			d, err := parseJSONDoc(cur)
			if err != nil {
				return nil, unmanagedFile(path + " is not valid JSON; left unchanged")
			}
			if !isOurStatusLine(d.root.get("statusLine")) {
				return cur, nil
			}
			if rec != nil && len(rec.Original) > 0 {
				d.root.set("statusLine", &jnode{kind: jScalar, raw: []byte(rec.Original)})
			} else {
				d.root.del("statusLine")
			}
			if rec != nil && rec.Created && len(d.root.keys) == 0 {
				return nil, nil
			}
			return d.encode(), nil
		},
		present: func(cur []byte) string {
			d, err := parseJSONDoc(cur)
			if err != nil || !isOurStatusLine(d.root.get("statusLine")) {
				return ""
			}
			return "statusLine relay (stagent statusline claude)"
		},
	}
}

// originalCommand is the command of a recorded statusLine (raw JSON); ""
// when it has none, or it is the relay itself (never run the relay from
// the relay).
func originalCommand(raw string) string {
	var sl struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(raw), &sl) != nil || sl.Type != "command" || isOurCommand(sl.Command) {
		return ""
	}
	return sl.Command
}

// StatusLineCommand is the command of the user's statusLine that the relay
// in the Claude settings.json at settings took the place of, read from the
// manifest at manifest; "" when there is none.
func StatusLineCommand(manifest, settings string) string {
	m, ok, err := loadManifest(manifest)
	if err != nil || !ok {
		return ""
	}
	if rec := m.config(statusLineID, filepath.Clean(settings)); rec != nil {
		return originalCommand(rec.Original)
	}
	return ""
}

// StatusLineRelayed reports whether the manifest at manifest records the
// relay in the settings.json of Claude configuration directory dir (a
// clean path): the directories a relay can report from.
func StatusLineRelayed(manifest, dir string) bool {
	m, ok, err := loadManifest(manifest)
	return err == nil && ok && m.config(statusLineID, filepath.Join(dir, "settings.json")) != nil
}

// statusLineReport is doctor's StatusLineReport; a statusLine the user put
// in place of the relay is a problem.
func (e *env) statusLineReport(r *DoctorReport) {
	s := StatusLineReport{Files: []StatusLineFile{}}
	for _, t := range e.statusLineTargets() {
		rec := e.m.config(statusLineID, t.path)
		var sl *jnode
		if cur, _ := readOptional(t.path); cur != nil {
			if d, err := parseJSONDoc(cur); err == nil {
				sl = d.root.get("statusLine")
			}
		}
		state := statusLineNone
		switch {
		case isOurStatusLine(sl):
			state, s.Installed = statusLineRelay, true
			s.UserStatusLine = s.UserStatusLine || rec != nil && originalCommand(rec.Original) != ""
		case rec != nil:
			state, s.Replaced = statusLineReplaced, true
			s.UserStatusLine = s.UserStatusLine || sl != nil
			r.Problems = append(r.Problems, "the statusLine in "+t.path+" was replaced, so the share used of Claude's limit windows (上限率) cannot be measured there; `stagent integrate --apply --statusline` relays the new one")
		case sl != nil:
			state, s.UserStatusLine = statusLineOther, true
		}
		s.Files = append(s.Files, StatusLineFile{Path: t.path, State: state})
	}
	r.StatusLine = s
}
