package wire

import "encoding/json"

// ---------------------------------------------------------------------------
// GitHub screen (ADR 0007): served by the bridge itself, only on native
// Windows, where the app cannot run its POSIX sh scripts. The bridge
// announces them with CapGitHub.

const (
	MethodGitHubRepos    = "github.repos"
	MethodGitHubActivity = "github.activity"
	MethodGitHubStatus   = "github.status"

	// CapGitHub (Windows only): the github.* methods.
	CapGitHub = "github"
)

// GitHubMethods are served by the bridge on hosts announcing CapGitHub.
var GitHubMethods = map[string]bool{
	MethodGitHubRepos: true, MethodGitHubActivity: true, MethodGitHubStatus: true,
}

// GitHubReposResult is github.repos: the git working trees the user worked
// in last, newest first.
type GitHubReposResult struct {
	Repos []GitHubRepo `json:"repos"`
	// GitMissing: git is not installed (not on the bridge's PATH).
	GitMissing bool `json:"git_missing"`
}

// GitHubRepo is one working tree, with the fields of the app's sh script
// (`@@REPO`).
type GitHubRepo struct {
	Path      string `json:"path"` // top-level directory
	RemoteURL string `json:"remote_url"`
	// SSHHost is the real host name of an ssh remote (`ssh -G`), which may
	// name an ~/.ssh/config alias.
	SSHHost string `json:"ssh_host,omitempty"`
	// Branch is the abbreviated commit when Detached.
	Branch        string `json:"branch"`
	Detached      bool   `json:"detached"`
	DefaultBranch string `json:"default_branch,omitempty"` // from origin/HEAD
	Changes       int    `json:"changes"`                  // changed and untracked files
	// Ahead and Behind count commits against the upstream branch; nil
	// without one.
	Ahead        *int   `json:"ahead,omitempty"`
	Behind       *int   `json:"behind,omitempty"`
	LastActiveAt string `json:"last_active_at,omitempty"` // RFC 3339
}

// GitHubActivityParams: Repo is gh's -R value (OWNER/REPO, HOST/OWNER/REPO
// on GitHub Enterprise), Host the host gh must be signed in to, Branch the
// working tree's feature branch whose pull request is wanted.
type GitHubActivityParams struct {
	Path   string `json:"path"`
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
}

// gh states of GitHubActivityResult.
const (
	GHReady           = "ready"
	GHMissing         = "missing"
	GHUnauthenticated = "unauthenticated"
)

// GitHubActivityResult is what gh reported for one repository. Pulls,
// Issues and Current are gh's JSON as is.
type GitHubActivityResult struct {
	GH             string          `json:"gh"`
	AuthMessage    string          `json:"auth_message,omitempty"`
	PackageManager string          `json:"package_manager,omitempty"`
	Description    string          `json:"description,omitempty"`
	IsPrivate      bool            `json:"is_private"`
	Pulls          json.RawMessage `json:"pulls,omitempty"`
	PullsError     string          `json:"pulls_error,omitempty"`
	Issues         json.RawMessage `json:"issues,omitempty"`
	IssuesError    string          `json:"issues_error,omitempty"`
	Current        json.RawMessage `json:"current,omitempty"`
}

// Kinds of github.status items.
const (
	GitHubKindIssue = "issue"
	GitHubKindPR    = "pr"
)

type GitHubStatusParams struct {
	Items []GitHubStatusQuery `json:"items"`
}

// GitHubStatusQuery names an issue or pull request; Repo is gh's -R value.
// Branch (issues only) asks for the state of that branch's pull request too.
type GitHubStatusQuery struct {
	Repo   string `json:"repo"`
	Kind   string `json:"kind"`
	Number int    `json:"number"`
	Branch string `json:"branch,omitempty"`
}

type GitHubStatusResult struct {
	Items []GitHubStatusItem `json:"items"`
}

// GitHubStatusItem: State and PRState as gh reports them (OPEN, CLOSED,
// MERGED). Branch is the query's branch of an issue item, PRState that
// branch's pull request.
type GitHubStatusItem struct {
	Repo    string `json:"repo"`
	Kind    string `json:"kind"`
	Number  int    `json:"number"`
	Branch  string `json:"branch,omitempty"`
	State   string `json:"state"`
	PRState string `json:"pr_state,omitempty"`
}
