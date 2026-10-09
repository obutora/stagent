package github

import (
	"github.com/obutora/stagent/internal/ptable"
)

// sessionProcs are the names of the processes an SSH connection's
// programs run under: Win32-OpenSSH's per-connection sshd (`sshd -z`, and
// sshd-session from OpenSSH 9.8).
var sessionProcs = map[string]bool{"sshd": true, "sshd-session": true}

// LiveDirs returns the working directories of owner's processes that run
// under an SSH session or under a holder (holders: their pids) — shells,
// coding agents and whatever runs in them — each directory once. The
// process self and everything under it (the bridge asking, and its git and
// gh) are left out. On Windows the directory is read from the process's
// PEB (ptable).
func LiveDirs(s *ptable.Snapshot, owner string, holders []int, self int) []string {
	isHolder := make(map[int]bool, len(holders))
	for _, pid := range holders {
		isHolder[pid] = true
	}
	var dirs []string
	seen := map[string]bool{}
	for _, pid := range s.PIDs() {
		if pid == self || isHolder[pid] || sessionProcs[ptable.BaseName(s.Get(pid).Name)] || !under(s, pid, isHolder, self) {
			continue
		}
		if owner == "" || s.Owner(pid) != owner {
			continue
		}
		dir := s.Cwd(pid)
		if dir == "" || seen[pathKey(dir)] {
			continue
		}
		seen[pathKey(dir)] = true
		dirs = append(dirs, dir)
	}
	return dirs
}

// under reports whether pid's nearest ancestor that is an SSH session, a
// holder or self is one of the first two.
func under(s *ptable.Snapshot, pid int, isHolder map[int]bool, self int) bool {
	seen := map[int]bool{pid: true}
	for p := s.Get(pid); p != nil && p.PPID != 0 && !seen[p.PPID]; p = s.Get(p.PPID) {
		parent := s.Get(p.PPID)
		if parent == nil {
			return false
		}
		seen[parent.PID] = true
		switch {
		case parent.PID == self:
			return false
		case isHolder[parent.PID], sessionProcs[ptable.BaseName(parent.Name)]:
			return true
		}
	}
	return false
}
