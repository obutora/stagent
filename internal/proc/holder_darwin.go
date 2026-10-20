package proc

import (
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// spawnLabelPrefix + the spawn name is the label of a spawn job.
const spawnLabelPrefix = "com.obutora.stagent."

// SpawnHolder starts a detached holder. While the user is logged in to the
// GUI it is started from a one-off launchd job in the GUI domain
// (gui/<uid>), so that the holder and its agent belong to the GUI login
// session: the login keychain is unlocked only there. Whether a process can
// read it depends on its audit session, not its bootstrap port, so the
// children of an SSH session or of a user/<uid> job find it locked and
// claude is logged out. The job runs `stagent spawn-task <spec>`, which
// starts the holder with SpawnDetached — outside the job, which the GUI
// logout stops — and removes the job; pid 0 is returned then (callers wait
// for the holder to answer). When there is no GUI login (launchctl refuses
// the job), the holder is started directly and the reason goes to its log.
func SpawnHolder(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	pid, _, err := StartHolder(exe, args, dir, env, logPath)
	return pid, err
}

// StartHolder is SpawnHolder that also tells whether the holder is outside
// the GUI login session (started directly), where claude cannot read its
// login from the keychain.
func StartHolder(exe string, args []string, dir string, env []string, logPath string) (pid int, outsideGUI bool, err error) {
	return startHolder(
		func() error { return spawnViaGUI(exe, args, dir, env, logPath) },
		func() (int, error) { return SpawnDetached(exe, args, dir, env, logPath) },
		logPath)
}

func spawnViaGUI(exe string, args []string, dir string, env []string, logPath string) error {
	name, err := newSpawnName()
	if err != nil {
		return err
	}
	if env == nil {
		env = os.Environ()
	}
	specDir := filepath.Dir(logPath)
	if logPath == "" {
		specDir = os.TempDir()
	} else if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		f.Close() // launchd would create StandardErrorPath readable by others
	}
	specPath := filepath.Join(specDir, name+".json")
	if err := writeSpec(specPath, spawnSpec{Args: args, Dir: dir, Env: env, Log: logPath}); err != nil {
		return err
	}
	plistPath := filepath.Join(specDir, name+".plist")
	if err := os.WriteFile(plistPath, []byte(spawnPlist(spawnLabelPrefix+name, exe, specPath, logPath)), 0o600); err != nil {
		os.Remove(specPath)
		return err
	}
	defer os.Remove(plistPath) // launchd has read it once bootstrap returns
	if out, err := exec.Command("launchctl", "bootstrap", guiDomain(), plistPath).CombinedOutput(); err != nil {
		os.Remove(specPath)
		return fmt.Errorf("launchctl bootstrap %s: %v: %s", guiDomain(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func guiDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

// spawnPlist is the job running `exe spawn-task specPath` once.
// AbandonProcessGroup keeps launchd from killing what the job leaves behind
// when it ends; ProcessType Standard avoids Background's CPU throttling.
func spawnPlist(label, exe, specPath, logPath string) string {
	var b strings.Builder
	str := func(s string) {
		b.WriteString("<string>")
		xml.EscapeText(&b, []byte(s))
		b.WriteString("</string>\n")
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>Label</key>
`)
	str(label)
	b.WriteString("<key>ProgramArguments</key>\n<array>\n")
	str(exe)
	str("spawn-task")
	str(specPath)
	b.WriteString(`</array>
<key>RunAtLoad</key>
<true/>
<key>AbandonProcessGroup</key>
<true/>
<key>ProcessType</key>
<string>Standard</string>
`)
	if logPath != "" {
		b.WriteString("<key>StandardErrorPath</key>\n")
		str(logPath)
	}
	b.WriteString("</dict>\n</plist>\n")
	return b.String()
}

// RunSpawnTask is `stagent spawn-task <spec>` run by the job of SpawnHolder:
// it starts the holder of the spec detached. cleanup removes the job, which
// makes launchd stop this process; call it last.
func RunSpawnTask(specPath string) (cleanup func(), err error) {
	label := spawnLabelPrefix + strings.TrimSuffix(filepath.Base(specPath), ".json")
	cleanup = func() { exec.Command("launchctl", "bootout", guiDomain()+"/"+label).Run() }
	s, err := readSpec(specPath)
	if err != nil {
		return cleanup, err
	}
	exe, err := os.Executable()
	if err != nil {
		return cleanup, err
	}
	_, err = SpawnDetached(exe, s.Args, s.Dir, s.Env, s.Log)
	return cleanup, err
}
