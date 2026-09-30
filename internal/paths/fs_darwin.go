package paths

import "golang.org/x/sys/unix"

// maxSunPath is sizeof(sockaddr_un.sun_path) on macOS.
const maxSunPath = 104

func isNetworkFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	name := unix.ByteSliceToString(st.Fstypename[:])
	switch name {
	case "nfs", "smbfs", "afpfs", "webdav", "macfuse", "osxfuse":
		return true
	}
	return false
}
