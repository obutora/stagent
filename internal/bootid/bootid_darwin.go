package bootid

import "golang.org/x/sys/unix"

func get() string {
	id, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return ""
	}
	return id
}
