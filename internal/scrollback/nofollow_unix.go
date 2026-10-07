//go:build !windows

package scrollback

import "syscall"

// oNoFollow makes opening a segment fail when its path is a symlink planted
// by someone else, instead of truncating the link's target.
const oNoFollow = syscall.O_NOFOLLOW
