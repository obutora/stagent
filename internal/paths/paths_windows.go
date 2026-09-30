package paths

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func (l *Layout) resolveIPC(isolated bool) error {
	l.RunDir = filepath.Join(l.Root, "run")
	sid, err := CurrentUserSID()
	if err != nil {
		return err
	}
	l.pipePrefix = `\\.\pipe\stagent-` + sid
	if isolated {
		l.pipePrefix += "-" + homeTag(l.Home)
	}
	l.DaemonAddr = l.pipePrefix
	return nil
}

// CurrentUserSID returns the string SID of the process token's user. Named
// pipes are named after it and their DACL grants access to it alone.
func CurrentUserSID() (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return tu.User.Sid.String(), nil
}

func userTag() string {
	sid, err := CurrentUserSID()
	if err != nil {
		return "user"
	}
	return sid
}

func localDataBase() string { return os.TempDir() }

// Network homes on Windows (roaming profiles on a share) keep %USERPROFILE%
// on the local disk, so no relocation is needed.
func isNetworkFS(string) bool { return false }
