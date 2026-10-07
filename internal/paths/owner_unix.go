//go:build !windows

package paths

import (
	"io/fs"
	"os/user"
	"strconv"
	"syscall"
)

// UIDOwner names the user with uid for an OwnerError: the user name when it
// resolves, else the decimal uid.
func UIDOwner(uid int) string {
	id := strconv.Itoa(uid)
	if u, err := user.LookupId(id); err == nil && u.Username != "" {
		return u.Username
	}
	return id
}

// foreignOwner reports whether st belongs to a user other than us, and who.
func foreignOwner(st fs.FileInfo) (owner string, foreign bool) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) == getuid() {
		return "", false
	}
	return UIDOwner(int(sys.Uid)), true
}
