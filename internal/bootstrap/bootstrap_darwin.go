package bootstrap

import (
	"fmt"
	"os"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libSystem exports the bootstrap and Mach calls (re-exported from libxpc
// and libsystem_kernel).
const libSystem = "/usr/lib/libSystem.B.dylib"

// taskBootstrapPort is TASK_BOOTSTRAP_PORT (mach/task_special_ports.h):
// task_set_bootstrap_port(task, port) is a macro for
// task_set_special_port(task, TASK_BOOTSTRAP_PORT, port).
const taskBootstrapPort = 4

// Swap is the first thing `stagent run` and `stagent daemon` do on macOS:
// the three calls tmux makes — bootstrap_get_root, then
// bootstrap_look_up_per_user for this user, then task_set_bootstrap_port —
// so that everything the process starts later (the agent, its tools, hooks,
// the daemon) inherits the per-user bootstrap port. libSystem's
// bootstrap_port, which this process's own lookups use, is updated too.
// Failing is not an error: the process runs on with the port it had, and
// Result says why.
func Swap() Result {
	r := Result{Applies: true}
	if err := swap(); err != nil {
		r.Err = err.Error()
	} else {
		r.Swapped = true
	}
	return r
}

func swap() (err error) {
	// purego panics on a malformed registration; never take the start down.
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	lib, err := purego.Dlopen(libSystem, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return fmt.Errorf("dlopen %s: %w", libSystem, err)
	}
	var syms [6]uintptr
	for i, name := range []string{"mach_task_self_", "bootstrap_port", "bootstrap_get_root", "bootstrap_look_up_per_user", "task_set_special_port", "mach_error_string"} {
		if syms[i], err = purego.Dlsym(lib, name); err != nil {
			return fmt.Errorf("dlsym %s: %w", name, err)
		}
	}
	taskSelf, bootstrapPort := (*uint32)(cptr(syms[0])), (*uint32)(cptr(syms[1]))
	var (
		getRoot       func(bp uint32, root *uint32) int32
		lookUpPerUser func(bp uint32, service uintptr, uid uint32, sp *uint32) int32
		setSpecial    func(task uint32, which int32, port uint32) int32
		errorString   func(kr int32) string
	)
	purego.RegisterFunc(&getRoot, syms[2])
	purego.RegisterFunc(&lookUpPerUser, syms[3])
	purego.RegisterFunc(&setSpecial, syms[4])
	purego.RegisterFunc(&errorString, syms[5])
	fail := func(call string, kr int32) error {
		return fmt.Errorf("%s: %s (%#x)", call, errorString(kr), uint32(kr))
	}

	var root, user uint32
	if kr := getRoot(*bootstrapPort, &root); kr != 0 {
		return fail("bootstrap_get_root", kr)
	}
	if kr := lookUpPerUser(root, 0, uint32(os.Getuid()), &user); kr != 0 {
		return fail("bootstrap_look_up_per_user", kr)
	}
	if kr := setSpecial(*taskSelf, taskBootstrapPort, user); kr != 0 {
		return fail("task_set_bootstrap_port", kr)
	}
	*bootstrapPort = user
	return nil
}

// cptr turns the address of a C variable (Dlsym) into a pointer.
func cptr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}
