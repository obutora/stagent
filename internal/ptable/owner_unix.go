//go:build unix

package ptable

import (
	"errors"
	"os"
	"strconv"
)

// errMixedUIDs marks a process whose real and effective uids differ.
var errMixedUIDs = errors.New("ptable: the process's real and effective uids differ")

// CurrentOwner returns the user this process runs as, in the form of
// Snapshot.Owner: its effective uid.
func CurrentOwner() (string, error) {
	return strconv.Itoa(os.Geteuid()), nil
}
