package wire

import "time"

// ---------------------------------------------------------------------------
// Tasks (タスク): an Issue or PR being worked on in a worktree of a source
// checkout (元のチェックアウト), or in the checkout itself (in_place). The
// daemon keeps them in <root>/state/tasks.json and is the only writer.

// Task methods (see PROTOCOL.md).
const (
	// Served by the bridge (spawn queue): register, prepare and start.
	MethodTaskCreate = "task.create"
	// Served by the bridge (login-shell environment): what would be copied.
	MethodTaskPreview = "task.preview"
	// Served by the bridge (its own queue): 片付け.
	MethodTaskRemove = "task.remove"
	// Served by the daemon: every task, in the order they were created.
	MethodTaskList = "task.list" // → TaskListResult

	// Local IPC only (daemon), refused to coding agents' process trees
	// (ADR 0004).
	//
	// task.progress reports a stage of `stagent task run` / `stagent task
	// remove` (TaskProgressParams → {}).
	MethodTaskProgress = "task.progress"
	// task.register adds a task by its key (source checkout + number), or
	// answers the existing one (TaskRegisterParams → TaskRegisterResult).
	// The bridge's task.create calls it before preparing anything.
	MethodTaskRegister = "task.register"
	// task.forget deletes a task's record (TaskRemoved → {}): its sessions
	// lose their task_id. The last step of task.remove.
	MethodTaskForget = "task.forget"

	// Notifications (daemon → app), like unwrapped.updated.
	NotifyTaskUpdated = "task.updated" // Task: added or changed
	NotifyTaskRemoved = "task.removed" // TaskRemoved
)

// Task kinds.
const (
	TaskKindIssue = "issue"
	TaskKindPR    = "pr"
)

// Task states.
const (
	TaskPreparing = "preparing" // `stagent task run` is preparing it
	TaskReady     = "ready"     // its agent (or terminal) was started
	TaskFailed    = "failed"    // a stage failed, or the prepared session went away (interrupted)
	TaskMissing   = "missing"   // its worktree directory or git's registration of it is gone
	TaskRemoving  = "removing"  // task.remove is under way
)

// Task stages: fetch … agent while preparing, stop … branch while
// removing (worktree and branch name a step of both).
const (
	TaskStageFetch    = "fetch"
	TaskStageBranch   = "branch"
	TaskStageWorktree = "worktree"
	TaskStageCopy     = "copy"
	TaskStageSetup    = "setup"
	TaskStageAgent    = "agent"
	TaskStageStop     = "stop"
	TaskStageArchive  = "archive"
)

// TaskErrInterrupted is Task.Error of a task whose prepared session went
// away (or never registered) before it got ready.
const TaskErrInterrupted = "interrupted"

// Task is one task. Repo is the canonical path of the source checkout
// (task.Canonical); with Number it is the task's key.
type Task struct {
	ID     string `json:"id"`
	Repo   string `json:"repo"`
	Kind   string `json:"kind"` // TaskKind*
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	// Worktree is where the task is worked on; Repo itself when InPlace.
	Worktree string `json:"worktree"`
	// InPlace: the task uses the source checkout, no worktree of its own.
	InPlace   bool      `json:"in_place"`
	CreatedAt time.Time `json:"created_at"` // RFC 3339, whole seconds
	State     string    `json:"state"`      // Task* states
	Stage     string    `json:"stage,omitempty"`
	Error     string    `json:"error,omitempty"` // one line
	// SessionID is the session that prepares the task. Sessions are tied
	// to tasks by Session.TaskID, not by this.
	SessionID string `json:"session_id,omitempty"`
}

// TaskRef names a task.
type TaskRef struct {
	ID string `json:"id"`
}

type TaskListResult struct {
	Tasks []Task `json:"tasks"`
}

