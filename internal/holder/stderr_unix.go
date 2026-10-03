//go:build !windows

package holder

import (
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// moveStderrToLog points a detached holder's stderr (fd 2) at its log
// file, LogDir/<id>.log (the file the bridge gives the holders it starts),
// when stderr is a pipe or socket — `stagent run --detached` run over an
// SSH exec channel. The reading end goes away with the connection, and a
// Go program that writes to fd 2 after that dies of SIGPIPE: the next log
// line (the daemon being replaced, say) would end the session without
// session_ended. Called once the program runs, so start-up errors still
// reach the caller.
func (h *Holder) moveStderrToLog() {
	st, err := os.Stderr.Stat()
	if err != nil || st.Mode()&(fs.ModeNamedPipe|fs.ModeSocket) == 0 {
		return
	}
	path := filepath.Join(h.layout.LogDir, h.id+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.logf("stagent run: log file: %v", err)
		return
	}
	defer f.Close()
	h.logf("stagent run: session %s is running; its log continues in %s", h.id, path)
	if err := unix.Dup2(int(f.Fd()), 2); err != nil {
		h.logf("stagent run: log file: %v", err)
	}
}
