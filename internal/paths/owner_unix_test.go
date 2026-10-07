//go:build !windows

package paths

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// asOtherUser makes every file we own look like another user's (tests
// cannot chown without root) and returns that user's uid.
func asOtherUser(t *testing.T) int {
	t.Helper()
	uid := os.Getuid()
	getuid = func() int { return uid + 1 }
	t.Cleanup(func() { getuid = os.Getuid })
	return uid
}

func TestForeignSharedRunDirIsOwnerError(t *testing.T) {
	l := sharedLayout(t)
	if oe := l.ForeignOwned(); oe != nil {
		t.Fatalf("ForeignOwned before the run dir exists = %+v", oe)
	}
	if err := l.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if oe := l.ForeignOwned(); oe != nil {
		t.Fatalf("ForeignOwned for our own run dir = %+v", oe)
	}
	owner := asOtherUser(t)
	want := OwnerError{Path: l.RunDir, Owner: UIDOwner(owner)}
	if oe := l.ForeignOwned(); oe == nil || *oe != want {
		t.Fatalf("ForeignOwned = %+v, want %+v", oe, want)
	}
	err := l.EnsureDirs()
	var oe *OwnerError
	if !errors.As(err, &oe) || !errors.Is(err, ErrForeignOwner) || *oe != want {
		t.Fatalf("EnsureDirs = %v, want %+v", err, want)
	}
}

func TestForeignNetworkDataParentIsOwnerError(t *testing.T) {
	l := networkLayout(t)
	if err := os.MkdirAll(l.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	asOtherUser(t)
	// The run dir does not exist yet, so only the data parent is found.
	oe := l.ForeignOwned()
	if oe == nil || oe.Path != filepath.Dir(l.DataDir) {
		t.Fatalf("ForeignOwned = %+v, want the data parent", oe)
	}
}
