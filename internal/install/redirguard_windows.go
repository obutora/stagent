package install

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessMitigationPolicy = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessMitigationPolicy")

// processRedirectionTrustPolicy is PROCESS_MITIGATION_POLICY's
// ProcessRedirectionTrustPolicy (Windows 10 2004+).
const processRedirectionTrustPolicy = 16

// redirectionGuard reports whether this process enforces RedirectionGuard
// (PROCESS_MITIGATION_REDIRECTION_TRUST_POLICY.EnforceRedirectionTrust):
// it then does not follow junctions that a non-administrator created.
// sshd.exe gets it from its Image File Execution Options and passes it to
// every process started over SSH, the holders of kept shells included.
// nil when Windows cannot tell (older versions).
func redirectionGuard() *bool {
	if procGetProcessMitigationPolicy.Find() != nil {
		return nil
	}
	var flags uint32
	r, _, _ := procGetProcessMitigationPolicy.Call(uintptr(windows.CurrentProcess()), processRedirectionTrustPolicy,
		uintptr(unsafe.Pointer(&flags)), unsafe.Sizeof(flags))
	if r == 0 {
		return nil
	}
	on := flags&1 != 0
	return &on
}
