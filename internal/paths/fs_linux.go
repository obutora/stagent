package paths

import "golang.org/x/sys/unix"

// maxSunPath is sizeof(sockaddr_un.sun_path) on Linux.
const maxSunPath = 108

// Magic numbers from statfs(2) for network file systems.
var networkFSMagic = map[int64]bool{
	0x6969:     true, // NFS
	0xFF534D42: true, // CIFS
	0xFE534D42: true, // SMB2
	0x517B:     true, // SMB
	0x564C:     true, // NCP
}

func isNetworkFS(path string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return false
	}
	return networkFSMagic[int64(st.Type)]
}
