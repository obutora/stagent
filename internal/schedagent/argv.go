// Package schedagent runs the agent of an agent schedule (定期実行): the
// command line the daemon builds for a run (Argv), and `stagent schedule
// agent`, the wrapper a run's holder starts it under. The wrapper runs the
// agent non-interactively with JSON output, shows that output on the
// session's terminal as text, and judges the run from it (Outcome).
package schedagent

import (
	"os"
	"path/filepath"

	"github.com/obutora/stagent/internal/wire"
)

// permissionFlags are the flags each permission gives each harness of
// wire.ScheduleHarnesses (#1083).
var permissionFlags = map[string]map[string][]string{
	wire.HarnessClaude: {
		wire.PermissionRead:  {"--permission-mode", "manual"},
		wire.PermissionWrite: {"--permission-mode", "acceptEdits"},
		wire.PermissionAll:   {"--permission-mode", "bypassPermissions"},
	},
	wire.HarnessCodex: {
		wire.PermissionRead:  {"--sandbox", "read-only"},
		wire.PermissionWrite: {"--sandbox", "workspace-write"},
		wire.PermissionAll:   {"--dangerously-bypass-approvals-and-sandbox"},
	},
	wire.HarnessOmp: {
		wire.PermissionRead:  {"--approval-mode=always-ask"},
		wire.PermissionWrite: {"--approval-mode=write"},
		wire.PermissionAll:   {"--approval-mode=yolo"},
	},
}

// Permission is s's permission, "" being read.
func Permission(s wire.Schedule) string {
	if s.Permission == "" {
		return wire.PermissionRead
	}
	return s.Permission
}

// ValidPermission reports whether p is a permission ("" is read).
func ValidPermission(p string) bool {
	_, ok := permissionFlags[wire.HarnessClaude][p]
	return ok || p == ""
}

// Argv is the command line of an agent schedule's run (s valid), as its
// directory is now: the harness's non-interactive mode with JSON output
// and s's permission, the prompt last after "--". Nothing in it asks for
// the conversation not to be kept. claude answers no permission prompt
// (refused instead); codex outside a Git work tree is told it may run
// there; omp started in home is kept there (it would move to a temporary
// directory).
func Argv(s wire.Schedule, home string) []string {
	perm := permissionFlags[s.Harness][Permission(s)]
	var argv []string
	switch s.Harness {
	case wire.HarnessClaude:
		argv = append([]string{"claude", "-p", "--output-format", "json", "--permission-prompts", "none"}, perm...)
	case wire.HarnessCodex:
		argv = append([]string{"codex", "exec", "--json"}, perm...)
		if !InGitWorkTree(s.Cwd) {
			argv = append(argv, "--skip-git-repo-check")
		}
	case wire.HarnessOmp:
		argv = append([]string{"omp", "-p", "--mode", "json"}, perm...)
		if home != "" && filepath.Clean(s.Cwd) == filepath.Clean(home) {
			argv = append(argv, "--allow-home")
		}
	}
	return append(argv, "--", s.Prompt)
}

// InGitWorkTree reports whether dir is in a Git work tree: it or a
// directory above it has .git (a directory, or a worktree's file).
func InGitWorkTree(dir string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		up := filepath.Dir(dir)
		if up == dir {
			return false
		}
		dir = up
	}
}
