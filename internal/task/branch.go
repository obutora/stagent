package task

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// slugMax is the longest slug of an issue's default branch.
const slugMax = 40

// IssueBranch is an issue's default branch: <number>-<slug>, the slug being
// its title lower-cased, every run of characters other than a-z and 0-9
// one hyphen, trimmed of hyphens, at most 40 characters. When nothing of
// the title is left the branch is issue-<number>.
func IssueBranch(number int, title string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if hyphen && b.Len() > 0 {
				b.WriteByte('-')
			}
			hyphen = false
			b.WriteRune(r)
		} else {
			hyphen = true
		}
	}
	slug := b.String()
	if len(slug) > slugMax {
		slug = strings.TrimRight(slug[:slugMax], "-")
	}
	if slug == "" {
		return "issue-" + strconv.Itoa(number)
	}
	return strconv.Itoa(number) + "-" + slug
}

// DefaultBranch is the repository's default branch as the checkout repo
// knows it: origin/HEAD (set by clone, or `git remote set-head origin
// --auto`), else origin's main or master.
func DefaultBranch(ctx context.Context, env []string, repo string) (string, error) {
	if out, err := Git(ctx, env, repo, "symbolic-ref", "--short", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if _, name, ok := strings.Cut(strings.TrimSpace(string(out)), "/"); ok && name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if HasRef(ctx, env, repo, "refs/remotes/origin/"+name) {
			return name, nil
		}
	}
	return "", errors.New("cannot tell the default branch: origin/HEAD is not set (git remote set-head origin --auto)")
}

// CurrentBranch is the branch checked out in dir, "" when detached.
func CurrentBranch(ctx context.Context, env []string, dir string) (string, error) {
	out, err := Git(ctx, env, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		var ge *GitError
		if errors.As(err, &ge) && ge.Stderr == "" {
			return "", nil // detached: symbolic-ref -q fails silently
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// HasRef reports whether ref names a commit in the checkout repo.
func HasRef(ctx context.Context, env []string, repo, ref string) bool {
	_, err := Git(ctx, env, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// CheckBranch fails when name cannot be a branch name.
func CheckBranch(ctx context.Context, env []string, repo, name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name %q", name)
	}
	if _, err := Git(ctx, env, repo, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("invalid branch name %q", name)
	}
	return nil
}

// PullRequestRefs asks gh, in the checkout repo, for a pull request's head
// branch (a task's branch, what `gh pr checkout` checks out) and base
// branch.
func PullRequestRefs(ctx context.Context, env []string, repo string, number int) (head, base string, err error) {
	out, err := GH(ctx, env, repo, "pr", "view", strconv.Itoa(number), "--json", "headRefName,baseRefName")
	if err != nil {
		return "", "", err
	}
	var pr struct {
		Head string `json:"headRefName"`
		Base string `json:"baseRefName"`
	}
	if err := json.Unmarshal(out, &pr); err != nil || pr.Head == "" {
		return "", "", fmt.Errorf("gh pr view %d: unexpected answer", number)
	}
	return pr.Head, pr.Base, nil
}

// GH runs gh in dir with env (nil: this process's), found on env's PATH,
// and returns its standard output. A failing gh is an error with its last
// stderr line.
func GH(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
	bin, err := lookProgram(env, "gh")
	if err != nil {
		return nil, errors.New("gh not found")
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %s", strings.Join(args[:min(2, len(args))], " "), cmp.Or(lastLine(stderr.String()), err.Error()))
	}
	return out, nil
}

// lastLine is the last non-blank line of s.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
