package ptable

import (
	"path/filepath"
	"strings"
)

// BaseName normalizes a process name for comparison: lower case, without
// directory and Windows executable suffix.
func BaseName(name string) string {
	name = strings.ToLower(filepath.Base(strings.ReplaceAll(name, `\`, "/")))
	return strings.TrimSuffix(name, ".exe")
}

// Argv0 is the BaseName of a command line's program, "" for an empty one.
func Argv0(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	return BaseName(argv[0])
}
