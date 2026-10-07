//go:build !windows

package ipc

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/obutora/stagent/internal/paths"
)

// listenTemp serves a socket in a fresh short directory (socket paths are
// limited to ~104 bytes, which t.TempDir can exceed on macOS).
func listenTemp(t *testing.T) (net.Listener, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	addr := filepath.Join(dir, "s.sock")
	ln, err := Listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, addr
}

// fakeServerUID makes every dialed server look like it runs as uid.
func fakeServerUID(t *testing.T, uid int) {
	t.Helper()
	orig := serverUID
	serverUID = func(net.Conn) (int, error) { return uid, nil }
	t.Cleanup(func() { serverUID = orig })
}

func TestDialAcceptsOwnServer(t *testing.T) {
	_, addr := listenTemp(t)
	c, err := Dial(addr, time.Second)
	if err != nil {
		t.Fatalf("Dial own socket: %v", err)
	}
	c.Close()
}

func TestDialRefusesForeignServer(t *testing.T) {
	_, addr := listenTemp(t)
	foreign := os.Getuid() + 4242
	fakeServerUID(t, foreign)
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected to a socket served by another user")
	}
	var oe *paths.OwnerError
	if !errors.As(err, &oe) || !errors.Is(err, paths.ErrForeignOwner) {
		t.Fatalf("err = %v, want *paths.OwnerError", err)
	}
	if oe.Path != addr || oe.Owner != paths.UIDOwner(foreign) {
		t.Fatalf("OwnerError = %+v, want path %s owner %s", oe, addr, paths.UIDOwner(foreign))
	}
}

func TestDialFailsWhenServerUnknown(t *testing.T) {
	_, addr := listenTemp(t)
	orig := serverUID
	serverUID = func(net.Conn) (int, error) { return -1, errors.New("no credentials") }
	t.Cleanup(func() { serverUID = orig })
	c, err := Dial(addr, time.Second)
	if err == nil {
		c.Close()
		t.Fatal("Dial connected without knowing who serves the socket")
	}
	if errors.Is(err, paths.ErrForeignOwner) {
		t.Fatalf("err = %v, want a plain error", err)
	}
}

// A second daemon racing a running one still sees ErrInUse (and exits
// quietly), whatever Dial makes of the address.
func TestListenOnServedAddressIsErrInUse(t *testing.T) {
	_, addr := listenTemp(t)
	if _, err := Listen(addr); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Listen = %v, want ErrInUse", err)
	}
	fakeServerUID(t, os.Getuid()+4242)
	if _, err := Listen(addr); !errors.Is(err, ErrInUse) {
		t.Fatalf("second Listen with a foreign-looking server = %v, want ErrInUse", err)
	}
}

func TestBlockedReportsForeignDaemonSocket(t *testing.T) {
	t.Setenv(paths.EnvHome, t.TempDir())
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if oe := Blocked(l); oe != nil {
		t.Fatalf("Blocked before anything exists = %+v", oe)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(l.DaemonAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if oe := Blocked(l); oe != nil {
		t.Fatalf("Blocked with our own daemon = %+v", oe)
	}
	fakeServerUID(t, os.Getuid()+4242)
	oe := Blocked(l)
	if oe == nil || oe.Path != l.DaemonAddr {
		t.Fatalf("Blocked with a foreign daemon = %+v, want %s", oe, l.DaemonAddr)
	}
}
