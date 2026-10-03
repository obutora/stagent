package install

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Terminal.app asks before closing a window, and so holds up a logout,
// while a process other than the login shell runs in it unless the
// process is in the window profile's noWarnProcesses. `integrate
// --terminal` (macOS) adds {ProcessName = stagent;} there for every
// profile, so a window running an agent through the shell wrapper closes
// at once and the agent is handed off. Terminal counts only login, the
// shell and stagent as the window's processes (the agent runs on
// stagent's PTY), so stagent is enough. The preferences are read and
// written through cfprefsd (`defaults export` / `defaults import`), never
// the plist file. A running Terminal keeps the profiles it loaded at
// launch: the change applies once Terminal is opened again (at the latest
// with the next login), and quitting it does not write the old list back.
// The manifest records each profile stagent added to; `--remove terminal`
// and `uninstall --level unhook` take only that entry out again.

const (
	terminalDomain  = "com.apple.Terminal"
	terminalProcess = "stagent"
	// terminalTarget is Change.Target of the Terminal change.
	terminalTarget = terminalDomain
)

// terminalDefaultNoWarn is Terminal's list for a profile without the
// noWarnProcesses key; writing the key replaces it, so it is written along.
var terminalDefaultNoWarn = []string{"screen", "tmux"}

// TerminalReport is doctor's view of Terminal.app (macOS).
type TerminalReport struct {
	// Profiles is the number of Terminal profiles (0 when Terminal has
	// none saved or they cannot be read).
	Profiles int `json:"profiles"`
	// NoWarn: every profile has stagent in noWarnProcesses.
	NoWarn bool `json:"no_warn"`
	// AddedByStagent: the manifest records profiles `integrate --terminal`
	// added stagent to.
	AddedByStagent bool `json:"added_by_stagent"`
}

// TerminalEntry records one profile `integrate --terminal` added stagent
// to.
type TerminalEntry struct {
	Profile string `json:"profile"`
	// CreatedKey: the profile had no noWarnProcesses, so Terminal's default
	// list was written along.
	CreatedKey bool `json:"created_key,omitempty"`
}

// terminalPrefs reads Terminal's preferences; nil profiles when it has
// none saved.
func (e *env) terminalPrefs() (root, profiles *pnode, err error) {
	out, err := e.run.Run(cmdTimeout, "defaults", "export", terminalDomain, "-")
	if err != nil {
		if s := strings.TrimSpace(out); s != "" {
			err = errors.New(s)
		}
		return nil, nil, errors.New("defaults export " + terminalDomain + ": " + err.Error())
	}
	root, err = parsePlist([]byte(out))
	if err != nil {
		return nil, nil, err
	}
	if p := root.get("Window Settings"); p != nil && p.kind == "dict" && len(p.vals) > 0 {
		profiles = p
	}
	return root, profiles, nil
}

