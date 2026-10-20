package daemon

import (
	"os"
	"path/filepath"

	"github.com/obutora/stagent/internal/schedagent"
	"github.com/obutora/stagent/internal/task"
	"github.com/obutora/stagent/internal/wire"
)

// Agent schedules (kind agent): a run's holder starts `stagent schedule
// agent --result <file> -- <agent…>` (schedagent), which runs the agent
// non-interactively with the schedule's permission, shows its output and
// leaves how the run went in <file>. The run's end reads it from there.
// The run's session handles no approvals and raises no needs_approval,
// waiting_input or turn_complete notification: nobody is there to answer,
// and the agent refuses what it would have asked about.

// runCommand is what the holder of a run of s runs: runArgv, under the
// wrapper (exe, the stagent binary) for an agent schedule.
func (d *Daemon) runCommand(exe string, env []string, s wire.Schedule, sessionID string) []string {
	argv := runArgv(env, s)
	if s.Kind != wire.ScheduleKindAgent {
		return argv
	}
	return append([]string{exe, "schedule", "agent", "--result", d.agentOutcomePath(sessionID), "--"}, argv...)
}

// agentOutcomePath is where the wrapper of run session sessionID leaves
// its outcome.
func (d *Daemon) agentOutcomePath(sessionID string) string {
	return filepath.Join(d.layout.LogDir, sessionID+".outcome.json")
}

// agentRunEndedLocked records the end of an agent run's session from the
// outcome its wrapper left; without one, by the exit code alone
// (unjudged). A lost session has no exit code.
func (d *Daemon) agentRunEndedLocked(p *pendingRun, sessionID string, exitCode int, lost bool) {
	path := d.agentOutcomePath(sessionID)
	o, err := schedagent.ReadOutcome(path)
	os.Remove(path)
	if lost {
		d.endRunLocked(p.run.RunID, wire.RunFailed, nil, "session lost")
		return
	}
	if err != nil || o.Result == "" {
		o = schedagent.Outcome{Result: wire.RunSucceeded, Unjudged: true}
		if exitCode != 0 {
			o.Result = wire.RunFailed
		}
	}
	p.run.Denials, p.run.ConversationID, p.run.Summary, p.run.Unjudged = o.Denials, o.ConversationID, o.Summary, o.Unjudged
	p.run.AuthFailed = o.AuthFailed
	d.endRunLocked(p.run.RunID, o.Result, &exitCode, o.Error)
}

// cwdKindLocked tells what directory cwd is: in the worktree of a task
// (worktreeTaskLocked), in another Git work tree (a checkout the user
// works in), or outside Git.
func (d *Daemon) cwdKindLocked(cwd string) (kind, taskID string) {
	if id := d.worktreeTaskLocked(task.CanonicalPath(cwd)); id != "" {
		return wire.CwdKindTask, id
	}
	if schedagent.InGitWorkTree(cwd) {
		return wire.CwdKindCheckout, ""
	}
	return wire.CwdKindDirectory, ""
}

// previewPlaceLocked adds to res what s's directory is (when it is there)
// and the warnings of an agent schedule that apply to it.
func (d *Daemon) previewPlaceLocked(res *wire.SchedulePreview, s wire.Schedule) {
	if st, err := os.Stat(s.Cwd); err != nil || !st.IsDir() {
		return
	}
	res.CwdKind, res.TaskID = d.cwdKindLocked(s.Cwd)
	if s.Kind != wire.ScheduleKindAgent {
		return
	}
	if s.Harness == wire.HarnessCodex && res.CwdKind == wire.CwdKindDirectory {
		res.Warnings = append(res.Warnings, wire.ScheduleWarning{Code: wire.WarnCodexOutsideGit,
			Message: s.Cwd + " is not in a Git repository: codex runs there with --skip-git-repo-check"})
	}
	if res.CwdKind == wire.CwdKindCheckout && schedagent.Permission(s) != wire.PermissionRead {
		res.Warnings = append(res.Warnings, wire.ScheduleWarning{Code: wire.WarnWritesCheckout,
			Message: "the agent may change files in " + s.Cwd + ", a checkout you work in (not a task's worktree)"})
	}
}

// quietInRuns: the notifications a schedule run's session does not raise
// (nobody is there to answer them; its end is the run's, notified with
// reason schedule).
func quietInRuns(s *session, reason string) bool {
	if s == nil || s.s.ScheduleID == "" {
		return false
	}
	switch reason {
	case reasonNeedsApproval, reasonWaitingInput, reasonTurnComplete, reasonExited:
		return true
	}
	return false
}
