// Package bootid reads the identifier the kernel gives the current boot:
// it changes with every start of the host, so a value stored before and read
// again tells whether the host restarted in between. Linux: the random
// /proc/sys/kernel/random/boot_id; macOS: sysctl kern.bootsessionuuid.
// Elsewhere (and when it cannot be read) it is "".
package bootid

import "strings"

// Get returns the current boot's id, "" when unknown.
func Get() string { return strings.TrimSpace(get()) }
