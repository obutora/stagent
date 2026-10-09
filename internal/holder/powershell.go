package holder

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/obutora/stagent/internal/pscwd"
)

// powershellCommand returns argv with the working-directory hook
// (pscwd.Hook, run after the profiles) when argv is a bare Windows
// PowerShell or PowerShell 7 (a `shell: true` session of a host whose sshd
// runs PowerShell): `-NoExit -EncodedCommand <hook>`. A PowerShell given
// any argument, and every program on other systems, is left as it is. The
// profiles' shell wrapper carries the same hook for the terminals sshd
// starts; with it the prompt is wrapped once more here, which changes
// nothing.
func powershellCommand(goos string, argv []string) []string {
	if goos != "windows" || len(argv) != 1 {
		return argv
	}
	switch strings.ToLower(filepath.Base(strings.ReplaceAll(argv[0], `\`, "/"))) {
	case "powershell", "powershell.exe", "pwsh", "pwsh.exe":
	default:
		return argv
	}
	return []string{argv[0], "-NoExit", "-EncodedCommand", encodePowerShell(pscwd.Hook)}
}

// encodePowerShell is script as -EncodedCommand takes it: UTF-16LE in
// base64.
func encodePowerShell(script string) string {
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
