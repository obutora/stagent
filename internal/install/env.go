package install

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/daemonclient"
	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/proc"
	"github.com/obutora/stagent/internal/rpc"
	"github.com/obutora/stagent/internal/wire"
)

// Runner executes system commands (systemctl, launchctl, schtasks, harness
// --version). Tests substitute a fake so nothing touches the real system.
type Runner interface {
	Run(timeout time.Duration, name string, args ...string) (output string, err error)
}

type execRunner struct{}

func (execRunner) Run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = commandEnv(os.Environ())
	cmd.WaitDelay = time.Second // do not hang on grandchildren holding the pipe
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commandEnv is env without PSModulePath. PowerShell 7 exports its module
// path to the programs it starts (stagent run by sshd when its default shell
// is pwsh), and Windows PowerShell started with that path cannot load its
// own modules: Get-ExecutionPolicy fails, and `doctor` read the policy as
// unset. Without the variable each PowerShell uses its own default path.
func commandEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); !strings.EqualFold(k, "PSModulePath") {
			out = append(out, kv)
		}
	}
	return out
}

// daemonAPI is the part of the daemon's local IPC the installer uses.
type daemonAPI interface {
	Status() (*wire.DaemonStatus, error)
	Shutdown() error
	Sessions() ([]wire.Session, error)
}

type ipcDaemon struct{ addr string }

func (d ipcDaemon) call(method string, result any) error {
	conn, err := ipc.Dial(d.addr, 500*time.Millisecond)
	if err != nil {
		return err
	}
	c := rpc.NewClient(conn, nil)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return c.Call(ctx, method, nil, result)
}

func (d ipcDaemon) Status() (*wire.DaemonStatus, error) {
	var st wire.DaemonStatus
	if err := d.call(wire.MethodDaemonStatus, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (d ipcDaemon) Shutdown() error { return d.call(wire.MethodDaemonShutdown, nil) }

func (d ipcDaemon) Sessions() ([]wire.Session, error) {
	var r wire.SessionsListResult
	if err := d.call(wire.MethodSessionsList, &r); err != nil {
		return nil, err
	}
	return r.Sessions, nil
}

// env bundles the host facts and side-effecting operations of one command
// run. Tests build it with a temporary layout and fakes.
type env struct {
	l      *paths.Layout
	goos   string
	run    Runner
	daemon daemonAPI
	// daemonAt reaches a daemon listening at another address (one started
	// by an earlier version with a different layout).
	daemonAt func(addr string) daemonAPI
	// spawnDaemon starts this binary's daemon detached (no login service).
	spawnDaemon func() error

	lookPath func(string) (string, error)
	getenv   func(string) string
	now      func() time.Time
	uid      int
	// kill terminates a process (fallback when daemon.shutdown fails).
	kill  func(pid int) error
	alive func(pid int) bool
	// settle is how long stop waits for the daemon to go away.
	settle time.Duration
	// sid is the user's SID on Windows (scheduled task principal).
	sid  string
	docs string              // cached Windows Documents folder
	ps   *[]PowerShellReport // cached powerShells
	// redirectionGuard reads whether this process runs with Windows'
	// RedirectionGuard (nil when unknown or off Windows).
	redirectionGuard func() *bool

	m     *Manifest
	haveM bool
	mErr  error
	dirty bool
	notes []Note
}

func newEnv() (*env, error) {
	l, err := paths.Resolve()
	if err != nil {
		return nil, err
	}
	e := &env{
		l:                l,
		goos:             runtime.GOOS,
		run:              execRunner{},
		daemon:           ipcDaemon{l.DaemonAddr},
		daemonAt:         func(addr string) daemonAPI { return ipcDaemon{addr} },
		spawnDaemon:      func() error { return daemonclient.StartDaemon(l) },
		lookPath:         exec.LookPath,
		getenv:           os.Getenv,
		now:              time.Now,
		uid:              os.Getuid(),
		kill:             killProcess,
		alive:            proc.Alive,
		settle:           3 * time.Second,
		sid:              currentSID(),
		redirectionGuard: redirectionGuard,
	}
	e.loadManifest()
	return e, nil
}

func (e *env) loadManifest() {
	m, ok, err := loadManifest(e.l.Manifest)
	e.mErr = err
	if !ok {
		m = newManifest(e.l, e.now().UnixMilli())
	}
	e.m, e.haveM = m, ok
}

func (e *env) saveManifest() error {
	if !e.dirty {
		return nil
	}
	if err := e.m.save(e.l.Manifest, e.now().UnixMilli()); err != nil {
		return err
	}
	e.haveM, e.dirty = true, false
	return nil
}

// Note is one remark of install / integrate (PROTOCOL.md): Text in English
// for people; Code, with Args, for a client that words it itself. A Code
// keeps its meaning; Text may be reworded.
type Note struct {
	Code string            `json:"code"`
	Text string            `json:"text"`
	Args map[string]string `json:"args,omitempty"`
}

// Codes of Note.Code (PROTOCOL.md).
const (
	noteClaudeRestart       = "claude_restart"
	noteClaudeWindowsHooks  = "claude_windows_hooks"
	noteOmpRestart          = "omp_restart"
	noteCodexTrust          = "codex_trust"
	noteCodexNotifyFallback = "codex_notify_fallback" // args: reason (codexReason*)
	noteCodexNotifyKept     = "codex_notify_kept"     // args: command
	noteCodexFeaturesKept   = "codex_features_kept"
	noteNoShell             = "no_shell"
	notePowerShellNoBOM     = "powershell_no_bom" // args: path, bin
	noteCmdNotWrapped       = "cmd_not_wrapped"   // args: shell
	noteBinaryMissing       = "binary_missing"    // args: path
	noteServiceLinger       = "service_linger"
	noteLaunchAgent         = "launch_agent"
	noteTerminalNoProfiles  = "terminal_no_profiles"
	noteTerminalRunning     = "terminal_running"
	noteNotApplied          = "not_applied"           // args: failed
	noteDaemonReplaceFailed = "daemon_replace_failed" // args: running, version, error
	noteLegacyDaemonStopped = "legacy_daemon_stopped" // args: addr
)

// note records a Note. kv are its Args as key, value pairs; text names
// each one as {key} where it shows the value.
func (e *env) note(code, text string, kv ...string) {
	n := Note{Code: code, Text: text}
	if len(kv) > 0 {
		n.Args = make(map[string]string, len(kv)/2)
		repl := make([]string, 0, len(kv))
		for i := 0; i+1 < len(kv); i += 2 {
			n.Args[kv[i]] = kv[i+1]
			repl = append(repl, "{"+kv[i]+"}", kv[i+1])
		}
		n.Text = strings.NewReplacer(repl...).Replace(text)
	}
	e.notes = append(e.notes, n)
}

// notesOut is e.notes for JSON: [] rather than null.
func (e *env) notesOut() []Note {
	if e.notes == nil {
		return []Note{}
	}
	return e.notes
}
