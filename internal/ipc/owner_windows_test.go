package ipc

import (
	"errors"
	"net"
	"testing"
	"time"

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

// A second daemon racing a running one still sees ErrInUse (and exits
// quietly).
func TestListenOnServedAddressIsErrInUse(t *testing.T) {
	addr := listenTemp(t)
	if _, err := Listen(addr); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Listen = %v, want ErrInUse", err)
	}
}
