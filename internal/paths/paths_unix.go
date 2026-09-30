//go:build !windows

package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

// sessionIDLen is the length of wire.NewSessionID; the longest socket path is
// <RunDir>/s/<id>.sock.
const sessionIDLen = 16

func (l *Layout) resolveIPC(isolated bool) error {
	run := filepath.Join(l.Root, "run")
	if !isolated && runtime.GOOS == "linux" {
		if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
			if st, err := os.Stat(x); err == nil && st.IsDir() {
				run = filepath.Join(x, "stagent")
			}
		}
	}
	if len(run)+len("/s/")+sessionIDLen+len(".sock") >= maxSunPath {
		name := "stagent-" + userTag()
		if isolated {
			name += "-" + homeTag(l.Home)
		}
		run = filepath.Join(os.TempDir(), name)
	}
	l.RunDir = run
	l.DaemonAddr = filepath.Join(run, "stagent.sock")
	return nil
}

func userTag() string { return strconv.Itoa(os.Getuid()) }

func localDataBase() string { return "/var/tmp" }
