package proc

import (
	"fmt"
	"os"
	"time"
)

// startHolder starts a holder with viaGUI (macOS: a launchd job in the GUI
// login session; pid 0). When that fails it starts it with direct: the
// holder is then outside the GUI login session, and its log (logPath)
// says why.
func startHolder(viaGUI func() error, direct func() (int, error), logPath string) (pid int, outsideGUI bool, err error) {
	gerr := viaGUI()
	if gerr == nil {
		return 0, false, nil
	}
	if logPath != "" {
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
			fmt.Fprintf(f, "%s stagent: started outside the GUI login session (the login keychain stays locked): %v\n", time.Now().Format(time.RFC3339), gerr)
			f.Close()
		}
	}
	pid, err = direct()
	return pid, err == nil, err
}
