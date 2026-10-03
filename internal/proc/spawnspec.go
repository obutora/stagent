//go:build windows || darwin

package proc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
)

// spawnSpec is what a process started through the OS scheduler (a Task
// Scheduler task on Windows, a launchd job on macOS) picks up from a file
// written next to the log; the scheduler's own command line stays short.
type spawnSpec struct {
	Task string   `json:"task,omitempty"` // Windows: the task to delete afterwards
	Args []string `json:"args"`
	Dir  string   `json:"dir"`
	Env  []string `json:"env"`
	Log  string   `json:"log"`
}

// newSpawnName returns a fresh "spawn-<12 hex>" name for a scheduler entry
// and its spec file.
func newSpawnName() (string, error) {
	var rb [6]byte
	if _, err := rand.Read(rb[:]); err != nil {
		return "", err
	}
	return "spawn-" + hex.EncodeToString(rb[:]), nil
}

func writeSpec(path string, s spawnSpec) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// readSpec loads a spec and deletes its file.
func readSpec(path string) (spawnSpec, error) {
	var s spawnSpec
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	os.Remove(path)
	return s, json.Unmarshal(b, &s)
}
