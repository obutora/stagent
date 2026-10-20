//go:build !darwin

package proc

// SpawnHolder starts a detached holder: SpawnDetached everywhere but macOS.
func SpawnHolder(exe string, args []string, dir string, env []string, logPath string) (int, error) {
	return SpawnDetached(exe, args, dir, env, logPath)
}

// StartHolder is SpawnHolder that also tells whether the holder is outside
// the GUI login session: never but on macOS.
func StartHolder(exe string, args []string, dir string, env []string, logPath string) (pid int, outsideGUI bool, err error) {
	pid, err = SpawnHolder(exe, args, dir, env, logPath)
	return pid, false, err
}
