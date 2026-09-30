// Package daemonclient connects to the stagent daemon, starting it on demand.
package daemonclient

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/obutora/stagent/internal/ipc"
	"github.com/obutora/stagent/internal/paths"
	"github.com/obutora/stagent/internal/proc"
)

// EnvExe overrides the stagent executable used to auto-start the daemon or
// detached holders. Tests set it to a built binary (os.Executable would be
// the test binary).
const EnvExe = "STAGENT_EXE"

// Exe returns the stagent executable to launch helper processes with.
func Exe() (string, error) {
	if e := os.Getenv(EnvExe); e != "" {
		return e, nil
	}
	return os.Executable()
}

// Dial connects to a running daemon without starting one.
func Dial(l *paths.Layout, timeout time.Duration) (net.Conn, error) {
	return ipc.Dial(l.DaemonAddr, timeout)
}

// DialOrStart connects to the daemon, starting it detached if nothing
// answers, and waits up to wait for it to come up.
func DialOrStart(l *paths.Layout, wait time.Duration) (net.Conn, error) {
	if c, err := Dial(l, 300*time.Millisecond); err == nil {
		return c, nil
	}
	if err := StartDaemon(l); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		c, err := Dial(l, 300*time.Millisecond)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(errors.New("daemon did not come up"), err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StartDaemon launches `stagent daemon` detached. A second concurrent start
// is harmless: the loser fails to take the IPC address and exits.
func StartDaemon(l *paths.Layout) error {
	if err := l.EnsureDirs(); err != nil {
		return err
	}
	exe, err := Exe()
	if err != nil {
		return err
	}
	_, err = proc.SpawnDetached(exe, []string{"daemon"}, l.Home, nil, filepath.Join(l.LogDir, "daemon.log"))
	return err
}
