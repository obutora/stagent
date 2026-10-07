//go:build linux || darwin

package ipc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPeerPID(t *testing.T) {
	dir, err := os.MkdirTemp("", "ipc") // short: socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	ln, err := Listen(filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := Dial(filepath.Join(dir, "s"), time.Second); err == nil {
			defer c.Close()
			c.Read(make([]byte, 1))
		}
	}()
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if pid, err := PeerPID(c); err != nil || pid != os.Getpid() {
		t.Fatalf("PeerPID = %d, %v; want %d", pid, err, os.Getpid())
	}
}
