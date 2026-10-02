//go:build !windows

package paths

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

// sessionIDLen is the length of wire.NewSessionID; the longest socket path is
// <RunDir>/s/<id>.sock.
const sessionIDLen = 16

// resolveIPC puts the sockets in <Root>/run, or in <TMPDIR>/stagent-<uid>
// when that would make a socket path too long — and always on Linux for the
// real installation: logind deletes $XDG_RUNTIME_DIR at the user's last
// logout (unless lingering), which would orphan every detached session.
func (l *Layout) resolveIPC(isolated bool) error {
	run := filepath.Join(l.Root, "run")
	if (!isolated && runtime.GOOS == "linux") || len(run)+len("/s/")+sessionIDLen+len(".sock") >= maxSunPath {
		name := "stagent-" + userTag()
		if isolated {
			name += "-" + homeTag(l.Home)
		}
		run = filepath.Join(os.TempDir(), name)
		l.sharedRunDir = true
	}
	l.RunDir = run
	l.DaemonAddr = filepath.Join(run, "stagent.sock")
	return nil
}

// ensurePrivateDir creates dir with mode 0700 and, like tmux, refuses to use
// it unless it is a directory (not a symlink) owned by us that nobody else
// can enter: in a world-writable temporary directory another user could have
// created it first to read or replace our sockets. Such a directory is not
// repaired: someone else's cannot be, and one of ours that was open to
// others may already hold their files.
func ensurePrivateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("refusing to use %s: a symlink or not a directory", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("refusing to use %s: not owned by uid %d", dir, os.Getuid())
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("refusing to use %s: accessible by other users (mode %04o); remove it or run chmod 700 on it", dir, perm)
	}
	return nil
}

func userTag() string { return strconv.Itoa(os.Getuid()) }

func localDataBase() string { return "/var/tmp" }
