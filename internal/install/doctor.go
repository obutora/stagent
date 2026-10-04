package install

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/hostid"
	"github.com/obutora/stagent/internal/logind"
	"github.com/obutora/stagent/internal/notify"
	"github.com/obutora/stagent/internal/version"
	"github.com/obutora/stagent/internal/wire"
)

// DoctorReport is the output of `stagent doctor` (PROTOCOL.md).
type DoctorReport struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Home     string `json:"home"`
	// HostID is the host's id (hostid); absent until stagent created it
	// (install, the bridge's hello or the daemon). doctor never creates it.
	HostID       string            `json:"host_id,omitempty"`
	Layout       LayoutInfo        `json:"layout"`
	Daemon       DaemonReport      `json:"daemon"`
	Harnesses    []HarnessReport   `json:"harnesses"`
	ShellWrapper ShellReport       `json:"shell_wrapper"`
	Service      ServiceReport     `json:"service"`
	Persistence  PersistenceReport `json:"persistence"`
	// Terminal is Terminal.app's close confirmation (macOS; null
	// elsewhere).
	Terminal *TerminalReport `json:"terminal"`
	// PowerShell lists the installed PowerShells with their effective
	// execution policy (Windows; null elsewhere).
	PowerShell []PowerShellReport `json:"powershell"`
	// RedirectionGuard is whether this doctor process runs with Windows'
	// RedirectionGuard: started over SSH, as kept shells are, it follows
	// no junction a user created (scoop's shims fail). Null off Windows or
	// when unknown.
	RedirectionGuard *bool `json:"redirection_guard"`
	// Notify comes from the daemon's last failures file, so it is there
	// whether or not the daemon runs.
	Notify wire.NotifyStatus `json:"notify"`
	// Unwrapped: agents started without a stagent session that the
	// running daemon saw (empty when it does not run).
	Unwrapped []wire.UnwrappedLaunch `json:"unwrapped"`
	Orphans   []Finding              `json:"orphans"`
	Problems  []string               `json:"problems"`
}

type DaemonReport struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	Version  string `json:"version"`
	Sessions int    `json:"sessions"`
	// BootstrapSwapped is whether the running daemon moved itself to the
	// per-user bootstrap port (macOS); null when it is not running, off
	// macOS, or it is a version before 0.4.0. BootstrapError says why it
	// failed; null otherwise.
	BootstrapSwapped *bool   `json:"bootstrap_swapped"`
	BootstrapError   *string `json:"bootstrap_error"`
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
	// RemoteControlAtStartup (claude only; other harnesses omit it) is
	// remoteControlAtStartup in ~/.claude/settings.json: true, false or
	// null when absent, not a boolean or unreadable.
	RemoteControlAtStartup json.RawMessage `json:"remote_control_at_startup,omitempty"`
}

type ShellReport struct {
	Installed bool     `json:"installed"`
	Files     []string `json:"files"`
	// LastRunAt is when `stagent run --handoff=auto` (a shell wrapper) last
	// started a program on this host, unix ms; null when never.
	LastRunAt *int64 `json:"last_run_at"`
	// LastRunSurvivesLogout is whether that start outlives the user's
	// logout: on Linux where it ended up after leaving the login session,
	// judged with logind's settings now; on macOS whether it swapped its
	// bootstrap port. Null when unknown or never recorded (before 0.4.0,
	// Windows).
	LastRunSurvivesLogout *bool `json:"last_run_survives_logout"`
	// LastRunBootstrapError is why that start's swap failed (macOS); null
	// otherwise.
	LastRunBootstrapError *string `json:"last_run_bootstrap_error"`
}

type ServiceReport struct {
	Kind      string `json:"kind"` // systemd | launchd | schtasks | none
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
}

