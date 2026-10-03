//go:build !darwin

package proc

// SpawnHolder starts a detached holder: SpawnDetached everywhere but macOS.
func SpawnHolder(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	return SpawnDetached(exe, args, dir, env, logPath)
}
