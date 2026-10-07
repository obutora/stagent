package install

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Opt-in login service keeping the daemon running (needed for push
// notifications while no app is connected): a systemd user unit, a launchd
// LaunchAgent, or a Windows scheduled task at logon. System commands go
// through e.run.

const (
	systemdUnit    = "stagent.service"
	launchdLabel   = "com.obutora.stagent"
	schtasksDaemon = `\stagent\daemon`
	// schtasksFolder also holds the spawn fallback tasks of proc.SpawnDetached.
	schtasksFolder = `\stagent\`
	serviceMarker  = "installed by ssh-term (stagent integrate --service); managed file"
	cmdTimeout     = 15 * time.Second
)

func (e *env) serviceKind() string {
	switch e.goos {
	case "linux":
		if _, err := e.lookPath("systemctl"); err == nil {
			return "systemd"
		}
	case "darwin":
		return "launchd"
	case "windows":
		return "schtasks"
	}
	return "none"
}

func (e *env) systemdUnitPath() string {
	return e.home(".config", "systemd", "user", systemdUnit)
}

func (e *env) launchdPlistPath() string {
	return e.home("Library", "LaunchAgents", launchdLabel+".plist")
}

// runCmd runs a system command and folds its output into the error.
func (e *env) runCmd(name string, args ...string) error {
	out, err := e.run.Run(cmdTimeout, name, args...)
	if err != nil {
		if s := strings.TrimSpace(out); s != "" {
			return errors.New(name + " " + strings.Join(args, " ") + ": " + s)
		}
		return errors.New(name + " " + strings.Join(args, " ") + ": " + err.Error())
	}
	return nil
}

// linger reports whether systemd keeps the user's service manager (and all
// that runs under it) after the last logout; known is false when loginctl
// cannot tell.
func (e *env) linger() (on, known bool) {
	out, err := e.run.Run(cmdTimeout, "loginctl", "show-user", strconv.Itoa(e.uid), "--property=Linger", "--value")
	if err != nil {
		return false, false
	}
	switch strings.TrimSpace(out) {
	case "yes":
		return true, true
	case "no":
		return false, true
	}
	return false, false
}

// --- systemd -----------------------------------------------------------------

func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

func (e *env) systemdUnitText() string {
	var sb strings.Builder
	sb.WriteString("# " + serviceMarker + "\n")
	sb.WriteString("[Unit]\nDescription=SSH Term agent daemon (stagent)\n\n[Service]\nType=simple\n")
	sb.WriteString("ExecStart=" + systemdQuote(e.l.Bin) + " daemon\n")
	sb.WriteString("Restart=on-failure\nRestartSec=5\n")
	// The daemon's socket lives under TMPDIR (paths); give the service's
	// daemon the TMPDIR this host's clients resolve it with. Environment=
	// expands specifiers but not variables, so only % is doubled.
	if t := e.getenv("TMPDIR"); t != "" {
		q := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%")
		sb.WriteString(`Environment="` + q.Replace("TMPDIR="+t) + "\"\n")
	} else {
		sb.WriteString("UnsetEnvironment=TMPDIR\n")
	}
	sb.WriteString("\n[Install]\nWantedBy=default.target\n")
	return sb.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// launchdPlistText is the LaunchAgent of the daemon, for the user domain
// (user/<uid>): LimitLoadToSessionType=Background, so launchd loads it
// there, outside any GUI session and past its logout, and ProcessType
// Standard, since Background throttles the CPU 10–30 times.
func (e *env) launchdPlistText() string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!-- ` + serviceMarker + ` -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + launchdLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + xmlEscape(e.l.Bin) + `</string>
    <string>daemon</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>LimitLoadToSessionType</key>
  <string>Background</string>
  <key>ProcessType</key>
  <string>Standard</string>
  <key>StandardErrorPath</key>
  <string>` + xmlEscape(filepath.Join(e.l.LogDir, "daemon.log")) + `</string>
</dict>
</plist>
`
}

