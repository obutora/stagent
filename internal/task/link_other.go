//go:build !windows

package task

import (
	"io/fs"
	"os"
)

// isLink reports whether fi (from os.Lstat) is a symbolic link.
func isLink(fi fs.FileInfo) bool {
	return fi.Mode()&fs.ModeSymlink != 0
}

// LinkDir makes link a symbolic link to the directory target (a shared
// directory of a task's worktree).
func LinkDir(target, link string) error {
	return firstLink(target, link, os.Symlink)
}
