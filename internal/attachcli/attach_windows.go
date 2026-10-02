package attachcli

import (
	"fmt"
	"os"
)

// Attach is the `stagent attach` entry point. It needs a Unix terminal
// (raw mode, SIGWINCH); Windows consoles are not supported.
func Attach(args []string) int {
	fmt.Fprintln(os.Stderr, "stagent attach is not supported on Windows")
	return exitUsage
}
