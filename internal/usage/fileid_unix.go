//go:build !windows

package usage

import (
	"io/fs"
	"strconv"
	"syscall"
)

// inodesSupported: fileID tells a replaced file from an appended one.
const inodesSupported = true

// fileID names the file fi describes ("dev:ino"), so a transcript replaced
// by another file under the same name is read again from the start.
func fileID(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(uint64(st.Ino), 10)
}
