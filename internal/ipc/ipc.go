// Package ipc provides the local, same-user-only transport between stagent
// processes: Unix domain sockets on Linux/macOS and named pipes on Windows.
// Nothing here ever listens on TCP.
package ipc

import "errors"

// ErrInUse is returned by Listen when another live process already serves
// the address (e.g. a second daemon racing the first).
var ErrInUse = errors.New("ipc: address already served by a live process")
