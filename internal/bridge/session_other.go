//go:build !windows

package bridge

import "os"

// watchSession: outside Windows the end of sshd closes stdin, so nothing
// else is watched.
func watchSession(*os.File) <-chan struct{} { return nil }
