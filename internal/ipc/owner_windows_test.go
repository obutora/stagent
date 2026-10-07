package ipc

import (
	"errors"
	"net"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/obutora/stagent/internal/paths"
)

func listenTemp(t *testing.T) string {
	t.Helper()
	addr := `\\.\pipe\stagent-test-` + t.Name()
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	return addr
}

// listenDenying creates one instance of a pipe that we own but whose DACL
// grants only SYSTEM, as another user's pipe that refuses us but lets us
// read its owner: Dial is refused, while reading the owner by name (an
// owner always has READ_CONTROL) connects to the instance. winio cannot
// serve such a pipe (each instance Accept adds needs GENERIC_READ|WRITE on
// it); the first instance is not checked against its own DACL and takes a
// client before ConnectNamedPipe.
func listenDenying(t *testing.T) string {
	t.Helper()
	addr := `\\.\pipe\stagent-test-` + t.Name()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	name, err := windows.UTF16PtrFromString(addr)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 4096, 4096, 0, sa)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { windows.CloseHandle(h) })
	return addr
}

// fakePipeOwner makes every dialed pipe look owned by sid.
func fakePipeOwner(t *testing.T, sid string) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	orig := pipeOwner
	pipeOwner = func(net.Conn) (*windows.SID, error) { return s, nil }
	t.Cleanup(func() { pipeOwner = orig })
}

// fakeNamedPipeOwner makes every pipe that refuses Dial look owned by sid.
func fakeNamedPipeOwner(t *testing.T, sid string) {
	t.Helper()
	s, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	orig := namedPipeOwner
	namedPipeOwner = func(string) (*windows.SID, error) { return s, nil }
	t.Cleanup(func() { namedPipeOwner = orig })
}

func TestDialAcceptsOwnPipe(t *testing.T) {
	addr := listenTemp(t)
	c, err := Dial(addr, time.Second)
	if err != nil {
		t.Fatalf("Dial own pipe: %v", err)
	}
	c.Close()
}

// An administrator's elevated daemon owns its pipe as Administrators.
func TestDialAcceptsAdministratorsPipe(t *testing.T) {
	addr := listenTemp(t)
	fakePipeOwner(t, "S-1-5-32-544")
	c, err := Dial(addr, time.Second)
	if err != nil {
		t.Fatalf("Dial Administrators-owned pipe: %v", err)
	}
	c.Close()
}

func TestDialRefusesForeignPipe(t *testing.T) {
	addr := listenTemp(t)
	fakePipeOwner(t, "S-1-5-21-1-2-3-4242")
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected to a pipe another user owns")
	}
	var oe *paths.OwnerError
	if !errors.As(err, &oe) || !errors.Is(err, paths.ErrForeignOwner) {
		t.Fatalf("err = %v, want *paths.OwnerError", err)
	}
	if oe.Path != addr || oe.Owner != "S-1-5-21-1-2-3-4242" {
		t.Fatalf("OwnerError = %+v", oe)
	}
}

// Another user's pipe whose DACL refuses us is still reported with its
// owner, not as a bare access denied.
func TestDialReportsForeignOwnerOfDenyingPipe(t *testing.T) {
	addr := listenDenying(t)
	fakeNamedPipeOwner(t, "S-1-5-21-1-2-3-4242")
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected to a pipe whose DACL refuses us")
	}
	var oe *paths.OwnerError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %v, want *paths.OwnerError", err)
	}
	if oe.Path != addr || oe.Owner != "S-1-5-21-1-2-3-4242" {
		t.Fatalf("OwnerError = %+v", oe)
	}
}

// Our own pipe refusing us (an administrator's elevated daemon dialed
// unelevated) stays access denied: it is not another user's. The owner is
// read for real here, and must have been read.
func TestDialKeepsAccessDeniedForOwnDenyingPipe(t *testing.T) {
	addr := listenDenying(t)
	orig := namedPipeOwner
	var owner *windows.SID
	var ownerErr error
	namedPipeOwner = func(a string) (*windows.SID, error) {
		owner, ownerErr = orig(a)
		return owner, ownerErr
	}
	t.Cleanup(func() { namedPipeOwner = orig })
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected to a pipe whose DACL refuses us")
	}
	var oe *paths.OwnerError
	if errors.As(err, &oe) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("err = %v, want access denied", err)
	}
	if ownerErr != nil {
		t.Fatalf("reading the owner of our own pipe: %v", ownerErr)
	}
	if ok, err := ownedByUs(owner); err != nil || !ok {
		t.Fatalf("owner %v not ours (err %v)", owner, err)
	}
}

// When the owner cannot be read either, Dial keeps the access denied.
func TestDialKeepsAccessDeniedWhenOwnerUnreadable(t *testing.T) {
	addr := listenDenying(t)
	orig := namedPipeOwner
	namedPipeOwner = func(string) (*windows.SID, error) { return nil, windows.ERROR_ACCESS_DENIED }
	t.Cleanup(func() { namedPipeOwner = orig })
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected to a pipe whose DACL refuses us")
	}
	var oe *paths.OwnerError
	if errors.As(err, &oe) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("err = %v, want access denied", err)
	}
}

// A second daemon racing a running one still sees ErrInUse (and exits
// quietly).
func TestListenOnServedAddressIsErrInUse(t *testing.T) {
	addr := listenTemp(t)
	if _, err := Listen(addr); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Listen = %v, want ErrInUse", err)
	}
}
