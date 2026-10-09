package github

import (
	"cmp"
	"context"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/obutora/stagent/internal/wire"
)

// Candidate is a directory the user worked in and when, in unix ms (0 when
// unknown).
type Candidate struct {
	Dir string
	At  int64
}

const (
	// maxCandidates directories are resolved to a working tree, newest
	// first (the sh script's `head -n 200`).
	maxCandidates = 200
	// MaxRepos working trees are inspected and listed.
	MaxRepos = 20
	// gitWorkers bounds the git processes run at once.
	gitWorkers = 8
)

// Repos lists the working trees of cands, newest first: each directory
// counts with its newest time, directories outside a working tree and
// working trees without a remote are dropped, and at most MaxRepos are
// inspected. It mirrors the app's sh script (`@@REPO`).
func Repos(ctx context.Context, r Runner, cands []Candidate) wire.GitHubReposResult {
	git := r.Look("git")
	if git == "" {
		return wire.GitHubReposResult{Repos: []wire.GitHubRepo{}, GitMissing: true}
	}
	cands = newest(cands)
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}
	tops := make([]string, len(cands))
	parallel(len(cands), gitWorkers, func(i int) {
		if top, ok := runGit(ctx, r, git, "-C", cands[i].Dir, "rev-parse", "--show-toplevel"); ok && top != "" {
			tops[i] = filepath.FromSlash(top)
		}
	})
	var trees []Candidate
	seen := map[string]bool{}
	for i, top := range tops {
		if len(trees) == MaxRepos {
			break
		}
		if top == "" || seen[pathKey(top)] {
			continue
		}
		seen[pathKey(top)] = true
		trees = append(trees, Candidate{Dir: top, At: cands[i].At})
	}
	found := make([]*wire.GitHubRepo, len(trees))
	parallel(len(trees), gitWorkers, func(i int) {
		found[i] = inspect(ctx, r, git, trees[i])
	})
	repos := []wire.GitHubRepo{}
	for _, repo := range found {
		if repo != nil {
			repos = append(repos, *repo)
		}
	}
	return wire.GitHubReposResult{Repos: repos}
}

// newest keeps one candidate per directory with its newest time, newest
// first; ties keep their order.
func newest(cands []Candidate) []Candidate {
	var out []Candidate
	at := map[string]int{}
	for _, c := range cands {
		if c.Dir == "" || !filepath.IsAbs(c.Dir) {
			continue
		}
		dir := filepath.Clean(c.Dir)
		if i, ok := at[pathKey(dir)]; ok {
			out[i].At = max(out[i].At, c.At)
			continue
		}
		at[pathKey(dir)] = len(out)
		out = append(out, Candidate{Dir: dir, At: c.At})
	}
	slices.SortStableFunc(out, func(a, b Candidate) int { return cmp.Compare(b.At, a.At) })
	return out
}

// pathKey compares paths as the file system does: case-insensitively on
// Windows.
func pathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// inspect reads one working tree; nil when it has no remote.
func inspect(ctx context.Context, r Runner, git string, tree Candidate) *wire.GitHubRepo {
	top := tree.Dir
	g := func(args ...string) (string, bool) {
		return runGit(ctx, r, git, append([]string{"-C", top}, args...)...)
	}
	remote, _ := g("remote", "get-url", "origin")
	if remote == "" {
		if names, ok := g("remote"); ok && names != "" {
			first, _, _ := strings.Cut(names, "\n")
			remote, _ = g("remote", "get-url", strings.TrimSpace(first))
		}
	}
	if remote == "" {
		return nil
	}
	repo := &wire.GitHubRepo{Path: top, RemoteURL: remote, SSHHost: sshHost(ctx, r, remote)}
	if tree.At > 0 {
		repo.LastActiveAt = time.UnixMilli(tree.At).UTC().Format(time.RFC3339)
	}
	if b, ok := g("symbolic-ref", "--short", "-q", "HEAD"); ok {
		repo.Branch = b
		if ab, ok := g("rev-list", "--left-right", "--count", "@{upstream}...HEAD"); ok {
			if be, ah, ok := strings.Cut(ab, "\t"); ok {
				behind, err1 := strconv.Atoi(be)
				ahead, err2 := strconv.Atoi(ah)
				if err1 == nil && err2 == nil {
					repo.Behind, repo.Ahead = &behind, &ahead
				}
			}
		}
	} else {
		repo.Branch, _ = g("rev-parse", "--short", "HEAD")
		repo.Detached = true
	}
	if def, ok := g("symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); ok {
		if _, name, ok := strings.Cut(def, "/"); ok {
			repo.DefaultBranch = name
		}
	}
	// --no-optional-locks keeps status from taking the index lock under a
	// git command the user is running.
	if st, ok := runGit(ctx, r, git, "--no-optional-locks", "-C", top, "status", "--porcelain"); ok && st != "" {
		repo.Changes = strings.Count(st, "\n") + 1
	}
	return repo
}

// runGit runs git and returns its output without the trailing newline. Long
// paths are on for Windows (core.longpaths; ignored elsewhere).
func runGit(ctx context.Context, r Runner, git string, args ...string) (string, bool) {
	out, _, err := r.run(ctx, "", git, append([]string{"-c", "core.longpaths=true"}, args...)...)
	return strings.TrimRight(string(out), "\r\n"), err == nil
}

// sshHost returns the real host name of an ssh remote's host (which may be
// an ~/.ssh/config alias) as `ssh -G` resolves it, "" for other remotes.
func sshHost(ctx context.Context, r Runner, remote string) string {
	var h string
	switch {
	case strings.HasPrefix(remote, "http://"), strings.HasPrefix(remote, "https://"):
		return ""
	case strings.Contains(remote, "://"):
		_, h, _ = strings.Cut(remote, "://")
		if _, rest, ok := strings.Cut(h, "@"); ok {
			h = rest
		}
		if i := strings.IndexAny(h, ":/"); i >= 0 {
			h = h[:i]
		}
	case strings.Contains(remote, ":"):
		h, _, _ = strings.Cut(remote, ":")
		if _, rest, ok := strings.Cut(h, "@"); ok {
			h = rest
		}
	}
	ssh := r.Look("ssh")
	if h == "" || ssh == "" || strings.HasPrefix(h, "-") {
		return ""
	}
	out, _, err := r.run(ctx, "", ssh, "-G", h)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "hostname "); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}
