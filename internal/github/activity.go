package github

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"

	"github.com/obutora/stagent/internal/wire"
)

// listLimit is how many open issues and pull requests are listed (the
// app's gitHubListLimit).
const listLimit = "30"

const (
	pullFields   = "number,title,author,headRefName,isDraft,updatedAt,url,reviewDecision,labels"
	issueFields  = "number,title,author,updatedAt,url,labels"
	currentField = "number,title,url,state,isDraft,reviewDecision,statusCheckRollup,author,updatedAt,headRefName"
)

// gh is gh at path, run without prompts or update notices.
type gh struct {
	r    Runner
	path string
	dir  string
}

// lookGH finds gh on r's PATH; ok is false when it is not installed.
func lookGH(r Runner, dir string) (gh, bool) {
	path := r.Look("gh")
	if path == "" {
		return gh{}, false
	}
	r.Env = append(append([]string(nil), r.Env...), "GH_NO_UPDATE_NOTIFIER=1", "GH_PROMPT_DISABLED=1")
	return gh{r: r, path: path, dir: dir}, true
}

func (g gh) run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	return g.r.run(ctx, g.dir, g.path, args...)
}

// Activity asks gh for the repository's description, its open pull
// requests and issues, and the pull request of p.Branch, the gh calls in
// parallel, as the app's sh script does. gh runs in p.Path when that is a
// directory.
func Activity(ctx context.Context, r Runner, p wire.GitHubActivityParams) (wire.GitHubActivityResult, error) {
	if !validRepo(p.Repo) || !validHost(p.Host) || strings.HasPrefix(p.Branch, "-") {
		return wire.GitHubActivityResult{}, wire.Errorf(wire.ErrBadRequest, "github.activity: invalid host %q, repo %q or branch %q", p.Host, p.Repo, p.Branch)
	}
	dir := ""
	if st, err := os.Stat(p.Path); err == nil && st.IsDir() {
		dir = p.Path
	}
	g, ok := lookGH(r, dir)
	if !ok {
		res := wire.GitHubActivityResult{GH: wire.GHMissing}
		if r.Look("winget") != "" {
			res.PackageManager = "winget"
		}
		return res, nil
	}
	if out, errOut, err := g.run(ctx, "auth", "status", "--hostname", p.Host); err != nil {
		return wire.GitHubActivityResult{GH: wire.GHUnauthenticated, AuthMessage: squash(string(out) + " " + string(errOut))}, nil
	}

	res := wire.GitHubActivityResult{GH: wire.GHReady}
	var wg sync.WaitGroup
	wg.Go(func() {
		out, _, err := g.run(ctx, "repo", "view", p.Repo, "--json", "description,isPrivate")
		var v struct {
			Description string `json:"description"`
			IsPrivate   bool   `json:"isPrivate"`
		}
		if err == nil && json.Unmarshal(out, &v) == nil {
			res.Description = strings.TrimSpace(v.Description)
			res.IsPrivate = v.IsPrivate
		}
	})
	wg.Go(func() {
		res.Pulls, res.PullsError = list(g.run(ctx, "pr", "list", "-R", p.Repo, "--state", "open", "--limit", listLimit, "--json", pullFields))
	})
	wg.Go(func() {
		// Issue dependencies need a recent gh and a host that has them
		// (older GitHub Enterprise does not); without them the list loads
		// as before.
		out, errOut, err := g.run(ctx, "issue", "list", "-R", p.Repo, "--state", "open", "--limit", listLimit, "--json", issueFields+",blockedBy,blocking")
		if err != nil {
			out, errOut, err = g.run(ctx, "issue", "list", "-R", p.Repo, "--state", "open", "--limit", listLimit, "--json", issueFields)
		}
		res.Issues, res.IssuesError = list(out, errOut, err)
	})
	if p.Branch != "" {
		wg.Go(func() {
			// Failing is the normal "no pull request for this branch".
			out, _, err := g.run(ctx, "pr", "view", p.Branch, "-R", p.Repo, "--json", currentField)
			if err == nil && isJSON(out, '{') {
				res.Current = json.RawMessage(out)
			}
		})
	}
	wg.Wait()
	return res, nil
}

// list returns gh's JSON array, or what to show instead of it.
func list(out, errOut []byte, err error) (json.RawMessage, string) {
	switch {
	case err != nil:
		if msg := squash(string(errOut)); msg != "" {
			return nil, msg
		}
		return nil, err.Error()
	case isJSON(out, '['):
		return json.RawMessage(out), ""
	case squash(string(out)) != "":
		return nil, squash(string(out))
	default:
		return nil, "No response from gh"
	}
}

// isJSON reports whether out is valid JSON starting with open ('[' or '{').
func isJSON(out []byte, open byte) bool {
	s := strings.TrimSpace(string(out))
	return s != "" && s[0] == open && json.Valid([]byte(s))
}

// squash folds whitespace runs (newlines included) into single spaces.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

// validRepo accepts gh's -R value: OWNER/REPO or HOST/OWNER/REPO.
func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if !validName(p) {
			return false
		}
	}
	return true
}

// validHost accepts a host name (with a port, for GitHub Enterprise).
func validHost(host string) bool { return validName(host) }

// validName accepts what an owner, a repository or a host is made of, and
// nothing a program would read as an option.
func validName(s string) bool {
	if s == "" || s[0] == '-' {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-:", c)) {
			return false
		}
	}
	return true
}
