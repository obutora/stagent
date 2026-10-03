package install

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
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
	cmd.WaitDelay = time.Second // do not hang on grandchildren holding the pipe
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
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
	docs string // cached Windows Documents folder

	m     *Manifest
	haveM bool
	mErr  error
	dirty bool
	notes []string
}

func newEnv() (*env, error) {
	l, err := paths.Resolve()
	if err != nil {
		return nil, err
	}
	e := &env{
		l:           l,
		goos:        runtime.GOOS,
		run:         execRunner{},
		daemon:      ipcDaemon{l.DaemonAddr},
		daemonAt:    func(addr string) daemonAPI { return ipcDaemon{addr} },
		spawnDaemon: func() error { return daemonclient.StartDaemon(l) },
		lookPath:    exec.LookPath,
		getenv:      os.Getenv,
		now:         time.Now,
		uid:         os.Getuid(),
		kill:        killProcess,
		alive:       proc.Alive,
		settle:      3 * time.Second,
		sid:         currentSID(),
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

func (e *env) note(format string, args ...any) {
	e.notes = append(e.notes, fmt.Sprintf(format, args...))
}
