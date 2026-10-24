//go:build !windows

package worktree

import (
	"io/fs"
	"syscall"
)

// fileID names a file across its hard links.
type fileID struct{ dev, ino uint64 }

// diskUsage is what fi (from Lstat) takes on disk, st_blocks*512, and
// whether it is a file with more hard links (counted once by id).
func diskUsage(fi fs.FileInfo) (int64, fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fi.Size(), fileID{}, false
	}
	return int64(st.Blocks) * 512, fileID{uint64(st.Dev), uint64(st.Ino)}, !fi.IsDir() && st.Nlink > 1
}
