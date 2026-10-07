// Package seatbelt tells whether a process runs in a macOS sandbox
// (Seatbelt), as a coding agent's sandboxed commands do (ADR 0004).
package seatbelt

import (
	"sync"

	"github.com/ebitengine/purego"
)

// libSystem re-exports libsystem_sandbox, which has sandbox_check.
const libSystem = "/usr/lib/libSystem.B.dylib"

// filterNone is SANDBOX_FILTER_NONE: with no operation, sandbox_check asks
// whether the process is sandboxed at all.
const filterNone = 0

// Load returns a function telling whether pid runs in a sandbox, or nil
// when that cannot be told. It calls sandbox_check(pid, NULL,
// SANDBOX_FILTER_NONE), which Apple does not document: 0 for a process
// outside any sandbox, 1 for one inside — and for a pid with no process —
// -1 on error. Only 0 reads as outside, so a process that is gone counts
// as sandboxed. Load is nil when the call is missing or does not answer 0
// for launchd, which is never sandboxed, so that a macOS that changes it
// does not make every process look sandboxed.
var Load = sync.OnceValue(load)

func load() (sandboxed func(pid int) bool) {
	// purego panics on a malformed registration; never take the start down.
	defer func() {
		if recover() != nil {
			sandboxed = nil
		}
	}()
	lib, err := purego.Dlopen(libSystem, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return nil
	}
	sym, err := purego.Dlsym(lib, "sandbox_check")
	if err != nil {
		return nil
	}
	// Variadic in C; SANDBOX_FILTER_NONE reads no variadic argument.
	var check func(pid int32, operation uintptr, filter int32) int32
	purego.RegisterFunc(&check, sym)
	if check(1, 0, filterNone) != 0 {
		return nil
	}
	return func(pid int) bool { return check(int32(pid), 0, filterNone) != 0 }
}