// TaskProgressParams reports a stage. While preparing, stage agent without
// an error makes the task ready; an error makes it failed. Stage agent
// with an error from the task's prepared session (SessionID) fails a
// ready task too: the agent did not start. Stage stop or archive starts
// removing it (stop again while removing: busy); an error while removing
// takes it back to where it was, keeping the error and the stage.
type TaskProgressParams struct {
	ID        string `json:"id"`
	Stage     string `json:"stage"`
	Error     string `json:"error,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// TaskRegisterParams adds a task (task.register). Repo and Worktree are
// canonical paths (task.Canonical, task.CanonicalPath); Worktree is
// ignored (Repo) when InPlace. SessionID is the session the caller starts
// to prepare the task unless the result says it existed.
type TaskRegisterParams struct {
	Repo      string `json:"repo"`
	Kind      string `json:"kind"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	Branch    string `json:"branch"`
	Base      string `json:"base"`
	Worktree  string `json:"worktree"`
	InPlace   bool   `json:"in_place,omitempty"`
	SessionID string `json:"session_id"`
}

// TaskRegisterResult: without Existed the caller starts the prepared
// session (TaskRegisterParams.SessionID) now — the task is new, or a
// failed or interrupted one is prepared again. With Existed nothing is to
// be started; Session is the live prepared session of a task still
// preparing, if any.
type TaskRegisterResult struct {
	Task    Task     `json:"task"`
	Existed bool     `json:"existed"`
	Session *Session `json:"session,omitempty"`
}

// TaskCreateParams is task.create: register the task (task.register) and
// start its prepared session, `stagent task run <id> -- <command…>`, in
// the task's place. Branch overrides an issue's default branch
// (<number>-<slug>); a PR's is its head branch. Base defaults to the
// repository's default branch. An empty Command opens a terminal (the
// login shell; cmd.exe on Windows). SwitchBranch and Setup apply to
// in_place tasks only: a worktree task always gets its branch and setup.
type TaskCreateParams struct {
	Repo         string            `json:"repo"`
	Kind         string            `json:"kind"`
	Number       int               `json:"number"`
	Title        string            `json:"title"`
	URL          string            `json:"url"`
	Base         string            `json:"base,omitempty"`
	Branch       string            `json:"branch,omitempty"`
	Command      []string          `json:"command,omitempty"`
	Cols         int               `json:"cols"`
	Rows         int               `json:"rows"`
	Env          map[string]string `json:"env,omitempty"`
	InPlace      bool              `json:"in_place,omitempty"`
	SwitchBranch bool              `json:"switch_branch,omitempty"`
	Setup        bool              `json:"setup,omitempty"`
}

// TaskCreateResult: Session is the prepared session started now (not
// Existed), or the live one of a task still preparing (Existed).
type TaskCreateResult struct {
	Task    Task     `json:"task"`
	Session *Session `json:"session,omitempty"`
	Existed bool     `json:"existed"`
}

// TaskRemoved is task.forget's params and the task.removed notification.
// KeptBranch is the task's branch that task.remove left: not merged, or
// gh could not tell (omitted: deleted, or none).
type TaskRemoved struct {
	ID         string `json:"id"`
	KeptBranch string `json:"kept_branch,omitempty"`
}

// TaskRemoveParams is task.remove (片付け). DryRun only reports what is in
// the way; otherwise uncommitted changes need Force (not_clean) and live
// sessions StopSessions (busy). SkipArchive leaves scripts.archive out.
type TaskRemoveParams struct {
	ID           string `json:"id"`
	Force        bool   `json:"force,omitempty"`
	StopSessions bool   `json:"stop_sessions,omitempty"`
	SkipArchive  bool   `json:"skip_archive,omitempty"`
	DryRun       bool   `json:"dry_run,omitempty"`
}

// TaskRemoveResult: a dry run reports Changes (the worktree's `git status
// --porcelain` lines: tracked changes and untracked files not ignored, at
// most TaskRemoveChangesMax, then ChangesTruncated), Unpushed (commits of
// the branch on no remote) and Sessions (ids of the task's live
// sessions). A removal reports KeptBranch.
type TaskRemoveResult struct {
	Changes          []string `json:"changes,omitempty"`
	ChangesTruncated bool     `json:"changes_truncated,omitempty"`
	Unpushed         int      `json:"unpushed,omitempty"`
	Sessions         []string `json:"sessions,omitempty"`
	KeptBranch       string   `json:"kept_branch,omitempty"`
}

// TaskRemoveChangesMax caps TaskRemoveResult.Changes.
const TaskRemoveChangesMax = 200
