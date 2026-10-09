package wire

// ---------------------------------------------------------------------------
// Copied files (コピーするファイル) and shared directories (共有ディレクトリ)
// of a task's worktree: what task.preview answers and the copy stage of
// `stagent task run` does.

// TaskPreviewParams is task.preview. WorktreeInclude, when present (an
// empty list included), replaces the patterns saved in config.json for
// repo: an unsaved trial. Absent (null) uses the saved ones.
type TaskPreviewParams struct {
	Repo            string   `json:"repo"`
	WorktreeInclude []string `json:"worktree_include"`
}

// TaskPreviewResult: what a new worktree of Repo (canonical) would get.
// Copies holds at most TaskPreviewRows rows (Truncated then), Skipped too;
// TotalBytes counts every copy. Setup is the source checkout's orca.yaml
// scripts.setup; OrcaError, one line, says that orca.yaml is unusable.
type TaskPreviewResult struct {
	Repo       string     `json:"repo"`
	Copies     []TaskCopy `json:"copies"`
	Links      []TaskLink `json:"links"`
	Skipped    []TaskSkip `json:"skipped"`
	TotalBytes int64      `json:"total_bytes"`
	Truncated  bool       `json:"truncated"`
	Setup      string     `json:"setup,omitempty"`
	OrcaError  string     `json:"orca_error,omitempty"`
}

// TaskPreviewRows caps TaskPreviewResult.Copies and Skipped.
const TaskPreviewRows = 500

// TaskCopy is a file or directory copied into the worktree. Path is
// relative to the checkout's root, with / separators.
type TaskCopy struct {
	Path   string `json:"path"`
	Source string `json:"source"` // TaskSource*
	Bytes  int64  `json:"bytes"`
}

// TaskLink is a shared directory: linked, not copied.
type TaskLink struct {
	Path string `json:"path"`
}

// TaskSkip is what is not copied, and why. Entry is the .worktreeinclude
// line or app pattern, or a path one of them selects.
type TaskSkip struct {
	Entry  string `json:"entry"`
	Source string `json:"source"` // TaskSource*
	Reason string `json:"reason"` // TaskSkip*
}

// Where a copy comes from.
const (
	TaskSourceInclude = "include" // the source checkout's .worktreeinclude
	TaskSourceApp     = "app"     // config.json repos.<repo>.worktree_include
)

// Why something is not copied.
const (
	TaskSkipTracked    = "tracked"     // git tracks it: the worktree has it
	TaskSkipNotIgnored = "not_ignored" // untracked but not gitignored
	TaskSkipMissing    = "missing"     // a .worktreeinclude path that does not exist
	TaskSkipNoMatch    = "no_match"    // an app pattern that selects nothing
	TaskSkipUnderLink  = "under_link"  // inside a shared directory, which is linked
	TaskSkipInvalid    = "invalid"     // a line or pattern stagent does not take
	TaskSkipLimit      = "limit"       // past 2 GiB or 50,000 entries
)
