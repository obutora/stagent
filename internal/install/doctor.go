package install

import (
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/obutora/stagent/internal/version"
)

// DoctorReport is the output of `stagent doctor` (PROTOCOL.md).
type DoctorReport struct {
	Version      string          `json:"version"`
	Protocol     int             `json:"protocol"`
	OS           string          `json:"os"`
	Arch         string          `json:"arch"`
	Home         string          `json:"home"`
	Layout       LayoutInfo      `json:"layout"`
	Daemon       DaemonReport    `json:"daemon"`
	Harnesses    []HarnessReport `json:"harnesses"`
	ShellWrapper ShellReport     `json:"shell_wrapper"`
	Service      ServiceReport   `json:"service"`
	Orphans      []Finding       `json:"orphans"`
	Problems     []string        `json:"problems"`
}

type DaemonReport struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	Sessions int    `json:"sessions"`
}

type HarnessReport struct {
	ID             string   `json:"id"`
	Found          bool     `json:"found"`
	Path           string   `json:"path"`
	Version        string   `json:"version"`
	ConfigPath     string   `json:"config_path"`
	Integrated     bool     `json:"integrated"`
	HooksSupported bool     `json:"hooks_supported"`
	Notes          []string `json:"notes"`
}

type ShellReport struct {
	Installed bool     `json:"installed"`
	Files     []string `json:"files"`
}

type ServiceReport struct {
	Kind      string `json:"kind"` // systemd | launchd | schtasks | none
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
}

