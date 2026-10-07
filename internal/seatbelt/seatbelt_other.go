//go:build !darwin

// Package seatbelt tells whether a process runs in a macOS sandbox
// (Seatbelt), as a coding agent's sandboxed commands do (ADR 0004).
package seatbelt

// Load is nil off macOS: Linux sandboxes (bubblewrap) keep their commands
// under the agent in a PID namespace, so walking parents finds it.
func Load() func(pid int) bool { return nil }
