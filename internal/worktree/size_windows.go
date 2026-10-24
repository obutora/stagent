package worktree

import "io/fs"

// fileID names a file across its hard links; not told apart on Windows.
type fileID struct{}

// diskUsage is fi's size: Windows reports no allocated size cheaply, and
// hard links are rare there.
func diskUsage(fi fs.FileInfo) (int64, fileID, bool) {
	if fi.IsDir() {
		return 0, fileID{}, false
	}
	return fi.Size(), fileID{}, false
}