// writeTerminalPrefs replaces Terminal's preferences with root.
func (e *env) writeTerminalPrefs(root *pnode) error {
	if err := os.MkdirAll(e.l.LogDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(e.l.LogDir, "terminal-prefs.plist")
	if err := os.WriteFile(tmp, root.encode(), 0o600); err != nil {
		return err
	}
	defer os.Remove(tmp)
	return e.runCmd("defaults", "import", terminalDomain, tmp)
}

// noWarnNames lists the process names of a profile's noWarnProcesses; ok
// is false when the profile has no such key.
func noWarnNames(profile *pnode) (names []string, ok bool) {
	list := profile.get("noWarnProcesses")
	if list == nil || list.kind != "array" {
		return nil, false
	}
	for _, v := range list.vals {
		switch v.kind {
		case "dict":
			if n := v.get("ProcessName"); n != nil {
				names = append(names, n.text)
			}
		case "string":
			names = append(names, v.text)
		}
	}
	return names, true
}

func noWarnDescription(profile *pnode) string {
	names, ok := noWarnNames(profile)
	if !ok {
		return "(not set: " + strings.Join(terminalDefaultNoWarn, ", ") + ")"
	}
	return strings.Join(names, ", ")
}

func noWarnEntry(name string) *pnode {
	return &pnode{kind: "dict", keys: []string{"ProcessName"}, vals: []*pnode{plistString(name)}}
}

// addNoWarn puts stagent into the profile's list; created is true when the
// key had to be created (with Terminal's default list).
func addNoWarn(profile *pnode) (created bool) {
	list := profile.get("noWarnProcesses")
	if list == nil || list.kind != "array" {
		list = &pnode{kind: "array"}
		for _, n := range terminalDefaultNoWarn {
			list.vals = append(list.vals, noWarnEntry(n))
		}
		profile.set("noWarnProcesses", list)
		created = true
	}
	list.vals = append(list.vals, noWarnEntry(terminalProcess))
	return created
}

// removeNoWarn takes stagent out of the profile's list; a key stagent
// created that holds exactly Terminal's default list then is removed, which
// restores the default.
func removeNoWarn(profile *pnode, createdKey bool) {
	list := profile.get("noWarnProcesses")
	if list == nil || list.kind != "array" {
		return
	}
	list.vals = slices.DeleteFunc(list.vals, func(v *pnode) bool {
		if v.kind == "string" {
			return v.text == terminalProcess
		}
		n := v.get("ProcessName")
		return n != nil && n.text == terminalProcess
	})
	if names, _ := noWarnNames(profile); createdKey && len(names) == len(list.vals) && slices.Equal(names, terminalDefaultNoWarn) {
		profile.del("noWarnProcesses")
	}
}

func hasNoWarn(profile *pnode) bool {
	names, _ := noWarnNames(profile)
	return slices.Contains(names, terminalProcess)
}

// terminalDiff shows each touched profile's list before and after.
func terminalDiff(before, after []string) string {
	j := func(s []string) []byte { return []byte(strings.Join(s, "\n") + "\n") }
	return unifiedDiff(terminalDomain+" noWarnProcesses", j(before), j(after))
}

// terminalAddChange plans adding stagent to every profile that lacks it;
// nil when they all have it or Terminal has no profiles saved.
func (e *env) terminalAddChange() (*Change, error) {
	if e.goos != "darwin" {
		return nil, errors.New("Terminal.app's close confirmation is a macOS setting")
	}
	_, profiles, err := e.terminalPrefs()
	if err != nil {
		return nil, err
	}
	if profiles == nil {
		e.note(noteTerminalNoProfiles, "Terminal.app has no saved profiles; nothing to change.")
		return nil, nil
	}
	var names, before, after []string
	for i, name := range profiles.keys {
		p := profiles.vals[i]
		if p.kind != "dict" || hasNoWarn(p) {
			continue
		}
		names = append(names, name)
		before = append(before, name+": "+noWarnDescription(p))
		addNoWarn(p)
		after = append(after, name+": "+noWarnDescription(p))
	}
	if len(names) == 0 {
		return nil, nil
	}
	if _, err := e.run.Run(cmdTimeout, "pgrep", "-x", "Terminal"); err == nil {
		e.note(noteTerminalRunning, "Terminal.app is running: it applies the change after it is quit and opened again (until then, closing a window running an agent still asks).")
	}
	c := &Change{ID: "terminal", Target: terminalTarget, Action: "modify",
		Summary: "add stagent to noWarnProcesses of the Terminal profiles " + strings.Join(names, ", ") + " (Terminal does not ask before closing a window running an agent or at logout)",
		Diff:    terminalDiff(before, after)}
	c.apply = func() error {
		// Read again: Terminal may have saved since the plan.
		root, profiles, err := e.terminalPrefs()
		if err != nil {
			return err
		}
		var added []TerminalEntry
		for _, name := range names {
			if p := profiles.get(name); p != nil && p.kind == "dict" && !hasNoWarn(p) {
				added = append(added, TerminalEntry{Profile: name, CreatedKey: addNoWarn(p)})
			}
		}
		if len(added) == 0 {
			return nil
		}
		if err := e.writeTerminalPrefs(root); err != nil {
			return err
		}
		for _, a := range added {
			e.m.TerminalNoWarn = slices.DeleteFunc(e.m.TerminalNoWarn, func(x TerminalEntry) bool { return x.Profile == a.Profile })
			e.m.TerminalNoWarn = append(e.m.TerminalNoWarn, a)
		}
		e.dirty = true
		return nil
	}
	return c, nil
}

// terminalRemoveChange plans taking stagent out of the profiles the
// manifest records; nil when none of them has it any more (the records
// are dropped).
func (e *env) terminalRemoveChange() (*Change, error) {
	if len(e.m.TerminalNoWarn) == 0 {
		return nil, nil
	}
	_, profiles, err := e.terminalPrefs()
	if err != nil {
		return nil, err
	}
	var names, before, after []string
	for _, rec := range e.m.TerminalNoWarn {
		p := profiles.get(rec.Profile)
		if p == nil || p.kind != "dict" || !hasNoWarn(p) {
			continue
		}
		names = append(names, rec.Profile)
		before = append(before, rec.Profile+": "+noWarnDescription(p))
		removeNoWarn(p, rec.CreatedKey)
		after = append(after, rec.Profile+": "+noWarnDescription(p))
	}
	if len(names) == 0 {
		e.m.TerminalNoWarn = nil
		e.dirty = true
		return nil, nil
	}
	recs := e.m.TerminalNoWarn
	c := &Change{ID: "terminal", Target: terminalTarget, Action: "modify",
		Summary: "remove stagent from noWarnProcesses of the Terminal profiles " + strings.Join(names, ", ") + " (only the entry stagent added)",
		Diff:    terminalDiff(before, after)}
	c.apply = func() error {
		root, profiles, err := e.terminalPrefs()
		if err != nil {
			return err
		}
		changed := false
		for _, rec := range recs {
			if p := profiles.get(rec.Profile); p != nil && p.kind == "dict" && hasNoWarn(p) {
				removeNoWarn(p, rec.CreatedKey)
				changed = true
			}
		}
		if changed {
			if err := e.writeTerminalPrefs(root); err != nil {
				return err
			}
		}
		e.m.TerminalNoWarn = nil
		e.dirty = true
		return nil
	}
	return c, nil
}

// terminalReport is doctor's Terminal item; nil off macOS.
func (e *env) terminalReport() *TerminalReport {
	if e.goos != "darwin" {
		return nil
	}
	r := &TerminalReport{AddedByStagent: len(e.m.TerminalNoWarn) > 0}
	if _, profiles, err := e.terminalPrefs(); err == nil && profiles != nil {
		r.Profiles = len(profiles.vals)
		r.NoWarn = true
		for _, p := range profiles.vals {
			if !hasNoWarn(p) {
				r.NoWarn = false
			}
		}
	}
	return r
}

// terminalLeft lists the recorded profiles that still have stagent's
// entry (inventory).
func (e *env) terminalLeft() []string {
	if e.goos != "darwin" || len(e.m.TerminalNoWarn) == 0 {
		return nil
	}
	_, profiles, err := e.terminalPrefs()
	if err != nil {
		return nil
	}
	var left []string
	for _, rec := range e.m.TerminalNoWarn {
		if p := profiles.get(rec.Profile); p != nil && hasNoWarn(p) {
			left = append(left, rec.Profile)
		}
	}
	return left
}
