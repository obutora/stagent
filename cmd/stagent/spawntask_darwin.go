package main

import (
	"fmt"
	"os"

	"github.com/obutora/stagent/internal/proc"
)

// spawnTask runs the launchd job of proc.SpawnHolder: start the holder of
// the spec, then remove the job (launchd stops this process there).
func spawnTask(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: stagent spawn-task <spec.json>")
		return 2
	}
	cleanup, err := proc.RunSpawnTask(args[0])
	code := 0
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagent spawn-task:", err)
		code = 1
	}
	cleanup()
	return code
}
