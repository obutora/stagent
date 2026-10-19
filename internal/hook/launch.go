package hook

import (
	"strings"

	"github.com/obutora/stagent/internal/ptable"
)

// Windows: the hook command runs under the harness, possibly through a
// shell of its own (Claude Code runs hooks with Git Bash; other hooks are
// PowerShell or cmd one-liners). The harness is the nearest ancestor that
// is not such a shell; a terminal's shell above it tells a launch from a
// terminal apart from one by an app that starts the harness itself (the
// Codex desktop app, or any other program).

// winShells are the shells looked past and looked for, by image name
// without ".exe", lower case.
var winShells = map[string]bool{"powershell": true, "pwsh": true, "cmd": true, "bash": true, "sh": true}

// maxAncestors bounds the walk up the process tree.
const maxAncestors = 32

func imageName(name string) string {
	return strings.TrimSuffix(strings.ToLower(name), ".exe")
}

// winAncestry finds the harness among the ancestors of self in s and
// whether a shell (PowerShell, pwsh, cmd, bash, sh) runs above it. harness
// is 0 when the parent cannot be read.
func winAncestry(s *ptable.Snapshot, self int) (harness int, terminal bool) {
	p := s.Get(self)
	seen := map[int]bool{self: true}
	for range maxAncestors {
		if p == nil || p.PPID <= 0 || seen[p.PPID] {
			return harness, false
		}
		seen[p.PPID] = true
		p = s.Get(p.PPID)
		if p == nil {
			return harness, false
		}
		if harness == 0 {
			if !winShells[imageName(p.Name)] {
				harness = p.PID
			}
			continue
		}
		if winShells[imageName(p.Name)] {
			return harness, true
		}
	}
	return harness, false
}
