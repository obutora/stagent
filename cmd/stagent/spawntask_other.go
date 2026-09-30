//go:build !windows

package main

import (
	"fmt"
	"os"
)

// spawnTask exists only on Windows (Task Scheduler fallback).
func spawnTask([]string) int {
	fmt.Fprintln(os.Stderr, "stagent: spawn-task is Windows-only")
	return 2
}
