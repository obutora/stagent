package main

import (
	"fmt"
	"os"

	"github.com/obutora/stagent/internal/proc"
)

// spawnTask runs a process started through the Task Scheduler fallback of
// proc.SpawnDetached: load the spec, then run its stagent command in-process.
func spawnTask(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: stagent spawn-task <spec.json>")
		return 2
	}
	sub, cleanup, err := proc.RunSpawnTask(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent spawn-task:", err)
		return 1
	}
	defer cleanup()
	if len(sub) > 0 && sub[0] == "spawn-task" {
		return 2
	}
	return dispatch(sub)
}
