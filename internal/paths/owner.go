package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrForeignOwner matches every *OwnerError (errors.Is).
var ErrForeignOwner = errors.New("stagent location owned by another user")

// OwnerError reports that a stagent location belongs to another user: a run
// or data directory in a shared temporary directory, a socket, or a named
// pipe. Someone created it first, so nothing was read from or sent to it,
// and it stays unusable until that user or an administrator removes it.
type OwnerError struct {
	Path  string // e.g. /tmp/stagent-1000, \\.\pipe\stagent-<SID>
	Owner string // user name when known, else uid (unix) / SID (Windows)
}

func (e *OwnerError) Error() string {
	return fmt.Sprintf("refusing to use %s: owned by another user (%s); ask the host's administrator to remove it", e.Path, e.Owner)
}

func (e *OwnerError) Is(target error) bool { return target == ErrForeignOwner }

// ForeignOwned returns the first directory of l in a shared temporary
// directory (a shared RunDir, the parent of a relocated DataDir) that
// exists and belongs to another user, or nil. Unlike EnsureDirs it creates
// and changes nothing, so doctor and the bridge's hello can call it.
func (l *Layout) ForeignOwned() *OwnerError {
	var dirs []string
	if l.sharedRunDir {
		dirs = append(dirs, l.RunDir)
	}
	if l.DataOnNetworkFS {
		dirs = append(dirs, filepath.Dir(l.DataDir))
	}
	for _, d := range dirs {
		if st, err := os.Lstat(d); err == nil {
			if owner, foreign := foreignOwner(st); foreign {
				return &OwnerError{Path: d, Owner: owner}
			}
		}
	}
	return nil
}
