package task

import (
	"io/fs"
	"os"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// isLink reports whether fi (from os.Lstat) is a symbolic link or a
// junction: any reparse point, which os.Remove removes as the link itself.
func isLink(fi fs.FileInfo) bool {
	if fi.Mode()&fs.ModeSymlink != 0 {
		return true
	}
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// LinkDir makes link point to the directory target (a shared directory of
// a task's worktree): a junction, which needs no privilege, else a
// directory symbolic link (Developer Mode or elevation), as Orca does.
func LinkDir(target, link string) error {
	return firstLink(target, link, junction, os.Symlink)
}

// junction makes link, a new directory, a mount point (junction) to the
// absolute directory target.
func junction(target, link string) error {
	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	if err := setMountPoint(target, link); err != nil {
		os.Remove(link)
		return &os.LinkError{Op: "junction", Old: target, New: link, Err: err}
	}
	return nil
}

func setMountPoint(target, link string) error {
	name, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	buf := winio.EncodeReparsePoint(&winio.ReparsePoint{Target: target, IsMountPoint: true})
	var n uint32
	return windows.DeviceIoControl(h, windows.FSCTL_SET_REPARSE_POINT, &buf[0], uint32(len(buf)), nil, 0, &n, nil)
}