// Finding names something stagent left on the host.
type Finding struct {
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// Artifact levels: what each uninstall level removes.
const (
	levelStop = iota + 1
	levelUnhook
	levelPurge
)

var levelNames = map[string]int{"stop": levelStop, "unhook": levelUnhook, "purge": levelPurge}

type artifact struct {
	Finding
	level int
	// integration elements (hooks, blocks, extension, service) can be
	// orphans; tracked says whether the manifest records them.
	integration bool
	tracked     bool
}

// inventory lists everything of ours that is present on the host.
func (e *env) inventory(uploads bool) []artifact {
	var out []artifact
	add := func(level int, target, detail string, integration, tracked bool) {
		out = append(out, artifact{Finding{target, detail}, level, integration, tracked})
	}
	if st, err := e.daemon.Status(); err == nil {
		add(levelStop, e.l.DaemonAddr, "daemon running (pid "+strconv.Itoa(st.PID)+")", false, true)
	}
	kind := e.serviceKind()
	installed, running := e.serviceState()
	if running {
		add(levelStop, kind, "login service running", false, true)
	}
	for _, t := range e.allTargets() {
		cur, err := readOptional(t.path)
		if err != nil || cur == nil {
			continue
		}
		if d := t.present(cur); d != "" {
			add(levelUnhook, t.path, d, true, e.m.config(t.id, t.path) != nil)
		}
	}
	if installed {
		tracked := e.m.hasService(kind)
		switch kind {
		case "systemd":
			add(levelUnhook, e.systemdUnitPath(), "systemd user unit "+systemdUnit, true, tracked)
		case "launchd":
			add(levelUnhook, e.launchdPlistPath(), "LaunchAgent "+launchdLabel, true, tracked)
		}
	}
	if kind == "schtasks" {
		for _, t := range e.stagentTasks() {
			tracked := t != schtasksDaemon || e.m.hasService(kind)
			add(levelUnhook, "schtasks:"+t, "scheduled task", t == schtasksDaemon, tracked)
		}
	}
	if exists(e.l.Root) {
		add(levelPurge, e.l.Root, "stagent installation (binary, manifest, state, logs)", false, true)
	}
	for _, d := range e.outsideDirs() {
		if exists(d) {
			add(levelPurge, d, "stagent data outside the installation directory", false, true)
		}
	}
	for _, b := range e.m.Backups {
		if exists(b) {
			add(levelPurge, b, "backup of a config file taken before stagent edited it", false, true)
		}
	}
	if uploads && exists(e.l.UploadsDir) {
		add(levelPurge, e.l.UploadsDir, "files sent from the app", false, true)
	}
	return out
}

// outsideDirs are stagent directories that live outside Root: relocated
// scrollback (network home) and the socket directory under
// XDG_RUNTIME_DIR / TMPDIR.
func (e *env) outsideDirs() []string {
	var out []string
	inRoot := func(p string) bool {
		rel, err := filepath.Rel(e.l.Root, p)
		return err == nil && !strings.HasPrefix(rel, "..")
	}
	for _, d := range []string{e.l.DataDir, e.m.Layout.DataDir} {
		if d == "" || inRoot(d) {
			continue
		}
		// .../stagent-<uid>/sessions → remove the whole stagent-<uid>
		if p := filepath.Dir(d); strings.HasPrefix(filepath.Base(p), "stagent-") {
			d = p
		}
		out = addUnique(out, d)
	}
	for _, d := range []string{e.l.RunDir, e.m.Layout.RunDir} {
		if d == "" || inRoot(d) || !strings.HasPrefix(filepath.Base(d), "stagent") {
			continue
		}
		out = addUnique(out, d)
	}
	return out
}

func (e *env) doctor() *DoctorReport {
	r := &DoctorReport{
		Version: version.Version, Protocol: version.Protocol,
		OS: e.goos, Arch: runtime.GOARCH, Home: e.l.Home,
		Layout:    layoutInfo(e.l),
		Harnesses: []HarnessReport{},
		Orphans:   []Finding{},
		Problems:  []string{},
	}
	binOK := exists(e.l.Bin)
	if !binOK {
		r.Problems = append(r.Problems, "stagent binary missing at "+e.l.Bin)
	}
	switch {
	case e.mErr != nil:
		r.Problems = append(r.Problems, "manifest unreadable: "+e.mErr.Error())
	case !e.haveM:
		r.Problems = append(r.Problems, "not installed: no manifest at "+e.l.Manifest+" (run `stagent install`)")
	}
	if st, err := e.daemon.Status(); err == nil {
		r.Daemon = DaemonReport{Running: true, PID: st.PID, Version: st.Version, Sessions: st.Sessions}
		if st.Version != version.Version {
			r.Problems = append(r.Problems, "running daemon is version "+st.Version+", binary is "+version.Version+" (restart it with `stagent uninstall --level stop`)")
		}
	}

	r.Harnesses = append(r.Harnesses, e.harnessReport(hClaude, e.claudeTarget()))
	r.Harnesses = append(r.Harnesses, e.codexReport())
	r.Harnesses = append(r.Harnesses, e.harnessReport(hOmp, e.ompTarget()))

	r.ShellWrapper.Files = []string{}
	for _, t := range e.shellTargets() {
		if cur, _ := readOptional(t.path); cur != nil && t.present(cur) != "" {
			r.ShellWrapper.Installed = true
			r.ShellWrapper.Files = append(r.ShellWrapper.Files, t.path)
		}
	}
	r.Service.Kind = e.serviceKind()
	r.Service.Installed, r.Service.Running = e.serviceState()

	for _, a := range e.inventory(false) {
		if !a.integration {
			continue
		}
		switch {
		case !binOK:
			r.Orphans = append(r.Orphans, Finding{a.Target, a.Detail + "; the stagent binary is missing"})
		case !a.tracked:
			r.Orphans = append(r.Orphans, Finding{a.Target, a.Detail + "; not recorded in the manifest"})
		}
	}
	return r
}

func (e *env) harnessReport(id string, t *target) HarnessReport {
	h := HarnessReport{ID: id, ConfigPath: t.path, HooksSupported: true, Notes: []string{}}
	h.Path = e.findHarness(id)
	h.Found = h.Path != ""
	if h.Found {
		h.Version = e.harnessVersion(h.Path)
	}
	cur, err := readOptional(t.path)
	if err != nil {
		h.Notes = append(h.Notes, err.Error())
	}
	if cur != nil {
		h.Integrated = t.present(cur) != ""
		if id == hClaude {
			if _, err := parseJSONDoc(cur); err != nil {
				h.Notes = append(h.Notes, t.path+" is not valid JSON")
			}
		}
	}
	return h
}

func (e *env) codexReport() HarnessReport {
	hooks := e.codexHooksTarget()
	h := e.harnessReport(hCodex, hooks)
	cfg := e.codexConfigTarget("")
	mode, why := e.codexMode()
	h.HooksSupported = mode == "hooks"
	if !h.HooksSupported {
		h.ConfigPath = cfg.path
		h.Notes = append(h.Notes, why+"; integration uses the notify program")
	}
	if cur, _ := readOptional(cfg.path); cur != nil {
		d := parseTOMLLines(cur)
		if i := topLevelNotify(d); i >= 0 && isOurCommand(d.lines[i].text()) {
			h.Integrated = true
		}
		if on, set := featureHooksEnabled(d); set && !on && h.HooksSupported {
			h.Notes = append(h.Notes, "hooks are disabled in config.toml ([features] hooks = false); integrate enables them")
		}
	}
	return h
}
