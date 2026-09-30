//go:build linux || darwin

package ptable

import (
	"path/filepath"
	"strings"
	"sync"
)

// ttyNames caches TTYName: a terminal device's number and name belong
// together.
var ttyNames sync.Map

// TTYName returns the name below /dev of the terminal device dev ("pts/3",
// "ttys003"), "" when there is no such device.
func TTYName(dev uint64) string {
	if dev == 0 {
		return ""
	}
	if n, ok := ttyNames.Load(dev); ok {
		return n.(string)
	}
	n := findTTY(dev)
	if n != "" {
		ttyNames.Store(dev, n)
	}
	return n
}

func findTTY(dev uint64) string {
	if g := ttyGuess(dev); g != "" && rdev("/dev/"+g) == dev {
		return g
	}
	for _, pattern := range []string{"/dev/pts/*", "/dev/ttys*", "/dev/tty*"} {
		paths, _ := filepath.Glob(pattern)
		for _, p := range paths {
			if rdev(p) == dev {
				return strings.TrimPrefix(p, "/dev/")
			}
		}
	}
	return ""
}
