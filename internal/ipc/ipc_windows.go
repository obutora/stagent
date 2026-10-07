package ipc

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/paths"
)

// Listen serves the named pipe addr. The pipe's DACL grants access to the
// current user's SID only (protected, so nothing is inherited).
func Listen(addr string) (net.Listener, error) {
	sid, err := paths.CurrentUserSID()
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + sid + ")",
		InputBufferSize:    64 << 10,
		OutputBufferSize:   64 << 10,
	})
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, ErrInUse
		}
		return nil, err
	}
	return ln, nil
}

// Dial connects to the named pipe addr and makes sure we own it: a pipe
// another user created first under our name is closed unused and reported
// as a *paths.OwnerError. A pipe owned by BUILTIN\Administrators is ours
// too: that is the owner of a daemon or holder an administrator started
// elevated (OpenSSH runs administrators' sessions elevated), and only an
// elevated administrator, who is beyond what this protects against, can
// give it that owner. The owner is not pinned in Listen's security
// descriptor ("O:"), so this check also passes for pipes of older versions.
// Reading the owner needs READ_CONTROL, which the GENERIC_READ DialPipe
// requests includes. A pipe whose DACL refuses us is reported as a
// *paths.OwnerError too when its owner can be read and is someone else
// (see deniedByForeignOwner); otherwise the access denied error stays.
func Dial(addr string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	c, err := winio.DialPipe(addr, &timeout)
	if err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if oe := deniedByForeignOwner(addr, deadline); oe != nil {
				return nil, oe
			}
		}
		return nil, err
	}
	owner, err := pipeOwner(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("ipc: %s: cannot tell who owns it: %w", addr, err)
	}
	ok, err := ownedByUs(owner)
	if err != nil {
		c.Close()
		return nil, err
	}
	if !ok {
		c.Close()
		return nil, &paths.OwnerError{Path: addr, Owner: paths.SIDOwner(owner)}
	}
	return c, nil
}

// deniedByForeignOwner explains a pipe whose DACL refused Dial: it reads
// the owner by name, which asks only for READ_CONTROL, and returns a
// *paths.OwnerError when another user owns the pipe. Opening the pipe by
// name takes a free instance, as DialPipe does, so a busy pipe is retried
// until deadline. It returns nil when the owner cannot be read (the DACL
// refuses READ_CONTROL as well, or no instance came free) or is ours: an
// administrator's elevated daemon can refuse the same user's unelevated
// processes, and that is not a foreign owner.
func deniedByForeignOwner(addr string, deadline time.Time) *paths.OwnerError {
	owner, err := namedPipeOwner(addr)
	for errors.Is(err, windows.ERROR_PIPE_BUSY) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		owner, err = namedPipeOwner(addr)
	}
	if err != nil {
		return nil
	}
	if ok, err := ownedByUs(owner); err != nil || ok {
		return nil
	}
	return &paths.OwnerError{Path: addr, Owner: paths.SIDOwner(owner)}
}

// pipeOwner returns the owner SID of a dialed pipe. Tests replace it: they
// cannot create a pipe as another user.
var pipeOwner = func(c net.Conn) (*windows.SID, error) {
	f, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return nil, errors.New("not a named pipe")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	return sdOwner(sd)
}

// namedPipeOwner returns the owner SID of the pipe addr without dialing
// it, for a pipe whose DACL refused Dial. Tests replace it, as pipeOwner.
var namedPipeOwner = func(addr string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(addr, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	return sdOwner(sd)
}

func sdOwner(sd *windows.SECURITY_DESCRIPTOR) (*windows.SID, error) {
	owner, _, err := sd.Owner()
	if err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, errors.New("no owner")
	}
	return owner.Copy()
}

// ownedByUs accepts our own SID and BUILTIN\Administrators (see Dial).
func ownedByUs(owner *windows.SID) (bool, error) {
	me, err := paths.CurrentUserSID()
	if err != nil {
		return false, err
	}
	if owner.String() == me {
		return true, nil
	}
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, err
	}
	return owner.Equals(admins), nil
}
