package wire

import "time"

// ---------------------------------------------------------------------------
// Worktrees (worktree): every linked worktree git knows on the host, who
// ever made it (a task, Orca, `claude --worktree`, by hand), and the other
// folders left in the worktree folder (置き場のほかのフォルダ). Served by
// the bridge, each kind on a queue of its own (ADR 0006); see PROTOCOL.md
// "Worktrees".

// Worktree methods (capability CapWorktrees).
const (
	// worktree.list ({} → WorktreeListResult) changes nothing on the host
	// but config.json's worktree_repos (queue "worktree").
	MethodWorktreeList = "worktree.list"
	// worktree.sizes ({} → WorktreeSizesResult): disk usage (queue
	// "worktree.sizes").
	MethodWorktreeSizes = "worktree.sizes"
	// worktree.prs ({} → WorktreePRsResult): gh's pull requests of the
	// worktrees' branches (queue "worktree.prs").
	MethodWorktreePRs = "worktree.prs"
	// worktree.watch (WorktreeWatchParams → WorktreeWatchResult) adds or
	// removes a watch folder (見るフォルダ); refused to coding agents'
	// process trees (queue "worktree").
	MethodWorktreeWatch = "worktree.watch"
	// worktree.remove (WorktreeRemoveParams → WorktreeRemoveResult) removes
	// one worktree or folder (worktree を消す); refused to coding agents'
	// process trees (task.remove's queue).
	MethodWorktreeRemove = "worktree.remove"
)

// CapWorktrees: the bridge serves worktree.* (every OS, since 0.18.0).
const CapWorktrees = "worktrees"

// WorktreeListResult is the host's inventory (目録). Repos are in the
// order of their source checkout's path, Folders in path order.
type WorktreeListResult struct {
	// WorktreeFolder is config.json's worktree_folder (canonical); absent
	// when none is set.
	WorktreeFolder string           `json:"worktree_folder,omitempty"`
	WatchFolders   []string         `json:"watch_folders"`
	Repos          []WorktreeRepo   `json:"repos"`
	Folders        []WorktreeFolder `json:"folders"`
}

// WorktreeRepo is one repository: its source checkout (元のチェックアウト,
// the main worktree) and its linked worktrees in git's order (the source
// checkout itself is not one of them).
type WorktreeRepo struct {
	Path string `json:"path"` // canonical
	Name string `json:"name"` // base name of Path
	// DefaultBranch: absent when it cannot be told (task.DefaultBranch).
	DefaultBranch string `json:"default_branch,omitempty"`
	// GitHub: a remote's URL is on github.com; worktree.prs asks gh.
	GitHub    bool            `json:"github"`
	Worktrees []WorktreeEntry `json:"worktrees"`
}

// WorktreeEntry is one linked worktree.
type WorktreeEntry struct {
	Path   string `json:"path"`             // canonical
	Branch string `json:"branch,omitempty"` // absent: detached
	Head   string `json:"head,omitempty"`   // the commit checked out
	// Missing: its directory is gone (git's prunable). Listing never
	// prunes it.
	Missing    bool   `json:"missing,omitempty"`
	Locked     bool   `json:"locked,omitempty"`
	LockReason string `json:"lock_reason,omitempty"`
	// TaskID: the task (not in_place) whose worktree this is.
	TaskID string `json:"task_id,omitempty"`
	// Sessions: ids of the live sessions whose cwd is inside it, or of
	// TaskID's task.
	Sessions []string `json:"sessions,omitempty"`
	// Changes counts task.Changes' lines (0 when Missing); Unpushed is
	// task.Unpushed of Branch.
	Changes  int `json:"changes"`
	Unpushed int `json:"unpushed"`
	// Merged: Head is an ancestor of the default branch (origin's, else
	// the local one).
	Merged bool `json:"merged"`
}

// WorktreeFolder is a folder of the worktree folder that is no worktree
// (置き場のほかのフォルダ): <folder>/<x>, or <folder>/<repo name>/<y> with
// Repo that name.
type WorktreeFolder struct {
	Path string `json:"path"`
	Repo string `json:"repo"`
}

// WorktreeSizesResult measures the source checkouts, the worktrees whose
// directory is there and the folders. Total adds the worktrees' and the
// folders' bytes, not the source checkouts'.
type WorktreeSizesResult struct {
	Entries []WorktreeSize `json:"entries"`
	Total   int64          `json:"total"`
}

// WorktreeSize is the disk usage of one of them, a worktree or source
// checkout inside it excluded, and its newest modification time (UTC,
// whole seconds; absent when nothing could be read).
type WorktreeSize struct {
	Path       string    `json:"path"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modified_at,omitzero"`
}

// WorktreePRsResult lists the pull requests gh found for the worktrees'
// branches of the repositories with a GitHub remote.
type WorktreePRsResult struct {
	PRs []WorktreePR `json:"prs"`
}

// WorktreePR is the pull request of a worktree's branch. Merged: State
// MERGED with the pull request's head the worktree's own commit (the rule
// task.remove deletes a branch by).
type WorktreePR struct {
	Path   string `json:"path"`
	Number int    `json:"number"`
	State  string `json:"state"` // MERGED | OPEN | CLOSED
	URL    string `json:"url"`
	Merged bool   `json:"merged"`
}

// WorktreeWatchParams adds a watch folder (Add: ~ expanded, an existing
// directory) or removes one (Remove).
type WorktreeWatchParams struct {
	Add    string `json:"add,omitempty"`
	Remove string `json:"remove,omitempty"`
}

// WorktreeWatchResult is config.json's worktree_watch afterwards.
type WorktreeWatchResult struct {
	WatchFolders []string `json:"watch_folders"`
}

// WorktreeRemoveParams is worktree.remove. DryRun only reports; otherwise
// a worktree's uncommitted changes need Force (not_clean) and its live
// sessions StopSessions (busy).
type WorktreeRemoveParams struct {
	Path         string `json:"path"`
	Force        bool   `json:"force,omitempty"`
	StopSessions bool   `json:"stop_sessions,omitempty"`
	DryRun       bool   `json:"dry_run,omitempty"`
}

// WorktreeRemoveResult: a worktree's dry run reports Changes (at most
// TaskRemoveChangesMax, then ChangesTruncated), Unpushed and Sessions, as
// task.remove's; a folder's, Bytes and its largest Entries. A removal
// reports KeptBranch, and RemovedParent when <worktree folder>/<repo
// name> was left empty and went too.
type WorktreeRemoveResult struct {
	Changes          []string            `json:"changes,omitempty"`
	ChangesTruncated bool                `json:"changes_truncated,omitempty"`
	Unpushed         int                 `json:"unpushed,omitempty"`
	Sessions         []string            `json:"sessions,omitempty"`
	KeptBranch       string              `json:"kept_branch,omitempty"`
	Bytes            int64               `json:"bytes,omitempty"`
	Entries          []WorktreeFolderTop `json:"entries,omitempty"`
	RemovedParent    string              `json:"removed_parent,omitempty"`
}

// WorktreeFolderTop is one entry directly in a folder, by disk usage.
type WorktreeFolderTop struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	Dir   bool   `json:"dir"`
}

// WorktreeRemoveTop caps WorktreeRemoveResult.Entries.
const WorktreeRemoveTop = 5
