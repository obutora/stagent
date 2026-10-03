//go:build !windows && !darwin

package main

import (
	"fmt"
	"os"
)

// spawnTask exists only on Windows (Task Scheduler fallback) and macOS
// (holders started in the GUI login session).
func spawnTask([]string) int {
	fmt.Fprintln(os.Stderr, "stagent: spawn-task is for Windows and macOS only")
	return 2
}
