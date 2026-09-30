// Package proc starts stagent processes that must outlive their caller: the
// daemon (auto-started by whoever needs it first) and detached holders.
package proc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"

	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/paths"
)

const (
	createBreakawayFromJob = 0x01000000
	createNewProcessGroup  = 0x00000200
	detachedProcess        = 0x00000008
)

// TaskFolder is the Task Scheduler folder holding fallback spawn tasks.
// Uninstall deletes everything under it.
const TaskFolder = `\stagent\`

// SpawnDetached starts exe with args outside the caller's console and job.
//
// Win32-OpenSSH puts every session in a kill-on-close job object, so a plain
// child dies when the SSH connection closes. The process is first created
// with CREATE_BREAKAWAY_FROM_JOB; when the job forbids breakaway, it is
// started through a one-off Task Scheduler task instead (pid 0 is returned
// then — callers wait for the process to register itself).
func SpawnDetached(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	pid, err := start(exe, args, dir, env, logPath)
	if err == nil {
		return pid, nil
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return 0, err
	}
	return 0, spawnViaTask(exe, args, dir, env, logPath)
}

func start(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	cmd := exec.Command(exe, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createBreakawayFromJob | createNewProcessGroup | detachedProcess,
		HideWindow:    true,
	}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go cmd.Wait()
	return pid, nil
}

// spawnSpec is written next to the log for the scheduled task to pick up;
// the task's own command line stays short (schtasks /TR is limited to 261
// characters, and the XML Arguments field is kept equally small).
type spawnSpec struct {
	Task string   `json:"task"`
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	Log  string   `json:"log"`
}

func spawnViaTask(exe string, args []string, dir string, env []string, logPath string) error {
	var rb [6]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return err
	}
	name := "spawn-" + hex.EncodeToString(rb[:])
	task := TaskFolder + name
	if env == nil {
		env = os.Environ()
	}
	specDir := filepath.Dir(logPath)
	if logPath == "" {
		specDir = os.TempDir()
	}
	specPath := filepath.Join(specDir, name+".json")
	spec, err := json.Marshal(spawnSpec{Task: task, Args: args, Dir: dir, Env: env, Log: logPath})
	if err != nil {
		return err
	}
	if err := os.WriteFile(specPath, spec, 0o600); err != nil {
		return err
	}
	sid, err := paths.CurrentUserSID()
	if err != nil {
		return err
	}
	xmlPath := filepath.Join(specDir, name+".xml")
	if err := os.WriteFile(xmlPath, utf16File(taskXML(sid, exe, `spawn-task "`+specPath+`"`, dir)), 0o600); err != nil {
		return err
	}
	defer os.Remove(xmlPath)
	if out, err := exec.Command("schtasks", "/Create", "/TN", task, "/XML", xmlPath, "/F").CombinedOutput(); err != nil {
		os.Remove(specPath)
		return fmt.Errorf("schtasks /Create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("schtasks", "/Run", "/TN", task).CombinedOutput(); err != nil {
		exec.Command("schtasks", "/Delete", "/TN", task, "/F").Run()
		os.Remove(specPath)
		return fmt.Errorf("schtasks /Run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RunSpawnTask is the `stagent spawn-task <spec>` entry point executed by a
// fallback task. It loads the spec, applies its environment, working
// directory and stderr log, and returns the stagent arguments to run
// in-process. cleanup deletes the task definition; call it when the process
// is about to exit.
func RunSpawnTask(specPath string) (args []string, cleanup func(), err error) {
	b, err := os.ReadFile(specPath)
	if err != nil {
		return nil, nil, err
	}
	os.Remove(specPath)
	var s spawnSpec
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, nil, err
	}
	os.Clearenv()
	for _, kv := range s.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			os.Setenv(k, v)
		}
	}
	if s.Dir != "" {
		os.Chdir(s.Dir)
	}
	if s.Log != "" {
		if f, err := os.OpenFile(s.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			os.Stderr = f
		}
	}
	cleanup = func() { exec.Command("schtasks", "/Delete", "/TN", s.Task, "/F").Run() }
	return s.Args, cleanup, nil
}

// taskXML defines a task that runs as the current user without a stored
// password (S4U), whether or not the user is logged on interactively, with
// no execution time limit.
func taskXML(sid, exe, arguments, dir string) string {
	esc := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch r {
			case '&':
				b.WriteString("&amp;")
			case '<':
				b.WriteString("&lt;")
			case '>':
				b.WriteString("&gt;")
			case '"':
				b.WriteString("&quot;")
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>stagent detached process</Description></RegistrationInfo>
  <Principals>
    <Principal id="Author">
      <UserId>` + esc(sid) + `</UserId>
      <LogonType>S4U</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>Parallel</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Hidden>true</Hidden>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>` + esc(exe) + `</Command>
      <Arguments>` + esc(arguments) + `</Arguments>
      <WorkingDirectory>` + esc(dir) + `</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`
}

// utf16File encodes s as UTF-16LE with a BOM, the encoding schtasks /XML
// reliably accepts.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// Alive reports whether pid is a running process.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