// taskXML defines the logon task that runs the daemon as the current user
// (S4U: no stored password, no admin rights).
func (e *env) taskXML() string {
	sid := xmlEscape(e.sid)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>SSH Term agent daemon (stagent) - ` + serviceMarker + `</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + sid + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + sid + `</UserId>
      <LogonType>S4U</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>3</Count>
    </RestartOnFailure>
    <Hidden>true</Hidden>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + xmlEscape(e.l.Bin) + `</Command>
      <Arguments>daemon</Arguments>
      <WorkingDirectory>` + xmlEscape(e.l.Home) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// ownedFileTarget is a service definition file we own entirely.
func (e *env) ownedFileTarget(id, path, content string, afterRemove func() error) *target {
	marker := serviceMarker
	return &target{
		id: id, path: path, owned: true, mode: 0o644,
		identify: "whole file (" + marker + ")",
		add: func(cur []byte) (addResult, error) {
			if cur != nil && !strings.Contains(string(cur), marker) {
				return addResult{}, unmanagedFile(path + " exists and is not managed by stagent; left unchanged")
			}
			return addResult{after: []byte(content), summary: "install the login service definition"}, nil
		},
		remove: func(cur []byte, _ *ConfigEntry) ([]byte, error) {
			if !strings.Contains(string(cur), marker) {
				return cur, nil
			}
			return nil, nil
		},
		present: func(cur []byte) string {
			if strings.Contains(string(cur), marker) {
				return "login service definition"
			}
			return ""
		},
		afterRemove: afterRemove,
	}
}

// serviceTarget is the service definition file (nil for schtasks / none).
func (e *env) serviceTarget() *target {
	switch e.serviceKind() {
	case "systemd":
		return e.ownedFileTarget("service", e.systemdUnitPath(), e.systemdUnitText(), func() error {
			return e.runCmd("systemctl", "--user", "daemon-reload")
		})
	case "launchd":
		return e.ownedFileTarget("service", e.launchdPlistPath(), e.launchdPlistText(), nil)
	}
	return nil
}

// stopRunningDaemon asks a running daemon to exit so the service's own
// instance can take the IPC address. It fails only when the daemon refuses
// (see agentRefused).
func (e *env) stopRunningDaemon() error {
	if st, err := e.daemon.Status(); err == nil {
		if err := e.daemon.Shutdown(); agentRefused(err) {
			return err
		}
		e.waitDaemonGone(st.PID)
	}
	return nil
}

func (e *env) waitDaemonGone(pid int) bool {
	deadline := time.Now().Add(e.settle)
	for {
		if _, err := e.daemon.Status(); err != nil && (pid <= 0 || !e.alive(pid)) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// launchdDomains are the domains our LaunchAgent may be loaded in: the user
// domain it is bootstrapped into, then the GUI domain of versions before
// 0.4.0, so that stopping and removing finds an older load too.
func (e *env) launchdDomains() []string {
	u := strconv.Itoa(e.uid)
	return []string{"user/" + u, "gui/" + u}
}

// serviceAddChanges plans installing and starting the service.
func (e *env) serviceAddChanges() ([]*Change, error) {
	kind := e.serviceKind()
	switch kind {
	case "none":
		return nil, errors.New("no supported service manager on this host (systemd user instance, launchd or Task Scheduler)")
	case "schtasks":
		xml := e.taskXML()
		sum := sha256Hex([]byte(xml))
		for _, s := range e.m.Services {
			if s.Kind == kind && s.SHA256 == sum && e.taskExists(schtasksDaemon) {
				return nil, nil
			}
		}
		c := &Change{ID: "service", Target: "schtasks:" + schtasksDaemon, Action: "create",
			Summary: "register a logon task that runs `stagent daemon` as the current user, and start it",
			Diff:    unifiedDiff("schtasks:"+schtasksDaemon, nil, []byte(xml))}
		c.apply = func() error {
			tmp := filepath.Join(e.l.LogDir, "daemon-task.xml")
			if err := os.MkdirAll(e.l.LogDir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(tmp, utf16LE(xml), 0o600); err != nil {
				return err
			}
			defer os.Remove(tmp)
			if err := e.stopRunningDaemon(); err != nil {
				return err
			}
			if err := e.runCmd("schtasks", "/Create", "/TN", schtasksDaemon, "/XML", tmp, "/F"); err != nil {
				return err
			}
			e.m.setService(ServiceEntry{Kind: kind, Name: schtasksDaemon, SHA256: sum})
			e.dirty = true
			return e.runCmd("schtasks", "/Run", "/TN", schtasksDaemon)
		}
		return []*Change{c}, nil
	}
	t := e.serviceTarget()
	c, err := e.addChange(t)
	if err != nil || c == nil {
		return nil, err
	}
	write := c.apply
	if kind == "systemd" {
		c.Summary = "install the systemd user unit and run `systemctl --user enable --now` (restart) for it"
		c.apply = func() error {
			if err := write(); err != nil {
				return err
			}
			e.m.setService(ServiceEntry{Kind: kind, Name: systemdUnit, Path: t.path})
			e.dirty = true
			if err := e.stopRunningDaemon(); err != nil {
				return err
			}
			if err := e.runCmd("systemctl", "--user", "daemon-reload"); err != nil {
				return err
			}
			if err := e.runCmd("systemctl", "--user", "enable", systemdUnit); err != nil {
				return err
			}
			return e.runCmd("systemctl", "--user", "restart", systemdUnit)
		}
		if on, _ := e.linger(); !on {
			e.note(noteServiceLinger, "systemd stops user services when your last session ends unless lingering is enabled: run `loginctl enable-linger` (may require an administrator) to keep the daemon running after logout.")
		}
	} else {
		c.Summary = "install the LaunchAgent and load it into the user domain with `launchctl bootstrap user/<uid>`"
		c.apply = func() error {
			if err := write(); err != nil {
				return err
			}
			e.m.setService(ServiceEntry{Kind: kind, Name: launchdLabel, Path: t.path})
			e.dirty = true
			if err := e.stopRunningDaemon(); err != nil {
				return err
			}
			for _, d := range e.launchdDomains() {
				e.run.Run(cmdTimeout, "launchctl", "bootout", d+"/"+launchdLabel)
			}
			return e.runCmd("launchctl", "bootstrap", e.launchdDomains()[0], t.path)
		}
		e.note(noteLaunchAgent, "The LaunchAgent runs in your user domain (user/<uid>), outside the GUI session, so the daemon keeps running after you log out; agents do not need it for that. Whether it starts after a restart before anyone has logged in has not been verified.")
	}
	return []*Change{c}, nil
}

// serviceRemoveChanges plans stopping and removing the service (and, on
// Windows, the spawn fallback tasks).
func (e *env) serviceRemoveChanges() ([]*Change, error) {
	switch e.serviceKind() {
	case "schtasks":
		tasks := e.stagentTasks()
		if len(tasks) == 0 {
			if e.m.hasService("schtasks") {
				e.m.dropService("schtasks")
				e.dirty = true
			}
			return nil, nil
		}
		c := &Change{ID: "service", Target: "schtasks:" + schtasksFolder, Action: "delete",
			Summary: "end and delete the scheduled tasks " + strings.Join(tasks, ", ")}
		c.apply = func() error {
			var errs []error
			for _, t := range tasks {
				e.run.Run(cmdTimeout, "schtasks", "/End", "/TN", t)
				if err := e.runCmd("schtasks", "/Delete", "/TN", t, "/F"); err != nil {
					errs = append(errs, err)
				}
			}
			if len(errs) == 0 {
				e.m.dropService("schtasks")
				e.dirty = true
			}
			return errors.Join(errs...)
		}
		return []*Change{c}, nil
	case "systemd", "launchd":
		t := e.serviceTarget()
		c, err := e.removeChange(t)
		if err != nil || c == nil {
			if err == nil && e.m.hasService(e.serviceKind()) {
				e.m.dropService(e.serviceKind())
				e.dirty = true
			}
			return nil, err
		}
		kind := e.serviceKind()
		del := c.apply
		c.apply = func() error {
			if kind == "systemd" {
				e.run.Run(cmdTimeout, "systemctl", "--user", "disable", "--now", systemdUnit)
			} else {
				for _, d := range e.launchdDomains() {
					e.run.Run(cmdTimeout, "launchctl", "bootout", d+"/"+launchdLabel)
				}
			}
			if err := del(); err != nil {
				return err
			}
			e.m.dropService(kind)
			e.dirty = true
			return nil
		}
		return []*Change{c}, nil
	}
	return nil, nil
}

// stopService stops (but keeps) the service. ok=false with a nil error means
// there was nothing to stop.
func (e *env) stopService() (target string, done bool, err error) {
	switch e.serviceKind() {
	case "systemd":
		if !exists(e.systemdUnitPath()) {
			return "", false, nil
		}
		return systemdUnit, true, e.runCmd("systemctl", "--user", "stop", systemdUnit)
	case "launchd":
		if !exists(e.launchdPlistPath()) {
			return "", false, nil
		}
		for _, d := range e.launchdDomains() {
			if _, err := e.run.Run(cmdTimeout, "launchctl", "print", d+"/"+launchdLabel); err == nil {
				return launchdLabel, true, e.runCmd("launchctl", "bootout", d+"/"+launchdLabel)
			}
		}
		return "", false, nil
	case "schtasks":
		if !e.taskExists(schtasksDaemon) {
			return "", false, nil
		}
		return schtasksDaemon, true, e.runCmd("schtasks", "/End", "/TN", schtasksDaemon)
	}
	return "", false, nil
}

// serviceState reports whether the service is installed and running.
func (e *env) serviceState() (installed, running bool) {
	switch e.serviceKind() {
	case "systemd":
		installed = exists(e.systemdUnitPath())
		if installed {
			out, _ := e.run.Run(cmdTimeout, "systemctl", "--user", "is-active", systemdUnit)
			running = strings.TrimSpace(out) == "active"
		}
	case "launchd":
		installed = exists(e.launchdPlistPath())
		if installed {
			for _, d := range e.launchdDomains() {
				out, err := e.run.Run(cmdTimeout, "launchctl", "print", d+"/"+launchdLabel)
				if err == nil && strings.Contains(out, "state = running") {
					running = true
				}
			}
		}
	case "schtasks":
		out, err := e.run.Run(cmdTimeout, "schtasks", "/Query", "/TN", schtasksDaemon, "/FO", "CSV", "/NH")
		installed = err == nil
		running = installed && strings.Contains(out, "Running")
	}
	return installed, running
}

func (e *env) taskExists(name string) bool {
	_, err := e.run.Run(cmdTimeout, "schtasks", "/Query", "/TN", name)
	return err == nil
}

// stagentTasks lists the scheduled tasks in our folder (the daemon task and
// leftover spawn fallback tasks).
func (e *env) stagentTasks() []string {
	out, err := e.run.Run(cmdTimeout, "schtasks", "/Query", "/FO", "CSV", "/NH")
	if err != nil {
		return nil
	}
	var tasks []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		end := strings.Index(line[1:], `"`)
		if end < 0 {
			continue
		}
		name := line[1 : 1+end]
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(schtasksFolder)) && !seen[name] {
			seen[name] = true
			tasks = append(tasks, name)
		}
	}
	return tasks
}
