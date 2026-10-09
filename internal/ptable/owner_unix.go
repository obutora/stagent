//go:build unix

package ptable

import "errors"

// errMixedUIDs marks a process whose real and effective uids differ.
var errMixedUIDs = errors.New("ptable: the process's real and effective uids differ")