// PersistenceReport says whether detached sessions outlive the user's
// logout. Unknown facts (off Linux, logind not reachable) are null.
type PersistenceReport struct {
	RunDir string `json:"run_dir"`
	// RunDirSurvivesLogout: RunDir is outside $XDG_RUNTIME_DIR, which
	// logind deletes at the last logout.
	RunDirSurvivesLogout bool  `json:"run_dir_survives_logout"`
	KillUserProcesses    *bool `json:"kill_user_processes"`
	Linger               *bool `json:"linger"`
	// LingerNeeded: agents started here end at logout unless lingering is
	// turned on (lingerNeeded); null when unknown.
	LingerNeeded *bool `json:"linger_needed"`
	// LingerReason says why LingerNeeded is true (lingerReason* values);
	// null otherwise.
	LingerReason *string `json:"linger_reason"`
	// LingerEnabledByStagent: the manifest records that `integrate
	// --linger` turned lingering on.
	LingerEnabledByStagent bool `json:"linger_enabled_by_stagent"`
	// WSL is WSL を動かし続ける when Linux runs in WSL; null otherwise.
	WSL *WSLReport `json:"wsl"`
}

// Values of PersistenceReport.LingerReason.
const (
	lingerReasonKill      = "kill_user_processes"
	lingerReasonGraphical = "graphical_session"
	lingerReasonLastRun   = "last_run"
)

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
	if left := e.terminalLeft(); len(left) > 0 {
		// Harmless without the binary, so never an orphan.
		add(levelUnhook, terminalTarget, "stagent in noWarnProcesses of the Terminal profiles "+strings.Join(left, ", "), false, true)
	}
	if rec := e.wslRecord(); rec != nil {
		if cur, err := readOptional(rec.Path); err == nil && cur != nil && wslPresent(cur) != "" {
			// The user's own Windows setting; purge undoes it.
			add(levelPurge, rec.Path, "WSL keeps running ("+wslIdleLine+" in [general]), added by stagent", false, true)
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
		OS: e.goos, Arch: runtime.GOARCH, Home: e.l.Home, HostID: hostid.Read(e.l.HostID),
		Layout:    layoutInfo(e.l),
		Harnesses: []HarnessReport{},
		Unwrapped: []wire.UnwrappedLaunch{},
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
		r.Daemon = DaemonReport{Running: true, PID: st.PID, Version: st.Version, Sessions: st.Sessions, BootstrapSwapped: st.BootstrapSwapped}
		if st.Unwrapped != nil {
			r.Unwrapped = st.Unwrapped
		}
		if st.Version != version.Version {
			r.Problems = append(r.Problems, "running daemon is version "+st.Version+", binary is "+version.Version+" (restart it with `stagent uninstall --level stop`)")
		}
		if st.BootstrapSwapped != nil && !*st.BootstrapSwapped {
			r.Daemon.BootstrapError = &st.BootstrapError
			r.Problems = append(r.Problems, "the daemon cannot reach the network after you log out of the GUI (push notifications fail): switching to the per-user bootstrap port failed: "+st.BootstrapError)
		}
	}

	r.Harnesses = append(r.Harnesses, e.claudeReport())
	r.Harnesses = append(r.Harnesses, e.codexReport())
	r.Harnesses = append(r.Harnesses, e.harnessReport(hOmp, e.ompTarget()))

	r.ShellWrapper.Files = []string{}
	for _, t := range e.shellTargets() {
		if cur, _ := readOptional(t.path); cur != nil && t.present(cur) != "" {
			r.ShellWrapper.Installed = true
			r.ShellWrapper.Files = append(r.ShellWrapper.Files, t.path)
		}
	}
	lastPlace := logind.PlaceUnknown
	if rec, ok := e.l.ReadWrapperRun(); ok {
		r.ShellWrapper.LastRunAt = &rec.At
		r.ShellWrapper.LastRunSurvivesLogout = rec.SurvivesLogout
		lastPlace = logind.ParsePlacement(rec.Placement)
		if rec.BootstrapError != "" {
			r.ShellWrapper.LastRunBootstrapError = &rec.BootstrapError
			r.Problems = append(r.Problems, "the tools of the last agent started through the shell wrapper cannot resolve host names after you log out of the GUI: switching to the per-user bootstrap port failed: "+rec.BootstrapError)
		}
	}
	r.Service.Kind = e.serviceKind()
	r.Service.Installed, r.Service.Running = e.serviceState()

	var lastRun *bool
	r.Persistence, lastRun = e.persistenceReport(lastPlace)
	if e.goos == "linux" {
		r.ShellWrapper.LastRunSurvivesLogout = lastRun
	}
	r.Terminal = e.terminalReport()
	r.PowerShell = e.powerShells()
	for _, p := range r.PowerShell {
		if p.LoadsProfile != nil && !*p.LoadsProfile {
			r.Problems = append(r.Problems, p.Name+" does not run its profile (execution policy "+*p.ExecutionPolicy+"), so the shell wrapper stays inactive there; run `stagent integrate --apply --execution-policy` to set RemoteSigned for your user")
		}
	}
	if e.goos == "windows" && e.redirectionGuard != nil {
		r.RedirectionGuard = e.redirectionGuard()
	}
	if p := r.Persistence; p.LingerNeeded != nil && *p.LingerNeeded {
		const fix = "; run `loginctl enable-linger` (may require an administrator) to keep them running"
		switch *p.LingerReason {
		case lingerReasonKill:
			r.Problems = append(r.Problems, "detached sessions end when you log out: logind kills user processes (KillUserProcesses=yes) and lingering is off"+fix)
		case lingerReasonGraphical:
			r.Problems = append(r.Problems, "agents started from a terminal of your graphical session end when you log out: they run under your systemd user manager and lingering is off"+fix)
		default:
			r.Problems = append(r.Problems, "the last agent started through the shell wrapper ends when you log out: lingering is off"+fix)
		}
	}
	if w := r.Persistence.WSL; w != nil && w.KeepRunningNeeded != nil && *w.KeepRunningNeeded {
		r.Problems = append(r.Problems, wslProblem(w))
	}

	r.Notify.LastError, _ = notify.LoadFailures(e.l.NotifyErrors)
	for _, ch := range slices.Sorted(maps.Keys(r.Notify.LastError)) {
		f := r.Notify.LastError[ch]
		r.Problems = append(r.Problems, "notify: the last push to "+ch+" failed at "+time.UnixMilli(f.At).Format(time.RFC3339)+": "+f.Error)
	}

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

// persistenceReport checks what ends detached sessions at logout on Linux:
// a RunDir in $XDG_RUNTIME_DIR (deleted at the last logout), logind's
// KillUserProcesses and lingering (see lingerNeeded). lastPlace is where
// the last start through a shell wrapper ran (PlaceUnknown when none is
// recorded); lastRun is whether it outlives logout with the settings read
// now, nil when unknown and off Linux.
func (e *env) persistenceReport(lastPlace logind.Placement) (p PersistenceReport, lastRun *bool) {
	p = PersistenceReport{RunDir: e.l.RunDir, RunDirSurvivesLogout: true, LingerEnabledByStagent: e.m.LingerEnabled, WSL: e.wslReport()}
	if e.goos != "linux" {
		return p, nil
	}
	if x := e.getenv("XDG_RUNTIME_DIR"); x != "" {
		rel, err := filepath.Rel(x, e.l.RunDir)
		p.RunDirSurvivesLogout = err != nil || strings.HasPrefix(rel, "..")
	}
	var f logind.Facts
	cmd := logind.KillUserProcessesCmd
	if out, err := e.run.Run(cmdTimeout, cmd[0], cmd[1:]...); err == nil {
		if f.Kill, f.KillKnown = logind.ParseBusctlBool(out); f.KillKnown {
			p.KillUserProcesses = &f.Kill
		}
	}
	if f.Linger, f.LingerKnown = e.linger(); f.LingerKnown {
		p.Linger = &f.Linger
	}
	if v, known := f.Survives(lastPlace); known {
		lastRun = &v
	}
	e.lingerNeeded(&p, lastRun)
	return p, lastRun
}

// lingerNeeded sets LingerNeeded and LingerReason. Lingering is needed
// when it is off and agents started here end at logout: as the last start
// through a shell wrapper does where it runs (lastRun), or, before any
// such start, when logind kills user processes (holders leave the session
// for the user manager, which ends at the last logout) or the user has a
// graphical session (its terminals run under the user manager). Unknown
// when neither KillUserProcesses nor lingering can be read.
func (e *env) lingerNeeded(p *PersistenceReport, lastRun *bool) {
	if p.WSL != nil {
		// WSL keeps the user's login session for as long as the instance
		// runs, so the user manager never stops before the agents do:
		// lingering changes nothing (WSL を動かし続ける does).
		needed := false
		p.LingerNeeded = &needed
		return
	}
	if p.KillUserProcesses == nil && p.Linger == nil {
		return
	}
	needed, reason := false, ""
	switch {
	case p.Linger != nil && *p.Linger:
	case lastRun != nil:
		needed, reason = !*lastRun, lingerReasonLastRun
	case p.KillUserProcesses != nil && *p.KillUserProcesses:
		needed, reason = true, lingerReasonKill
	case e.graphicalSession():
		needed, reason = true, lingerReasonGraphical
	}
	p.LingerNeeded = &needed
	if needed {
		p.LingerReason = &reason
	}
}

// graphicalSession reports whether the user has a login session of type
// x11 or wayland now.
func (e *env) graphicalSession() bool {
	out, err := e.run.Run(cmdTimeout, "loginctl", "show-user", strconv.Itoa(e.uid), "--property=Sessions", "--value")
	ids := strings.Fields(out)
	if err != nil || len(ids) == 0 {
		return false
	}
	// A session that ended in between fails the command; the others are
	// still printed.
	out, _ = e.run.Run(cmdTimeout, "loginctl", append(append([]string{"show-session"}, ids...), "--property=Type", "--value")...)
	for _, t := range strings.Fields(out) {
		if t == "x11" || t == "wayland" {
			return true
		}
	}
	return false
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

// claudeReport adds the user's Remote Control setting to the claude item.
// Only ~/.claude/settings.json is read: managed and project settings are
// not guessed, and stagent never changes the setting.
func (e *env) claudeReport() HarnessReport {
	h := e.harnessReport(hClaude, e.claudeTarget())
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

func (e *env) codexReport() HarnessReport {
	hooks := e.codexHooksTarget()
	h := e.harnessReport(hCodex, hooks)
	cfg := e.codexConfigTarget("")
	mode, reason := e.codexMode()
	h.HooksSupported = mode == "hooks"
	if !h.HooksSupported {
		h.ConfigPath = cfg.path
		h.Notes = append(h.Notes, codexNotifyWhy[reason]+"; integration uses the notify program")
	}
	if cur, _ := readOptional(cfg.path); cur != nil {
		d := parseTOMLLines(cur)
		if i := topLevelNotify(d); i >= 0 && isOurCommand(d.lines[i].text()) {
			if !h.HooksSupported {
				h.Integrated = true
			} else if removeNotify(parseTOMLLines(cur)) {
				// A leftover of the notify fallback: integrate replaces it
				// with hooks, so the app offers to run it.
				h.Integrated = false
				h.Notes = append(h.Notes, "config.toml still runs stagent as the notify program (set up while this Codex had no hooks); integrate replaces it with hooks")
			}
		}
		if on, set := featureHooksEnabled(d); set && !on && h.HooksSupported {
			h.Notes = append(h.Notes, "hooks are disabled in config.toml ([features] hooks = false); integrate enables them")
		}
	}
	return h
}
